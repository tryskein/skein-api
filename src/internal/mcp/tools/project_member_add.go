package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// projectMemberAddResponse is the wire shape returned by
// skein_project_members(action="add").
//
// Owner-or-admin gated. Idempotent (PUT semantics) — re-running with the
// same role is a no-op (no event written, same MemberDTO returned).
// Either `userId` (UUID) or `username` may be passed; not both. Username
// resolution uses the canonical lower-case match so case differences in
// the input do not cause spurious user_not_found.
type projectMemberAddResponse struct {
	Member *projects.MemberDTO `json:"member"`
}

func handleProjectMemberAdd(pool *pgxpool.Pool, projectsStore *projects.Store, usersStore *users.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		callerID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		projectArg, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, projectArg, callerID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		// Permission gate — the project owner, the space owner or the
		// instance admin (US-6, sk-741). EnsureProjectOwner returns
		// ErrProjectNotOwner for plain members (we map that to forbidden)
		// and ErrProjectNotVisible for invisible projects (collapsed to
		// project_not_found, no leak).
		if _, oerr := projects.EnsureProjectOwner(ctx, pool, projectID, callerID); oerr != nil {
			if errors.Is(oerr, projects.ErrProjectNotVisible) {
				return projectErrorResult(&ProjectNotFoundError{Query: projectArg})
			}
			if errors.Is(oerr, projects.ErrProjectNotOwner) {
				return skmcp.ToolError(ErrCodeForbidden,
					"only the project owner, the space owner or the instance admin can add members", nil), nil
			}
			return skmcp.ToolError(ErrCodeInternalError, "permission check failed", nil), nil
		}

		// Resolve target — exactly one of userId/username.
		userIDArg := strings.TrimSpace(req.GetString("userId", ""))
		usernameArg := strings.TrimSpace(req.GetString("username", ""))
		if userIDArg == "" && usernameArg == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"one of userId or username is required", nil), nil
		}
		if userIDArg != "" && usernameArg != "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"userId and username are mutually exclusive", nil), nil
		}
		var targetID uuid.UUID
		if userIDArg != "" {
			id, err := uuid.Parse(userIDArg)
			if err != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"userId is not a valid UUID",
					map[string]any{"userId": userIDArg}), nil
			}
			targetID = id
		} else {
			id, err := usersStore.ResolveByUsername(ctx, usernameArg)
			if err != nil {
				if errors.Is(err, users.ErrUserNotFound) {
					return skmcp.ToolError(ErrCodeUserNotFound,
						"user not found",
						map[string]any{"username": usernameArg}), nil
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_project_members", "action", "add",
					"reason", "username resolve failed",
					"actorUserId", callerID.String(), "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "username lookup failed", nil), nil
			}
			targetID = id
		}

		newRole, err := req.RequireString("role")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "role is required", nil), nil
		}
		if newRole != "owner" && newRole != "member" {
			return skmcp.ToolError(ErrCodeValidationError,
				"role must be 'owner' or 'member'",
				map[string]any{"field": "role"}), nil
		}

		actor := ProjectsAgentActor(ctx, pool, callerID)
		dto, err := projectsStore.UpsertMember(ctx, projectID, targetID, newRole, callerID, actor)
		if err != nil {
			if errors.Is(err, projects.ErrUserNotFound) {
				return skmcp.ToolError(ErrCodeUserNotFound,
					"target user does not exist",
					map[string]any{"userId": targetID.String()}), nil
			}
			if errors.Is(err, projects.ErrExternalCannotBeOwner) {
				// sk-163 S3 — external user capped at 'member'.
				return skmcp.ToolError(ErrCodeValidationError,
					"external users cannot be project owners",
					map[string]any{"field": "role"}), nil
			}
			if errors.Is(err, spaces.ErrConcurrentChange) {
				return skmcp.ToolError(ErrCodeConcurrentChange,
					"a concurrent membership change won; retry", nil), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_members", "action", "add",
				"reason", "upsert member failed",
				"actorUserId", callerID.String(),
				"projectId", projectID.String(),
				"targetUserId", targetID.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to upsert member", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_members", "action", "add",
			"actorUserId", callerID.String(),
			"projectId", projectID.String(),
			"targetUserId", targetID.String(),
			"role", newRole,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(projectMemberAddResponse{Member: dto})
	}
}

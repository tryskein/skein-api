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

// projectMemberRemoveResponse is the wire shape returned by
// skein_project_members(action="remove").
//
// Permission: caller must be the project owner, the space owner or the
// instance admin, OR the same
// user as the target (self-leave per R3.1 Q-7). The last explicit owner
// may go: the project stays under the implicit owner of its space (INV-4).
type projectMemberRemoveResponse struct {
	OK      bool             `json:"ok"`
	Removed projectMemberRef `json:"removed"`
}

type projectMemberRef struct {
	UserID string `json:"userId"`
}

func handleProjectMemberRemove(pool *pgxpool.Pool, projectsStore *projects.Store, usersStore *users.Store) server.ToolHandlerFunc {
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

		// Resolve target.
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
					"tool", "skein_project_members", "action", "remove",
					"reason", "username resolve failed",
					"actorUserId", callerID.String(), "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "username lookup failed", nil), nil
			}
			targetID = id
		}

		// Permission gate: self-leave bypasses owner check; otherwise
		// admin-or-owner. Self-leave still requires visibility (so a
		// non-member trying to "self-leave" a hidden project gets
		// project_not_found like any other invisible-project request).
		selfLeave := targetID == callerID
		if selfLeave {
			if _, vErr := projects.EnsureProjectVisible(ctx, pool, projectID, callerID); vErr != nil {
				return projectErrorResult(&ProjectNotFoundError{Query: projectArg})
			}
		} else {
			if _, oerr := projects.EnsureProjectOwner(ctx, pool, projectID, callerID); oerr != nil {
				if errors.Is(oerr, projects.ErrProjectNotVisible) {
					return projectErrorResult(&ProjectNotFoundError{Query: projectArg})
				}
				if errors.Is(oerr, projects.ErrProjectNotOwner) {
					return skmcp.ToolError(ErrCodeForbidden,
						"only the project owner, the space owner or the instance admin can remove members", nil), nil
				}
				return skmcp.ToolError(ErrCodeInternalError, "permission check failed", nil), nil
			}
		}

		actor := ProjectsAgentActor(ctx, pool, callerID)
		err = projectsStore.RemoveMember(ctx, projectID, targetID, actor)
		if err != nil {
			switch {
			case errors.Is(err, projects.ErrMemberViaSpace):
				return skmcp.ToolError(ErrCodeMemberViaSpace,
					"the member has access to the project through the space; switch the project's visibility to members or remove them from the space",
					map[string]any{"userId": targetID.String()}), nil
			case errors.Is(err, spaces.ErrConcurrentChange):
				return skmcp.ToolError(ErrCodeConcurrentChange,
					"a concurrent membership change won; retry", nil), nil
			case errors.Is(err, projects.ErrNotFound):
				return skmcp.ToolError(ErrCodeMemberNotFound,
					"user is not a member of this project",
					map[string]any{"userId": targetID.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_members", "action", "remove",
				"reason", "remove member failed",
				"actorUserId", callerID.String(),
				"projectId", projectID.String(),
				"targetUserId", targetID.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to remove member", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_members", "action", "remove",
			"actorUserId", callerID.String(),
			"projectId", projectID.String(),
			"targetUserId", targetID.String(),
			"selfLeave", selfLeave,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(projectMemberRemoveResponse{
			OK:      true,
			Removed: projectMemberRef{UserID: targetID.String()},
		})
	}
}

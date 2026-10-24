package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// projectMemberListResponse is the wire shape returned by
// skein_project_members(action="list").
//
// Visible-to-caller — anyone who can see the project can read the member
// roster (mirrors the REST GET /projects/{id}/members handler, which only
// requires EnsureProjectVisible). Admin, owner and plain member all see
// the same shape.
type projectMemberListResponse struct {
	Members []projects.MemberDTO `json:"members"`
}

func handleProjectMemberList(pool *pgxpool.Pool, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		projectArg, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, projectArg, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		// Visibility check — Resolve already enforced this for non-UUID
		// inputs, but the UUID branch only checks visibility once. Keep
		// it explicit so the code reads consistently.
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, projectID, userID); vErr != nil {
			return projectErrorResult(&ProjectNotFoundError{Query: projectArg})
		}

		members, err := projectsStore.ListMembers(ctx, projectID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_members", "action", "list",
				"reason", "list members failed",
				"actorUserId", userID.String(), "projectId", projectID.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list members", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_members", "action", "list",
			"actorUserId", userID.String(), "projectId", projectID.String(),
			"count", len(members),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(projectMemberListResponse{Members: members})
	}
}

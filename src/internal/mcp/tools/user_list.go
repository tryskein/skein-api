package tools

import (
	"context"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/users"
)

// userListResponse is the wire shape returned by
// skein_user(action="list") — a cursor-paginated page of users.
type userListResponse struct {
	Users      []users.UserDTO `json:"users"`
	NextCursor *string         `json:"nextCursor,omitempty"`
	HasMore    bool            `json:"hasMore"`
}

func handleUserList(store *users.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		if errResp := requireAdminTool(ctx); errResp != nil {
			return errResp, nil
		}

		archived := "false"
		if req.GetBool("archived", false) {
			archived = "true"
		}
		// "all" branch is reachable only via the REST endpoint today —
		// the MCP boolean flag is intentionally limited to the binary
		// case so the closed-set tool surface stays simple.

		in := users.ListUsersInput{
			Archived: archived,
			Search:   req.GetString("search", ""),
			Limit:    req.GetInt("limit", 50),
		}
		if cursor := req.GetString("cursor", ""); cursor != "" {
			t, id, err := users.DecodeCursor(cursor)
			if err != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"invalid cursor: "+err.Error(),
					map[string]any{"field": "cursor"}), nil
			}
			in.CursorTime = &t
			in.CursorID = &id
		}

		res, err := store.ListUsers(ctx, in)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_user", "action", "list",
				"reason", "list users failed",
				"actorUserId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list users", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_user", "action", "list",
			"actorUserId", userID.String(),
			"count", len(res.Items),
			"hasMore", res.HasMore,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(userListResponse{
			Users:      res.Items,
			NextCursor: res.NextCursor,
			HasMore:    res.HasMore,
		})
	}
}

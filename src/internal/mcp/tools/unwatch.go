package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/notifications"
)

type unwatchResponse struct {
	IssueID  string `json:"issueId"`
	Watching bool   `json:"watching"`
}

// handleUnwatch backs skein_watchers(action="unwatch") — the caller stops
// watching one issue. Mirrors DELETE /api/v1/issues/{id}/watch through the
// same primitive (notifications.Store#RemoveIssueWatch, sk-622.21 US-1): a
// subscription is deleted and an opt-out is recorded, so the automatic
// paths (creator / assignee / reopen) do not re-subscribe the user without
// an explicit action="watch"; the issue's Watching card is cleared on the
// caller's notifications. Without a subscription (a historical card) only
// the card is cleared — no opt-out (R3). Idempotent.
func handleUnwatch(pool *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		id, f := issueops.UnwatchIssue(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(unwatchResponse{IssueID: id, Watching: false})
	}
}

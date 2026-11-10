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

type watchResponse struct {
	IssueID  string `json:"issueId"`
	Watching bool   `json:"watching"`
}

// handleWatch backs skein_watchers(action="watch") — the caller manually
// subscribes to one issue. Mirrors POST /api/v1/issues/{id}/watch. Sets
// reason='manual' and deletes the caller's opt-out on the issue, if an
// earlier unwatch recorded one (sk-622.21, US-1); re-watching the same
// issue is a no-op. subtree=true also covers every hierarchical
// descendant (and future subtasks) and dominates: it upgrades an existing
// plain watch, and a later subtree=false watch never downgrades it.
//
// The issue id arrives as `issueId`, not `id` (sk-255.3): inside the
// consolidated tool the same schema also carries `filterId` — a custom
// watcher UUID — and one `id` meaning two different identifier kinds is
// exactly the ambiguity a flat schema cannot afford.
func handleWatch(pool *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		id, f := issueops.WatchIssue(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(watchResponse{IssueID: id, Watching: true})
	}
}

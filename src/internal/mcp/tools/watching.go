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

type watchingResponse struct {
	Issues            []notifications.IssueWatcherForUserDTO `json:"issues"`
	Projects          []notifications.ProjectWatcherDTO      `json:"projects"`
	Custom            []notifications.CustomWatcherDTO       `json:"custom"`
	IssuesTruncated   bool                                   `json:"issuesTruncated"`
	ProjectsTruncated bool                                   `json:"projectsTruncated"`
	CustomTruncated   bool                                   `json:"customTruncated"`
}

// handleWatching backs skein_watchers(action="list") — the caller's
// 3-section watcher view (issues / projects / custom). Mirrors
// GET /api/v1/me/watchers. No paging; each section is capped at 50 rows
// with a `truncated` flag the UI can surface. Read-only.
func handleWatching(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		all, f := issueops.Watching(ctx, d, userID)
		if f != nil {
			return toolFailure(f), nil
		}
		// ListAllWatchers caps each section at 50: exactly 50 rows raise the
		// section's truncation flag.
		return skmcp.ToolJSON(watchingResponse{
			Issues:            all.Issues,
			Projects:          all.Projects,
			Custom:            all.Custom,
			IssuesTruncated:   len(all.Issues) >= 50,
			ProjectsTruncated: len(all.Projects) >= 50,
			CustomTruncated:   len(all.Custom) >= 50,
		})
	}
}

package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		all, err := notifStore.ListAllWatchers(ctx, userID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "list", "userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list watchers", nil), nil
		}

		// Truncation flag: ListAllWatchers caps at 50 with LIMIT, so when
		// we see exactly 50 rows we surface the flag — the caller knows
		// to fall back to REST for the full list.
		resp := watchingResponse{
			Issues:            all.Issues,
			Projects:          all.Projects,
			Custom:            all.Custom,
			IssuesTruncated:   len(all.Issues) >= 50,
			ProjectsTruncated: len(all.Projects) >= 50,
			CustomTruncated:   len(all.Custom) >= 50,
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "list", "userId", userID.String(),
			"issues", len(all.Issues), "projects", len(all.Projects), "custom", len(all.Custom),
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(resp)
	}
}

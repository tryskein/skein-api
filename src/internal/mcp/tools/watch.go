package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		issueID := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": rawID}), nil
		}
		subtree := req.GetBool("subtree", false)

		// Resolve the issue's project so we can run the visibility check.
		if _, errResp := resolveVisibleIssueProject(ctx, pool, "skein_watchers", issueID, userID); errResp != nil {
			return errResp, nil
		}

		if err := notifStore.AddIssueWatch(ctx, issueID, userID, subtree); err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "watch", "reason", "add issue watch failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to add watch", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "watch",
			"userId", userID.String(), "issueId", issueID,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(watchResponse{IssueID: issueID, Watching: true})
	}
}

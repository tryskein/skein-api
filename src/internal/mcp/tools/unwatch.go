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

		if _, errResp := resolveVisibleIssueProject(ctx, pool, "skein_watchers", issueID, userID); errResp != nil {
			return errResp, nil
		}

		if err := notifStore.RemoveIssueWatch(ctx, issueID, userID); err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "unwatch", "reason", "remove issue watch failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to remove watch", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "unwatch",
			"userId", userID.String(), "issueId", issueID,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(unwatchResponse{IssueID: issueID, Watching: false})
	}
}

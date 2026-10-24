package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/reminders"
)

// handleRemindDismiss backs skein_remind(action="dismiss") (sk-62.2).
// Deletes a reminder (dismiss = delete; no soft-delete). Same targeting
// rule as snooze: reminderId, or issueId when the caller has exactly one
// active reminder on the issue.
//
// Until sk-255.3 this was its own tool, skein_remind_dismiss; the schema
// now lives in remind_group.go. It is the action that makes the whole
// consolidated tool declare destructiveHint=true.
func handleRemindDismiss(pool *pgxpool.Pool, store *reminders.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		id, errResp := resolveReminderTarget(ctx, store, userID,
			req.GetString("reminderId", ""), req.GetString("issueId", ""))
		if errResp != nil {
			return errResp, nil
		}
		note := strings.TrimSpace(req.GetString("note", ""))

		actor := RemindersAgentActor(ctx, pool, userID)
		if err := store.Dismiss(ctx, id, userID, note, actor); err != nil {
			if errors.Is(err, reminders.ErrNotFound) {
				return skmcp.ToolError(ErrCodeReminderNotFound,
					"reminder does not exist or is not yours",
					map[string]any{"reminderId": id.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_remind", "action", "dismiss",
				"reason", "store dismiss failed", "userId", userID.String(),
				"reminderId", id.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to dismiss reminder", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok", "tool", "skein_remind", "action", "dismiss",
			"userId", userID.String(), "reminderId", id.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(map[string]any{
			"dismissed":  true,
			"reminderId": id.String(),
		})
	}
}

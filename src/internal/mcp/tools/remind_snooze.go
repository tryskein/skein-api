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

// handleRemindSnooze backs skein_remind(action="snooze") (sk-62.2).
// Pushes a reminder's ring time forward by a relative offset (now + by)
// or to an absolute instant (until), resetting the notified flag so it
// rings again.
//
// Targeting: pass reminderId for the exact reminder, OR issueId when the
// caller has exactly one active reminder on that issue (>1 →
// reminder_target_ambiguous listing the candidates).
//
// Until sk-255.3 this was its own tool, skein_remind_snooze; the schema
// now lives in remind_group.go. Behaviour is unchanged — including
// `reason`, which stays a separate parameter from the group's shared
// `note` rather than being merged into it.
func handleRemindSnooze(pool *pgxpool.Pool, store *reminders.Store) server.ToolHandlerFunc {
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

		newRemindAt, derr := resolveSnoozeTime(
			strings.TrimSpace(req.GetString("by", "")),
			strings.TrimSpace(req.GetString("until", "")),
			time.Now().UTC())
		if derr != nil {
			return skmcp.ToolError(ErrCodeValidationError, derr.Error(), nil), nil
		}
		reason := strings.TrimSpace(req.GetString("reason", ""))

		actor := RemindersAgentActor(ctx, pool, userID)
		rem, err := store.Snooze(ctx, id, userID, newRemindAt, reason, actor)
		if err != nil {
			if errors.Is(err, reminders.ErrNotFound) {
				return skmcp.ToolError(ErrCodeReminderNotFound,
					"reminder does not exist or is not yours",
					map[string]any{"reminderId": id.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_remind", "action", "snooze",
				"reason", "store snooze failed", "userId", userID.String(),
				"reminderId", id.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to snooze reminder", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok", "tool", "skein_remind", "action", "snooze",
			"userId", userID.String(), "reminderId", id.String(),
			"snoozeCount", rem.SnoozeCount, "latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(rem)
	}
}

// resolveSnoozeTime computes the new remind_at from exactly one of by /
// until (by relative to now). Mirrors the REST handler's
// resolveSnoozeTarget — kept here so the mcp/tools package doesn't import
// the handler package.
func resolveSnoozeTime(by, until string, now time.Time) (time.Time, error) {
	hasBy := by != ""
	hasUntil := until != ""
	switch {
	case hasBy == hasUntil:
		return time.Time{}, errors.New("supply exactly one of `by` or `until`")
	case hasBy:
		d, err := reminders.ParseBy(by)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d).UTC(), nil
	default:
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return time.Time{}, errors.New("until must be an RFC3339 timestamp")
		}
		return t.UTC(), nil
	}
}

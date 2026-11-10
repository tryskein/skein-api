package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
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
	d := issueops.Deps{Pool: pool, Reminders: store}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rem, f := issueops.RemindSnooze(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(rem)
	}
}

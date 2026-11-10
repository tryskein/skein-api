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

// handleRemindDismiss backs skein_remind(action="dismiss") (sk-62.2).
// Deletes a reminder (dismiss = delete; no soft-delete). Same targeting
// rule as snooze: reminderId, or issueId when the caller has exactly one
// active reminder on the issue.
//
// Until sk-255.3 this was its own tool, skein_remind_dismiss; the schema
// now lives in remind_group.go. It is the action that makes the whole
// consolidated tool declare destructiveHint=true.
func handleRemindDismiss(pool *pgxpool.Pool, store *reminders.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Reminders: store}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		id, f := issueops.RemindDismiss(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(map[string]any{
			"dismissed":  true,
			"reminderId": id.String(),
		})
	}
}

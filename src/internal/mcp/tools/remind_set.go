package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/reminders"
)

// handleRemindSet backs skein_remind(action="set") (sk-62.2). Creates a
// personal reminder on an issue for the calling user. When
// remindAt passes, the scheduler materialises a bell notification.
//
// Until sk-255.3 this was its own tool, skein_remind_set; the schema now
// lives in remind_group.go and the handler is reached through the
// `action` discriminator. Behaviour is unchanged.
func handleRemindSet(pool *pgxpool.Pool, store *reminders.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Reminders: store, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rem, f := issueops.RemindSet(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(rem)
	}
}

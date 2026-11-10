package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// remindListItem is the slim per-reminder shape returned by
// skein_remind(action="list") — issue id/title + remind_at + derived state +
// reminderId + snoozeCount, the fields an agent needs to reason about
// "what's ringing / armed".
type remindListItem = issueops.ReminderListItem

// remindListResult is the tool payload: the (optionally project-filtered)
// list + the cross-project ringing-task badge count.
type remindListResult struct {
	Reminders        []remindListItem `json:"reminders"`
	RingingTaskCount int              `json:"ringingTaskCount"`
}

// handleRemindList backs skein_remind(action="list") (sk-62.2) — the
// caller's "what's ringing / armed" lens across projects, plus the
// ringing-task badge count. Rows come back most-urgent-first: ringing
// (due) before armed (future). Pure read.
//
// Until sk-255.3 this was its own tool, skein_remind_list; the schema now
// lives in remind_group.go, where it is the one read-only action of a
// tool whose annotations are coarsened to readOnly=false.
func handleRemindList(pool *pgxpool.Pool, store *reminders.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Reminders: store, Projects: projectsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.RemindList(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(remindListResult{Reminders: res.Reminders, RingingTaskCount: res.RingingTaskCount})
	}
}

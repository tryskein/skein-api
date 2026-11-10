package tools

import (
	"context"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/notifications"
)

// handleInbox backs skein_notifications(action="inbox") — paginated read
// of the caller's notifications inbox. Mirrors GET /api/v1/notifications.
//
// Token-budget tuning: default page size 25 (vs the REST default of 50)
// — agents typically want a quick scan, not the full backlog. Limit
// caps at 100.
//
// Until sk-255.3 this was its own tool, skein_inbox; the schema now lives
// in notifications_group.go, where it is the one read-only action of a
// tool whose annotations are coarsened to readOnly=false.
func handleInbox(notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Inbox(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		rows, perr := projectInboxRows(res.Page.Items)
		if perr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "inbox",
				"reason", "row projection failed",
				"userId", userID.String(), "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}
		resp := inboxResponse{Items: rows, NextCursor: res.Page.NextCursor, HasMore: res.Page.HasMore}
		if res.FirstPage {
			resp.Invitations = res.Invitations
		}
		return skmcp.ToolJSON(resp)
	}
}

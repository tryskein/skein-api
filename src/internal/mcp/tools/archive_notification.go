package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/notifications"
)

type archiveNotificationResponse struct {
	ID int64 `json:"id"`
}

// handleArchiveNotification backs skein_notifications(action="archive") —
// move one notification out of the inbox into the archive view. Mirrors
// POST /api/v1/notifications/{id}/archive. Idempotent — re-archiving an
// already-archived row returns the same shape without error.
//
// Until sk-255.3 this was its own tool, skein_archive_notification, and
// it took the notification id as `id`. The consolidated tool calls it
// `notificationId`, the name the mark_read family already used: one flat
// schema cannot carry both names for the same column without an agent
// having to remember which action spells it which way. The response field
// stays `id` — that is the tool's output shape, not its input, and the
// REST endpoint returns the same.
func handleArchiveNotification(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		id, f := issueops.ArchiveNotification(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(archiveNotificationResponse{ID: id})
	}
}

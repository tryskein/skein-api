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

// Marking notifications read used to be one tool, skein_mark_read, with a
// presence-based one-of: exactly one of notificationId / issueId /
// all=true had to be set, and every other combination — including sending
// none of them — came back as a validation_error the caller had to fix by
// re-reading the description.
//
// sk-255.3 folded it into skein_notifications and flattened that one-of
// into the top-level `action` enum: mark_read (one row), mark_read_issue
// (one issue and its comments), mark_read_all (every unread row). The
// width of the sweep is now named in the discriminator an agent has to
// pick anyway, so the "exactly one" class of error stops existing for the
// common case, and each handler below states its own single requirement.
//
// What did NOT go away is the check behind that one-of. An agent that
// picks mark_read_all and also sends notificationId is expressing two
// widths at once — the exact ambiguity the old validator rejected — and
// on THIS family guessing wrong is asymmetric: silently ignoring the
// narrow argument marks the caller's whole inbox read, and nothing undoes
// that. Each handler therefore rejects the targeting arguments that
// belong to its siblings, naming the action the caller probably wanted.
// It is the same rule as before, restated per action instead of counted.

type markReadResponse struct {
	Updated int64 `json:"updated"`
}

func handleMarkReadOne(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		if f := issueops.MarkNotificationRead(ctx, d, userID, issueops.Args(req.GetArguments())); f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(markReadResponse{Updated: 1})
	}
}

func handleMarkReadIssue(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		n, f := issueops.MarkIssueNotificationsRead(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(markReadResponse{Updated: n})
	}
}

func handleMarkReadCard(notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.MarkWatchingCardRead(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(res)
	}
}

func handleMarkReadAll(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		n, f := issueops.MarkAllNotificationsRead(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(markReadResponse{Updated: n})
	}
}

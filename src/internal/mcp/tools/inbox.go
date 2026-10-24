package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		args := notifications.ListInboxArgs{UserID: userID}
		args.Scope = strings.TrimSpace(req.GetString("scope", "unread"))
		switch args.Scope {
		case "unread", "all", "archived":
			// ok
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"scope must be one of: unread, all, archived",
				map[string]any{"scope": args.Scope}), nil
		}

		if pidRaw := strings.TrimSpace(req.GetString("projectId", "")); pidRaw != "" {
			pid, err := uuid.Parse(pidRaw)
			if err != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"projectId must be a UUID",
					map[string]any{"projectId": pidRaw}), nil
			}
			args.ProjectID = &pid
		}

		if cursor := strings.TrimSpace(req.GetString("cursor", "")); cursor != "" {
			id, err := notifications.DecodeCursor(cursor)
			if err != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"invalid cursor: "+err.Error(), nil), nil
			}
			args.Cursor = &id
		}

		if limit := int(req.GetFloat("limit", 25)); limit > 0 {
			if limit > 100 {
				limit = 100
			}
			args.Limit = limit
		} else {
			args.Limit = 25
		}

		res, err := notifStore.ListInbox(ctx, args)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "inbox", "userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list inbox", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_notifications", "action", "inbox", "userId", userID.String(),
			"scope", args.Scope, "count", len(res.Items),
			"latencyMs", time.Since(start).Milliseconds())

		rows, perr := projectInboxRows(res.Items)
		if perr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "inbox",
				"reason", "row projection failed",
				"userId", userID.String(), "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}
		resp := inboxResponse{
			Items:      rows,
			NextCursor: res.NextCursor,
			HasMore:    res.HasMore,
		}
		// sk-674.18 T029 (US-5): the pending invitations to the caller ride
		// on the first page of the live scopes.
		if args.Cursor == nil && args.Scope != "archived" {
			invs, ierr := notifStore.PendingInvitations(ctx, userID)
			if ierr != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_notifications", "action", "inbox",
					"reason", "invitations read failed",
					"userId", userID.String(), "error", ierr.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to list inbox", nil), nil
			}
			resp.Invitations = invs
		}
		return skmcp.ToolJSON(resp)
	}
}

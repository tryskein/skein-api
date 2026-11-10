package issueops

// notifications.go — the notification operations (sk-878.3.1 T027, the
// lead's decision of 10.11.2026 extending 01a1c640): the actions of the MCP
// tool skein_notifications, moved out of it with their checks and refusal
// codes unchanged. The key's scope narrows every one of them in the store
// (T011): a write by a key changes only notifications inside its scope.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/notifications"
	"skein-api/internal/spaces"
)

const notificationsTool = "skein_notifications"

// InboxResult is a page of the caller's notifications and, on the first
// page of a live scope, the invitations to him.
type InboxResult struct {
	Page *notifications.NotificationListResult
	// Invitations are nil on a later page and in scope archived.
	Invitations []spaces.PendingInvitation
	// FirstPage reports whether the request asked for the first page of a
	// live scope (the invitations ride on it).
	FirstPage bool
}

// Inbox reads a page of the caller's notifications (action inbox;
// listNotifications): scope unread (the default), all or archived,
// optionally of one project; limit 1..100, 25 when absent, a larger one is
// capped at 100.
func Inbox(ctx context.Context, d Deps, userID uuid.UUID, a Args) (InboxResult, *Failure) {
	start := time.Now()
	args := notifications.ListInboxArgs{UserID: userID}
	args.Scope = strings.TrimSpace(a.GetString("scope", "unread"))
	switch args.Scope {
	case "unread", "all", "archived":
	default:
		return InboxResult{}, Fail(ErrCodeValidationError, "scope must be one of: unread, all, archived",
			map[string]any{"scope": args.Scope})
	}
	if pidRaw := strings.TrimSpace(a.GetString("projectId", "")); pidRaw != "" {
		pid, err := uuid.Parse(pidRaw)
		if err != nil {
			return InboxResult{}, Fail(ErrCodeValidationError, "projectId must be a UUID",
				map[string]any{"projectId": pidRaw})
		}
		args.ProjectID = &pid
	}
	if cursor := strings.TrimSpace(a.GetString("cursor", "")); cursor != "" {
		id, err := notifications.DecodeCursor(cursor)
		if err != nil {
			return InboxResult{}, Fail(ErrCodeValidationError, "invalid cursor: "+err.Error(), nil)
		}
		args.Cursor = &id
	}
	args.Limit = 25
	if limit := int(a.GetFloat("limit", 25)); limit > 0 {
		args.Limit = min(limit, 100)
	}
	res, err := d.Notifications.ListInbox(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", notificationsTool, "action", "inbox", "userId", userID.String(), "error", err.Error())
		return InboxResult{}, Fail(ErrCodeInternalError, "failed to list inbox", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", notificationsTool, "action", "inbox", "userId", userID.String(),
		"scope", args.Scope, "count", len(res.Items),
		"latencyMs", time.Since(start).Milliseconds())
	out := InboxResult{Page: res, FirstPage: args.Cursor == nil && args.Scope != "archived"}
	// sk-674.18 T029 (US-5): the pending invitations ride on the first page
	// of the live scopes.
	if out.FirstPage {
		invs, ierr := d.Notifications.PendingInvitations(ctx, userID)
		if ierr != nil {
			slog.ErrorContext(ctx, logMessage(ctx, "error"),
				"tool", notificationsTool, "action", "inbox",
				"reason", "invitations read failed",
				"userId", userID.String(), "error", ierr.Error())
			return InboxResult{}, Fail(ErrCodeInternalError, "failed to list inbox", nil)
		}
		out.Invitations = invs
	}
	return out, nil
}

// hasTargetArg reports whether the caller supplied name: a JSON null, an
// omitted key and a blank string are all absent.
func hasTargetArg(a Args, name string) bool {
	v, ok := a[name]
	if !ok || v == nil {
		return false
	}
	if s, isString := v.(string); isString {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// notificationIDArg reads notificationId: present reports whether it was
// supplied; a non-integer or a non-positive one is refused.
func notificationIDArg(a Args) (id int64, present bool, f *Failure) {
	raw, ok := a["notificationId"]
	if !ok || raw == nil {
		return 0, false, nil
	}
	fl, ok := raw.(float64)
	if !ok {
		return 0, true, Fail(ErrCodeValidationError, "notificationId must be an integer", nil)
	}
	id = int64(fl)
	if id <= 0 {
		return 0, true, Fail(ErrCodeValidationError, "notificationId must be a positive integer",
			map[string]any{"notificationId": fl})
	}
	return id, true, nil
}

// MarkNotificationRead marks one notification of the caller read (action
// mark_read; markNotificationRead). One that is not his, missing or out of
// the key's scope is validation_error "notification not found", as MCP
// answers it.
func MarkNotificationRead(ctx context.Context, d Deps, userID uuid.UUID, a Args) *Failure {
	start := time.Now()
	id, present, f := notificationIDArg(a)
	if f != nil {
		return f
	}
	if !present {
		return Fail(ErrCodeValidationError, "notificationId is required for mark_read", nil)
	}
	if hasTargetArg(a, "issueId") {
		return Fail(ErrCodeValidationError,
			"mark_read marks the one notification named by notificationId — "+
				"to mark everything on an issue use action=\"mark_read_issue\"",
			map[string]any{"action": "mark_read"})
	}
	if err := d.Notifications.MarkRead(ctx, id, userID); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			return Fail(ErrCodeValidationError, "notification not found", map[string]any{"notificationId": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", notificationsTool, "action", "mark_read",
			"userId", userID.String(), "notificationId", id, "error", err.Error())
		return Fail(ErrCodeInternalError, "failed to mark read", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", notificationsTool, "action", "mark_read",
		"userId", userID.String(), "notificationId", id,
		"latencyMs", time.Since(start).Milliseconds())
	return nil
}

// markIssueArg reads the issueId of mark_read_issue and mark_read_card, and
// refuses a stray notificationId (a single row is mark_read).
func markIssueArg(a Args, action, what string) (string, *Failure) {
	issueID := strings.TrimSpace(a.GetString("issueId", ""))
	if issueID == "" {
		return "", Fail(ErrCodeValidationError, "issueId is required for "+action, nil)
	}
	if !LooksLikeHierarchicalID(issueID) {
		return "", Fail(ErrCodeValidationError, "issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": issueID})
	}
	if hasTargetArg(a, "notificationId") {
		return "", Fail(ErrCodeValidationError,
			action+" marks every notification on "+what+" — to mark a single row use action=\"mark_read\"",
			map[string]any{"action": action})
	}
	return issueID, nil
}

// MarkIssueNotificationsRead marks read every notification of the caller
// tied to one issue, its comments included (action mark_read_issue;
// markIssueNotificationsRead), and answers how many. Zero is an answer, not
// a refusal.
func MarkIssueNotificationsRead(ctx context.Context, d Deps, userID uuid.UUID, a Args) (int64, *Failure) {
	start := time.Now()
	issueID, f := markIssueArg(a, "mark_read_issue", "the issue")
	if f != nil {
		return 0, f
	}
	n, err := d.Notifications.MarkReadByEntity(ctx, userID, "issue", issueID)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", notificationsTool, "action", "mark_read_issue",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return 0, Fail(ErrCodeInternalError, "failed to mark read", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", notificationsTool, "action", "mark_read_issue",
		"userId", userID.String(), "issueId", issueID, "updated", n,
		"latencyMs", time.Since(start).Milliseconds())
	return n, nil
}

// MarkWatchingCardRead reads a Watching card (action mark_read_card;
// markWatchingCardRead): every unread notification carded to the issue, the
// descendants' events of a subtree subscription included — of the key's
// scope only. A card that is somebody else's, missing or has nothing unread
// answers marked 0.
func MarkWatchingCardRead(ctx context.Context, d Deps, userID uuid.UUID, a Args) (notifications.MarkCardReadResult, *Failure) {
	start := time.Now()
	issueID, f := markIssueArg(a, "mark_read_card", "the Watching card")
	if f != nil {
		return notifications.MarkCardReadResult{}, f
	}
	res, err := d.Notifications.MarkReadCard(ctx, userID, issueID)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", notificationsTool, "action", "mark_read_card",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return notifications.MarkCardReadResult{}, Fail(ErrCodeInternalError, "failed to mark read", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", notificationsTool, "action", "mark_read_card",
		"userId", userID.String(), "issueId", issueID, "marked", res.Marked,
		"latencyMs", time.Since(start).Milliseconds())
	return res, nil
}

// MarkAllNotificationsRead marks every unread, unarchived notification of
// the caller read, optionally of one project (action mark_read_all;
// markAllNotificationsRead) — of the key's scope only — and answers how
// many. A stray notificationId or issueId is refused: the widest write of
// the surface has no undo.
func MarkAllNotificationsRead(ctx context.Context, d Deps, userID uuid.UUID, a Args) (int64, *Failure) {
	start := time.Now()
	if hasTargetArg(a, "notificationId") || hasTargetArg(a, "issueId") {
		return 0, Fail(ErrCodeValidationError,
			"mark_read_all marks every unread notification read and cannot be "+
				"narrowed by notificationId or issueId — use action=\"mark_read\" "+
				"for one row, \"mark_read_issue\" for one issue, or projectId to "+
				"scope the sweep to a project",
			map[string]any{"action": "mark_read_all"})
	}
	markArgs := notifications.MarkAllReadArgs{UserID: userID}
	if pidRaw := strings.TrimSpace(a.GetString("projectId", "")); pidRaw != "" {
		pid, err := uuid.Parse(pidRaw)
		if err != nil {
			return 0, Fail(ErrCodeValidationError, "projectId must be a UUID", map[string]any{"projectId": pidRaw})
		}
		markArgs.ProjectID = &pid
	}
	n, err := d.Notifications.MarkAllRead(ctx, markArgs)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", notificationsTool, "action", "mark_read_all",
			"userId", userID.String(), "error", err.Error())
		return 0, Fail(ErrCodeInternalError, "failed to mark read", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", notificationsTool, "action", "mark_read_all",
		"userId", userID.String(), "updated", n,
		"latencyMs", time.Since(start).Milliseconds())
	return n, nil
}

// ArchiveNotification archives one notification of the caller (action
// archive; archiveNotification). One that is not his, missing or out of the
// key's scope is validation_error "notification not found", as MCP answers
// it.
func ArchiveNotification(ctx context.Context, d Deps, userID uuid.UUID, a Args) (int64, *Failure) {
	start := time.Now()
	id, present, f := notificationIDArg(a)
	if f != nil {
		return 0, f
	}
	if !present {
		return 0, Fail(ErrCodeValidationError, "notificationId is required for archive", nil)
	}
	if err := d.Notifications.Archive(ctx, id, userID); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			return 0, Fail(ErrCodeValidationError, "notification not found", map[string]any{"notificationId": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", notificationsTool, "action", "archive",
			"userId", userID.String(), "notificationId", id, "error", err.Error())
		return 0, Fail(ErrCodeInternalError, "failed to archive notification", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", notificationsTool, "action", "archive",
		"userId", userID.String(), "notificationId", id,
		"latencyMs", time.Since(start).Milliseconds())
	return id, nil
}

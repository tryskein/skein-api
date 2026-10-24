package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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

// hasTargetArg reports whether the caller actually supplied `name`.
//
// Presence, not value: a JSON null and an omitted key are both "absent",
// and — matching the pre-sk-255.3 behaviour of skein_mark_read, which
// treated issueId:"" as unset — so is a blank string.
func hasTargetArg(req mcpgo.CallToolRequest, name string) bool {
	v, ok := req.GetArguments()[name]
	if !ok || v == nil {
		return false
	}
	if s, isString := v.(string); isString {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// notificationIDArg reads the shared `notificationId` argument.
//
// It carries the type and range validation the folded tools did on their
// own: JSON numbers arrive as float64, and a non-positive id is a
// client-side mistake worth naming rather than a lookup that will miss.
// The bool distinguishes "absent" from "present and rejected", so callers
// can tell a missing required argument from a malformed one.
func notificationIDArg(req mcpgo.CallToolRequest) (int64, bool, *mcpgo.CallToolResult) {
	raw, ok := req.GetArguments()["notificationId"]
	if !ok || raw == nil {
		return 0, false, nil
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, true, skmcp.ToolError(ErrCodeValidationError,
			"notificationId must be an integer", nil)
	}
	id := int64(f)
	if id <= 0 {
		return 0, true, skmcp.ToolError(ErrCodeValidationError,
			"notificationId must be a positive integer",
			map[string]any{"notificationId": f})
	}
	return id, true, nil
}

// handleMarkReadOne backs skein_notifications(action="mark_read") — flip
// read_at on exactly one notification the caller owns.
func handleMarkReadOne(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		notificationID, present, errResp := notificationIDArg(req)
		if errResp != nil {
			return errResp, nil
		}
		if !present {
			return skmcp.ToolError(ErrCodeValidationError,
				"notificationId is required for mark_read", nil), nil
		}
		if hasTargetArg(req, "issueId") {
			return skmcp.ToolError(ErrCodeValidationError,
				"mark_read marks the one notification named by notificationId — "+
					"to mark everything on an issue use action=\"mark_read_issue\"",
				map[string]any{"action": "mark_read"}), nil
		}

		if err := notifStore.MarkRead(ctx, notificationID, userID); err != nil {
			if errors.Is(err, notifications.ErrNotFound) {
				return skmcp.ToolError(ErrCodeValidationError,
					"notification not found",
					map[string]any{"notificationId": notificationID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "mark_read",
				"userId", userID.String(), "notificationId", notificationID,
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mark read", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_notifications", "action", "mark_read",
			"userId", userID.String(), "notificationId", notificationID,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(markReadResponse{Updated: 1})
	}
}

// handleMarkReadIssue backs skein_notifications(action="mark_read_issue")
// — flip read_at on every notification tied to one issue, comments on
// that issue included (see notifications.Store.MarkReadByEntity).
//
// Unlike mark_read there is no not-found path: the store reports how many
// rows it touched and zero is a legitimate answer ("nothing unread
// there"), which is why the response is a count rather than an ack.
func handleMarkReadIssue(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		issueID := strings.TrimSpace(req.GetString("issueId", ""))
		if issueID == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId is required for mark_read_issue", nil), nil
		}
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": issueID}), nil
		}
		if hasTargetArg(req, "notificationId") {
			return skmcp.ToolError(ErrCodeValidationError,
				"mark_read_issue marks every notification on the issue — "+
					"to mark a single row use action=\"mark_read\"",
				map[string]any{"action": "mark_read_issue"}), nil
		}

		n, err := notifStore.MarkReadByEntity(ctx, userID, "issue", issueID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "mark_read_issue",
				"userId", userID.String(), "issueId", issueID,
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mark read", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_notifications", "action", "mark_read_issue",
			"userId", userID.String(), "issueId", issueID, "updated", n,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(markReadResponse{Updated: n})
	}
}

// handleMarkReadCard backs skein_notifications(action="mark_read_card")
// (sk-622.21, US-7) — "read the card": flip read_at on every unread
// notification whose Watching card is `issueId`, the descendants' events the
// fan-out carded to it included (see notifications.Store.MarkReadCard). It
// differs from mark_read_issue, which reads the task's own events only and
// is unchanged.
//
// Like mark_read_issue it has no not-found path: a card that is somebody
// else's, does not exist or has nothing unread answers marked = 0. The
// response is the REST shape of the same operation, {"marked": N}.
func handleMarkReadCard(notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		issueID := strings.TrimSpace(req.GetString("issueId", ""))
		if issueID == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId is required for mark_read_card", nil), nil
		}
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": issueID}), nil
		}
		if hasTargetArg(req, "notificationId") {
			return skmcp.ToolError(ErrCodeValidationError,
				"mark_read_card marks every notification on the Watching card — "+
					"to mark a single row use action=\"mark_read\"",
				map[string]any{"action": "mark_read_card"}), nil
		}

		res, err := notifStore.MarkReadCard(ctx, userID, issueID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "mark_read_card",
				"userId", userID.String(), "issueId", issueID,
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mark read", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_notifications", "action", "mark_read_card",
			"userId", userID.String(), "issueId", issueID, "marked", res.Marked,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(res)
	}
}

// handleMarkReadAll backs skein_notifications(action="mark_read_all") —
// flip read_at on every unread, unarchived notification the caller owns,
// optionally narrowed to one project.
//
// This is the widest write on the whole notifications surface and it has
// no undo, which is why it rejects a stray notificationId / issueId
// instead of ignoring it: a caller that sent one meant a narrower sweep.
func handleMarkReadAll(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		if hasTargetArg(req, "notificationId") || hasTargetArg(req, "issueId") {
			return skmcp.ToolError(ErrCodeValidationError,
				"mark_read_all marks every unread notification read and cannot be "+
					"narrowed by notificationId or issueId — use action=\"mark_read\" "+
					"for one row, \"mark_read_issue\" for one issue, or projectId to "+
					"scope the sweep to a project",
				map[string]any{"action": "mark_read_all"}), nil
		}

		markArgs := notifications.MarkAllReadArgs{UserID: userID}
		if pidRaw := strings.TrimSpace(req.GetString("projectId", "")); pidRaw != "" {
			pid, err := uuid.Parse(pidRaw)
			if err != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"projectId must be a UUID",
					map[string]any{"projectId": pidRaw}), nil
			}
			markArgs.ProjectID = &pid
		}
		n, err := notifStore.MarkAllRead(ctx, markArgs)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_notifications", "action", "mark_read_all",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mark read", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_notifications", "action", "mark_read_all",
			"userId", userID.String(), "updated", n,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(markReadResponse{Updated: n})
	}
}

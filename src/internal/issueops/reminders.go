package issueops

// reminders.go — the reminder operations (sk-878.3.1 T027, the lead's
// decision of 10.11.2026 extending 01a1c640): the actions of the MCP tool
// skein_remind, moved out of it with their checks and refusal codes
// unchanged. A reminder is the caller's own; one on an issue outside the
// key's scope is neither listed nor counted, and snoozing or dismissing it
// answers as for a missing one (T011, in the store).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

const remindTool = "skein_remind"

// ReminderListItem is one reminder of RemindList.
type ReminderListItem struct {
	// ReminderID is the reminder's UUID.
	ReminderID string `json:"reminderId"`
	// IssueID and IssueTitle name the issue it rings on.
	IssueID    string `json:"issueId"`
	IssueTitle string `json:"issueTitle"`
	// ProjectID is the issue's project.
	ProjectID string `json:"projectId"`
	// RemindAt is when it rings, RFC 3339.
	RemindAt string `json:"remindAt"`
	// State: ringing (due) or armed (future).
	State string `json:"state"`
	// SnoozeCount is how often it was snoozed.
	SnoozeCount int `json:"snoozeCount"`
	// NotifiedAt is when it last rang, RFC 3339; omitted before it did.
	NotifiedAt string `json:"notifiedAt,omitempty"`
}

// ReminderList is the answer of RemindList.
type ReminderList struct {
	Reminders []ReminderListItem
	// RingingTaskCount is the badge: issues with a ringing reminder, across
	// projects (the key's scope applies).
	RingingTaskCount int
}

// ReminderRef is a reminder named in reminder_target_ambiguous.
type ReminderRef struct {
	ReminderID  string `json:"reminderId"`
	IssueID     string `json:"issueId"`
	IssueTitle  string `json:"issueTitle"`
	RemindAt    string `json:"remindAt"`
	State       string `json:"state"`
	SnoozeCount int    `json:"snoozeCount"`
}

// RemindSet sets a reminder of the caller on an issue he sees (action set;
// createReminder): remindAt RFC 3339, an optional note.
func RemindSet(ctx context.Context, d Deps, userID uuid.UUID, a Args) (reminders.Reminder, *Failure) {
	start := time.Now()
	rawID, err := a.RequireString("issueId")
	if err != nil {
		return reminders.Reminder{}, Fail(ErrCodeValidationError, "issueId is required", nil)
	}
	issueID := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(issueID) {
		return reminders.Reminder{}, Fail(ErrCodeValidationError, "issueId must be a full hierarchical id like \"sk-62\"",
			map[string]any{"issueId": rawID})
	}
	rawRemind, err := a.RequireString("remindAt")
	if err != nil {
		return reminders.Reminder{}, Fail(ErrCodeValidationError, "remindAt is required", nil)
	}
	remindAt, perr := time.Parse(time.RFC3339, strings.TrimSpace(rawRemind))
	if perr != nil {
		return reminders.Reminder{}, Fail(ErrCodeValidationError, "remindAt must be an RFC3339 timestamp", map[string]any{"remindAt": rawRemind})
	}
	note := strings.TrimSpace(a.GetString("note", ""))
	hidden := Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible", map[string]any{"id": issueID})

	issue, err := d.Issues.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return reminders.Reminder{}, hidden
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "tool", remindTool, "action", "set",
			"reason", "load issue failed", "userId", userID.String(), "issueId", issueID, "error", err.Error())
		return reminders.Reminder{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, d.Pool, issue.ProjectID, userID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			return reminders.Reminder{}, hidden
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "tool", remindTool, "action", "set",
			"reason", "visibility check failed", "userId", userID.String(), "issueId", issueID, "error", vErr.Error())
		return reminders.Reminder{}, Fail(ErrCodeInternalError, "visibility check failed", nil)
	}
	rem, err := d.Reminders.Create(ctx, issueID, userID, remindAt, note, RemindersAgentActor(ctx, d.Pool, userID))
	if err != nil {
		if errors.Is(err, reminders.ErrIssueNotFound) {
			return reminders.Reminder{}, hidden
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "tool", remindTool, "action", "set",
			"reason", "store create failed", "userId", userID.String(), "issueId", issueID, "error", err.Error())
		return reminders.Reminder{}, Fail(ErrCodeInternalError, "failed to create reminder", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"), "tool", remindTool, "action", "set",
		"userId", userID.String(), "issueId", issueID,
		"reminderId", rem.ID.String(), "latencyMs", time.Since(start).Milliseconds())
	return rem, nil
}

// RemindList lists the caller's reminders, the most urgent first (action
// list; listReminders): state ringing, armed or all (the default), project
// a handle narrowing the list (not the count).
func RemindList(ctx context.Context, d Deps, userID uuid.UUID, a Args) (ReminderList, *Failure) {
	start := time.Now()
	filter, ok := reminders.ParseListFilter(strings.TrimSpace(a.GetString("state", "")))
	if !ok {
		return ReminderList{}, Fail(ErrCodeValidationError, "state must be one of ringing | armed | all", nil)
	}
	var projectFilter *uuid.UUID
	if raw := strings.TrimSpace(a.GetString("project", "")); raw != "" {
		pid, err := ResolveProjectHandle(ctx, d.Pool, d.Projects, raw, userID)
		if err != nil {
			return ReminderList{}, ProjectFailure(err)
		}
		projectFilter = &pid
	}
	resp, err := d.Reminders.ListAndCount(ctx, userID, filter)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "tool", remindTool, "action", "list",
			"reason", "store list failed", "userId", userID.String(), "error", err.Error())
		return ReminderList{}, Fail(ErrCodeInternalError, "failed to list reminders", nil)
	}
	items := make([]ReminderListItem, 0, len(resp.Reminders))
	for _, r := range resp.Reminders {
		if projectFilter != nil && r.ProjectID != *projectFilter {
			continue
		}
		item := ReminderListItem{
			ReminderID: r.ID.String(), IssueID: r.IssueID, IssueTitle: r.IssueTitle,
			ProjectID: r.ProjectID.String(), RemindAt: r.RemindAt.Format(time.RFC3339),
			State: string(r.State), SnoozeCount: r.SnoozeCount,
		}
		if r.NotifiedAt != nil {
			item.NotifiedAt = r.NotifiedAt.Format(time.RFC3339)
		}
		items = append(items, item)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"), "tool", remindTool, "action", "list",
		"userId", userID.String(), "count", len(items),
		"ringingTaskCount", resp.RingingTaskCount, "latencyMs", time.Since(start).Milliseconds())
	return ReminderList{Reminders: items, RingingTaskCount: resp.RingingTaskCount}, nil
}

// reminderTarget is the reminder a snooze or a dismiss acts on: reminderId,
// or the caller's one reminder on issueId (several —
// reminder_target_ambiguous with them; none — reminder_not_found).
func reminderTarget(ctx context.Context, d Deps, ownerID uuid.UUID, a Args) (uuid.UUID, *Failure) {
	rawReminderID, rawIssueID := a.GetString("reminderId", ""), a.GetString("issueId", "")
	reminderID, issueID := strings.TrimSpace(rawReminderID), strings.TrimSpace(rawIssueID)
	if reminderID != "" {
		id, err := uuid.Parse(reminderID)
		if err != nil {
			return uuid.Nil, Fail(ErrCodeValidationError, "reminderId must be a UUID", map[string]any{"reminderId": rawReminderID})
		}
		return id, nil
	}
	if issueID == "" {
		return uuid.Nil, Fail(ErrCodeValidationError, "supply either reminderId or issueId", nil)
	}
	if !LooksLikeHierarchicalID(issueID) {
		return uuid.Nil, Fail(ErrCodeValidationError, "issueId must be a full hierarchical id like \"sk-62\"",
			map[string]any{"issueId": rawIssueID})
	}
	list, err := d.Reminders.ListForUserIssue(ctx, ownerID, issueID, time.Now().UTC())
	if err != nil {
		return uuid.Nil, Fail(ErrCodeInternalError, "failed to resolve reminders for issue", nil)
	}
	switch len(list) {
	case 0:
		return uuid.Nil, Fail(ErrCodeReminderNotFound, "you have no reminder on this issue", map[string]any{"issueId": issueID})
	case 1:
		return list[0].ID, nil
	}
	refs := make([]ReminderRef, 0, len(list))
	for _, r := range list {
		refs = append(refs, ReminderRef{
			ReminderID: r.ID.String(), IssueID: r.IssueID, IssueTitle: r.IssueTitle,
			RemindAt: r.RemindAt.Format(time.RFC3339), State: string(r.State), SnoozeCount: r.SnoozeCount,
		})
	}
	return uuid.Nil, Fail(ErrCodeReminderTargetAmbiguous,
		"more than one reminder on this issue — pass reminderId to disambiguate",
		map[string]any{"issueId": issueID, "reminders": refs})
}

// SnoozeTime is the new remind_at of a snooze from exactly one of by (a
// duration from now) and until (RFC 3339).
func SnoozeTime(by, until string, now time.Time) (time.Time, error) {
	hasBy, hasUntil := by != "", until != ""
	switch {
	case hasBy == hasUntil:
		return time.Time{}, errors.New("supply exactly one of `by` or `until`")
	case hasBy:
		dur, err := reminders.ParseBy(by)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(dur).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return time.Time{}, errors.New("until must be an RFC3339 timestamp")
	}
	return t.UTC(), nil
}

// RemindSnooze moves a reminder of the caller later (action snooze;
// snoozeReminder), by or until, with an optional reason.
func RemindSnooze(ctx context.Context, d Deps, userID uuid.UUID, a Args) (reminders.Reminder, *Failure) {
	start := time.Now()
	id, f := reminderTarget(ctx, d, userID, a)
	if f != nil {
		return reminders.Reminder{}, f
	}
	newRemindAt, err := SnoozeTime(strings.TrimSpace(a.GetString("by", "")), strings.TrimSpace(a.GetString("until", "")), time.Now().UTC())
	if err != nil {
		return reminders.Reminder{}, Fail(ErrCodeValidationError, err.Error(), nil)
	}
	reason := strings.TrimSpace(a.GetString("reason", ""))
	rem, err := d.Reminders.Snooze(ctx, id, userID, newRemindAt, reason, RemindersAgentActor(ctx, d.Pool, userID))
	if err != nil {
		if errors.Is(err, reminders.ErrNotFound) {
			return reminders.Reminder{}, Fail(ErrCodeReminderNotFound, "reminder does not exist or is not yours", map[string]any{"reminderId": id.String()})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "tool", remindTool, "action", "snooze",
			"reason", "store snooze failed", "userId", userID.String(), "reminderId", id.String(), "error", err.Error())
		return reminders.Reminder{}, Fail(ErrCodeInternalError, "failed to snooze reminder", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"), "tool", remindTool, "action", "snooze",
		"userId", userID.String(), "reminderId", id.String(),
		"snoozeCount", rem.SnoozeCount, "latencyMs", time.Since(start).Milliseconds())
	return rem, nil
}

// RemindDismiss dismisses a reminder of the caller (action dismiss;
// dismissReminder), with an optional note.
func RemindDismiss(ctx context.Context, d Deps, userID uuid.UUID, a Args) (uuid.UUID, *Failure) {
	start := time.Now()
	id, f := reminderTarget(ctx, d, userID, a)
	if f != nil {
		return uuid.Nil, f
	}
	note := strings.TrimSpace(a.GetString("note", ""))
	if err := d.Reminders.Dismiss(ctx, id, userID, note, RemindersAgentActor(ctx, d.Pool, userID)); err != nil {
		if errors.Is(err, reminders.ErrNotFound) {
			return uuid.Nil, Fail(ErrCodeReminderNotFound, "reminder does not exist or is not yours", map[string]any{"reminderId": id.String()})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "tool", remindTool, "action", "dismiss",
			"reason", "store dismiss failed", "userId", userID.String(), "reminderId", id.String(), "error", err.Error())
		return uuid.Nil, Fail(ErrCodeInternalError, "failed to dismiss reminder", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"), "tool", remindTool, "action", "dismiss",
		"userId", userID.String(), "reminderId", id.String(), "latencyMs", time.Since(start).Milliseconds())
	return id, nil
}

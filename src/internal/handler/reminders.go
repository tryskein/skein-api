package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// RemindersHandler serves the sk-62.2 reminder ("alarm") REST surface.
// Every endpoint is caller-scoped: a reminder belongs to exactly one user
// and only that user (the owner) may read, snooze, or dismiss it. Creating
// a reminder is a per-issue action gated by project visibility; the other
// three operations key on the reminder id (owner scope folds "not yours"
// into 404).
type RemindersHandler struct {
	pool        *pgxpool.Pool
	store       *reminders.Store
	issuesStore *issues.Store
	projects    *projects.Store
}

// NewRemindersHandler wires the handler over the shared stores.
func NewRemindersHandler(
	pool *pgxpool.Pool,
	store *reminders.Store,
	issuesStore *issues.Store,
	projectsStore *projects.Store,
) *RemindersHandler {
	return &RemindersHandler{
		pool:        pool,
		store:       store,
		issuesStore: issuesStore,
		projects:    projectsStore,
	}
}

// createReminderRequest is the POST body for creating a reminder.
type createReminderRequest struct {
	// RemindAt is the RFC3339 instant the reminder should ring. Stored in
	// UTC. A past value is accepted — the next scheduler tick will ring it
	// immediately (useful for "remind me when I next look").
	RemindAt string `json:"remindAt"`
	// Note is an optional free-form reason recorded as last_reason.
	Note string `json:"note"`
}

// snoozeReminderRequest is the POST body for snoozing. Exactly one of By /
// Until must be supplied.
type snoozeReminderRequest struct {
	// By is a relative offset from NOW: "1h", "1d", "3d", "5d", "1w" (and
	// the general <n><unit> grammar, unit in h|d|w).
	By string `json:"by"`
	// Until is an absolute RFC3339 instant to snooze to. Mutually
	// exclusive with By.
	Until string `json:"until"`
	// Reason is an optional free-form note recorded as last_reason.
	Reason string `json:"reason"`
}

// dismissReminderRequest is the optional DELETE body.
type dismissReminderRequest struct {
	// Note is an optional free-form reason recorded on the
	// reminder.dismissed event.
	Note string `json:"note"`
}

// Create handles POST /api/v1/projects/{projectID}/issues/{issueID}/reminders.
//
// Algorithm:
//  1. Parse remindAt (RFC3339, stored UTC).
//  2. EnsureProjectVisible on the path project (404 for non-members).
//  3. ensureIssueInProject — the path issue must belong to the path project,
//     else 404 issue_not_found, indistinguishable from an id that never
//     existed and with no reminder row written (sk-194.6.50).
//  4. Insert a reminder owned by the caller; emit reminder.set.
//
// @Summary Create a reminder on an issue
// @Description Hangs a personal alarm on the issue for the calling user. When remindAt passes, the scheduler materialises a bell notification for the caller. Multiple reminders per issue are allowed. The reminder's state (armed vs ringing) is derived from remindAt — no status is stored. A past remindAt rings on the next scheduler tick.
// @Tags Reminders
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project handle (id / alias / name)"
// @Param issueID path string true "Issue ID (hierarchical, e.g. sk-62)"
// @Param body body createReminderRequest true "Reminder creation payload"
// @Success 201 {object} reminders.Reminder "Created reminder"
// @Failure 400 {object} map[string]any "invalid_json | invalid_remind_at"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "Issue not found or project not visible"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/projects/{projectID}/issues/{issueID}/reminders [post]
func (h *RemindersHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_id", "issueID is required", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}

	var req createReminderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	remindAt, err := time.Parse(time.RFC3339, req.RemindAt)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_remind_at",
			"remindAt must be an RFC3339 timestamp", nil)
		return
	}

	// Project-scoped REST contract: the route project must be visible to the
	// caller (404 for non-members, anti-leak) AND must actually own the issue.
	// A visible issue reached through the wrong project path is a 404 — same
	// rule the issues/comments handlers enforce (issue.go: dto.ProjectID != pid).
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Scoped predicate rather than GetIssue-then-compare (sk-194.6.50): a
	// foreign issue's full detail is never hydrated on the deny path, so
	// "no such issue" and "issue of another project" cost the same query
	// and answer identically. See handler.ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	rem, err := h.store.Create(ctx, issueID, c.UserID, remindAt, req.Note, reminders.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	})
	if err != nil {
		if errors.Is(err, reminders.ErrIssueNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "reminder create failed",
			"issueId", issueID, "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "reminder created",
		"userId", c.UserID.String(), "issueId", issueID,
		"reminderId", rem.ID.String(), "remindAt", rem.RemindAt.Format(time.RFC3339))
	writeJSON(w, http.StatusCreated, rem)
}

// Snooze handles POST /api/v1/reminders/{reminderID}/snooze.
//
// @Summary Snooze a reminder
// @Description Pushes a reminder's ring time forward. Supply exactly one of `by` (relative to now — 1h | 1d | 3d | 5d | 1w, or the general <n><unit> with unit h|d|w) or `until` (absolute RFC3339). Resets the notified flag so the reminder rings again at the new time, and increments snoozeCount. Owner-only — another user's reminder reads as 404.
// @Tags Reminders
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param reminderID path string true "Reminder UUID"
// @Param body body snoozeReminderRequest true "Snooze payload (exactly one of by / until)"
// @Success 200 {object} reminders.Reminder "Updated reminder"
// @Failure 400 {object} map[string]any "invalid_json | invalid_snooze (need exactly one of by/until) | invalid_duration | invalid_until"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "Reminder not found or not owned by caller"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/reminders/{reminderID}/snooze [post]
func (h *RemindersHandler) Snooze(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "reminderID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}

	var req snoozeReminderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	newRemindAt, derr := resolveSnoozeTarget(req.By, req.Until, time.Now().UTC())
	if derr != nil {
		writeSnoozeError(w, derr)
		return
	}

	rem, err := h.store.Snooze(ctx, id, c.UserID, newRemindAt, req.Reason, reminders.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	})
	if err != nil {
		if errors.Is(err, reminders.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "reminder_not_found", "reminder not found", nil)
			return
		}
		slog.ErrorContext(ctx, "reminder snooze failed",
			"reminderId", id.String(), "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "reminder snoozed",
		"userId", c.UserID.String(), "reminderId", id.String(),
		"remindAt", rem.RemindAt.Format(time.RFC3339), "snoozeCount", rem.SnoozeCount)
	writeJSON(w, http.StatusOK, rem)
}

// Dismiss handles DELETE /api/v1/reminders/{reminderID}.
//
// @Summary Dismiss (delete) a reminder
// @Description Permanently removes the reminder row (dismiss = delete; there is no soft-delete). Owner-only — another user's reminder reads as 404. Optional body {note} is recorded on the reminder.dismissed event.
// @Tags Reminders
// @Accept json
// @Security ApiKeyAuth
// @Param reminderID path string true "Reminder UUID"
// @Param body body dismissReminderRequest false "Optional dismiss note"
// @Success 204 "Dismissed"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "Reminder not found or not owned by caller"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/reminders/{reminderID} [delete]
func (h *RemindersHandler) Dismiss(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "reminderID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}

	// Body is optional — a DELETE with no body is the common path. Only
	// decode when something was sent.
	var note string
	if r.ContentLength != 0 {
		var req dismissReminderRequest
		if err := decodeJSON(r, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
			return
		}
		note = req.Note
	}

	if err := h.store.Dismiss(ctx, id, c.UserID, note, reminders.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	}); err != nil {
		if errors.Is(err, reminders.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "reminder_not_found", "reminder not found", nil)
			return
		}
		slog.ErrorContext(ctx, "reminder dismiss failed",
			"reminderId", id.String(), "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "reminder dismissed",
		"userId", c.UserID.String(), "reminderId", id.String())
	w.WriteHeader(http.StatusNoContent)
}

// List handles GET /api/v1/reminders?state=ringing|armed|all.
//
// @Summary List the caller's reminders + ringing badge count
// @Description Returns the caller's reminders across all projects, ordered most-urgent-first (ringing first by remind time ascending, then armed). The companion `ringingTaskCount` is the number of DISTINCT issues with at least one ringing reminder — the number the tab badge shows. `state` filters the list (ringing | armed | all, default all); the badge count is always the full ringing-task count regardless of the list filter.
// @Tags Reminders
// @Produce json
// @Security ApiKeyAuth
// @Param state query string false "Filter: ringing | armed | all (default all)"
// @Success 200 {object} reminders.ListResponse "Reminders + ringing badge count"
// @Failure 400 {object} map[string]any "invalid_state"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/reminders [get]
func (h *RemindersHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	filter, ok := reminders.ParseListFilter(r.URL.Query().Get("state"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid_state",
			"state must be one of ringing | armed | all", nil)
		return
	}

	resp, err := h.store.ListAndCount(ctx, c.UserID, filter)
	if err != nil {
		slog.ErrorContext(ctx, "reminder list failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// snoozeTargetError carries the specific validation failure so handlers can
// emit the right 400 code. Shared by the REST handler + the MCP tool.
type snoozeTargetError struct {
	Code    string
	Message string
}

func (e *snoozeTargetError) Error() string { return e.Message }

// resolveSnoozeTarget computes the new remind_at from exactly one of by /
// until. by is relative to `now` (now + duration); until is absolute. Both
// empty or both set is a validation error. Returns the resolved UTC instant.
func resolveSnoozeTarget(by, until string, now time.Time) (time.Time, *snoozeTargetError) {
	hasBy := by != ""
	hasUntil := until != ""
	switch {
	case hasBy == hasUntil:
		return time.Time{}, &snoozeTargetError{
			Code:    "invalid_snooze",
			Message: "supply exactly one of `by` or `until`",
		}
	case hasBy:
		d, err := reminders.ParseBy(by)
		if err != nil {
			return time.Time{}, &snoozeTargetError{
				Code:    "invalid_duration",
				Message: err.Error(),
			}
		}
		return now.Add(d).UTC(), nil
	default: // hasUntil
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return time.Time{}, &snoozeTargetError{
				Code:    "invalid_until",
				Message: "until must be an RFC3339 timestamp",
			}
		}
		return t.UTC(), nil
	}
}

// writeSnoozeError maps a snoozeTargetError to a 400 problem response.
func writeSnoozeError(w http.ResponseWriter, e *snoozeTargetError) {
	writeProblem(w, http.StatusBadRequest, e.Code, e.Message, nil)
}

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/push"
)

// isUniqueViolation reports whether err wraps a Postgres unique-violation
// error (SQLSTATE 23505). Used by the custom watcher CREATE handler to
// translate UNIQUE(user_id, name) collisions into a 422 duplicate_name
// response without leaking constraint internals to clients.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// errIssueNotFound is the local sentinel returned by lookupIssueProject
// when the path-supplied issue id doesn't exist. Caller maps to 404 to
// avoid a separate ErrNotFound import.
var errIssueNotFound = errors.New("issue not found")

// lookupIssueProject reads issues.project_id for the given issue id.
// Returns errIssueNotFound on no match. Used by /watch endpoints — the
// caller follows up with EnsureProjectVisible on the returned project.
func (h *NotificationsHandler) lookupIssueProject(ctx context.Context, issueID string) (uuid.UUID, error) {
	var pid uuid.UUID
	err := h.pool.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errIssueNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("looking up issue %s: %w", issueID, err)
	}
	return pid, nil
}

// NotificationsHandler hosts every R1.6 endpoint that touches watchers,
// notifications, or per-user notification preferences. Routes mounted
// under /api/v1; route-level RequireUser is composed in main.go.
type NotificationsHandler struct {
	pool  *pgxpool.Pool
	store *notifications.Store
}

// NewNotificationsHandler builds the handler with a fresh
// notifications.Store wrapping the supplied pool.
func NewNotificationsHandler(pool *pgxpool.Pool) *NotificationsHandler {
	return &NotificationsHandler{
		pool:  pool,
		store: notifications.NewStore(pool),
	}
}

// ----------------- Issue watchers -----------------

// WatchIssue handles POST /api/v1/issues/{issueID}/watch. The caller
// must be able to see the issue's project; we verify by reading the
// project_id off the issue and running EnsureProjectVisible.
//
// @Summary Watch an issue (manual subscription)
// @Description Subscribes the CALLER to one issue with reason `manual` (sk-622.21, US-1). Algorithm: authenticate -> look up the issue's project -> check the caller can see it -> in one SERIALIZABLE transaction (a serialization conflict or deadlock is replayed, up to 3 attempts in all; a conflict that outlives the attempts is answered 500 internal_error and nothing is written) delete the caller's opt-out on this issue, if any, and upsert the subscription row.
// @Description
// @Description Effects: an earlier explicit unwatch of this issue stops blocking the automatic subscriptions (creator, assignee, reopen) — watching by hand is the way to undo a refusal. Re-watching is idempotent. Issue scope only; it never narrows an existing subtree subscription (subtree watches are made through MCP `skein_watchers` watch with subtree=true). Notifications written after the subscription store this issue as their Watching card (the card is computed when a notification is written); notifications written before it are not re-carded. Today the Watching column and badge do not yet read the stored card: they attribute the caller's feed to the caller's CURRENT subscriptions by issue id, so older notifications of the issue show on its card too. The switch of the column and badge to the stored card is sk-622.21 T022.
// @Description
// @Description No notifications or events are produced. The issue joins the caller's Watching list.
// @Tags Notifications
// @Security ApiKeyAuth
// @Param issueID path string true "Full hierarchical issue id, e.g. sk-42 or sk-42.1"
// @Success 204 "Subscribed (or already subscribed)"
// @Failure 400 {object} map[string]any "missing_issue_id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the caller is not a member of the issue's project"
// @Failure 404 {object} map[string]any "issue_not_found / not_found — no such issue or project"
// @Failure 500 {object} map[string]any "internal_error — a DB fault, or a serialization conflict that outlived the 3 attempts"
// @Router /api/v1/issues/{issueID}/watch [post]
func (h *NotificationsHandler) WatchIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	pid, err := h.lookupIssueProject(ctx, issueID)
	if err != nil {
		if errors.Is(err, errIssueNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "watch issue: lookup failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if err := h.store.AddIssueWatch(ctx, issueID, c.UserID, false); err != nil {
		slog.ErrorContext(ctx, "watch issue failed",
			"issueId", issueID, "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnwatchIssue handles DELETE /api/v1/issues/{issueID}/watch — see
// notifications.RemoveIssueWatch.
//
// @Summary Stop watching an issue, or remove its historical Watching card
// @Description The CALLER's explicit "stop watching" of one issue (sk-622.21, US-1, US-6). Algorithm: authenticate -> look up the issue's project -> check the caller can see it -> in one SERIALIZABLE transaction (a serialization conflict or deadlock is replayed, up to 3 attempts in all; a conflict that outlives the attempts is answered 500 internal_error and nothing is written):
// @Description - if the caller is subscribed to the issue (any reasons, manual included): the subscription is deleted and an opt-out is recorded — the automatic paths (becoming the creator's errand, being assigned, the issue being reopened) will not subscribe the caller to this issue again; watching it by hand (POST) removes the opt-out;
// @Description - if the caller is not subscribed (a historical Watching card, e.g. left after auto-unsubscribe on close): nothing is recorded, only the card is removed.
// @Description In both cases every notification of the caller whose Watching card is this issue, archived ones included, loses the card: the card leaves the Watching column and the badge, the notifications stay in the inbox. A subtree subscription of an ANCESTOR is not affected — the opt-out is about this issue alone.
// @Description
// @Description Idempotent: a repeated call finds nothing and changes nothing. No notifications or events are produced.
// @Tags Notifications
// @Security ApiKeyAuth
// @Param issueID path string true "Full hierarchical issue id, e.g. sk-42 or sk-42.1"
// @Success 204 "Unsubscribed / card removed (or nothing to do)"
// @Failure 400 {object} map[string]any "missing_issue_id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the caller is not a member of the issue's project"
// @Failure 404 {object} map[string]any "issue_not_found / not_found — no such issue or project"
// @Failure 500 {object} map[string]any "internal_error — a DB fault, or a serialization conflict that outlived the 3 attempts"
// @Router /api/v1/issues/{issueID}/watch [delete]
func (h *NotificationsHandler) UnwatchIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	pid, err := h.lookupIssueProject(ctx, issueID)
	if err != nil {
		if errors.Is(err, errIssueNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "unwatch issue: lookup failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if err := h.store.RemoveIssueWatch(ctx, issueID, c.UserID); err != nil {
		slog.ErrorContext(ctx, "unwatch issue failed",
			"issueId", issueID, "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListIssueWatchers handles GET /api/v1/issues/{issueID}/watchers.
func (h *NotificationsHandler) ListIssueWatchers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	pid, err := h.lookupIssueProject(ctx, issueID)
	if err != nil {
		if errors.Is(err, errIssueNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "list issue watchers: lookup failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	out, err := h.store.ListIssueWatchers(ctx, issueID)
	if err != nil {
		slog.ErrorContext(ctx, "list issue watchers failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ----------------- Project watchers -----------------

// WatchProject handles POST /api/v1/projects/{projectID}/watch.
func (h *NotificationsHandler) WatchProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if err := h.store.AddProjectWatch(ctx, pid, c.UserID); err != nil {
		slog.ErrorContext(ctx, "watch project failed",
			"projectId", pid.String(), "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnwatchProject handles DELETE /api/v1/projects/{projectID}/watch.
func (h *NotificationsHandler) UnwatchProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if err := h.store.RemoveProjectWatch(ctx, pid, c.UserID); err != nil {
		slog.ErrorContext(ctx, "unwatch project failed",
			"projectId", pid.String(), "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListProjectWatchers handles GET /api/v1/projects/{projectID}/watchers.
func (h *NotificationsHandler) ListProjectWatchers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	out, err := h.store.ListProjectWatchers(ctx, pid)
	if err != nil {
		slog.ErrorContext(ctx, "list project watchers failed", "projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ----------------- Notifications inbox -----------------

// ListNotifications handles GET /api/v1/notifications.
//
// Query params:
//   - scope: 'unread' | 'all' | 'archived' | 'mentions' (default
//     'unread' for the primary inbox view). sk-303.13 / T017 removed
//     'outbox': the surface it fed is gone, so the slice is no longer
//     served and an explicit request for it is a 400 like any other
//     unknown scope.
//   - project: project uuid — narrow to one project
//   - cursor: opaque cursor from previous page
//   - limit: 1..100, default 50
//
// @Summary Read the caller's notification inbox
// @Description Returns one page of the caller's own notification rows — what happened on the issues they watch, were assigned, created or were mentioned in — newest first, with the cursor to fetch the next page. Business meaning: this is the feed the Inbox / Mentions surfaces render, and the only read of the `notifications` table a client gets.
// @Description
// @Description Algorithm: authenticate -> validate `scope` against the allowlist (unread | all | archived | mentions; anything else is 400, NOT a silent fallback) -> parse the optional `project`, `cursor` and `limit` -> run one query that filters the caller's rows by scope and hydrates actor, entity headline and the ready-to-render `summary` in the same statement (JOINs to events / users / issues / projects, so no per-row follow-up call) -> for `scope=mentions` a second pass fills `mentionContext` -> return 200 with the page.
// @Description
// @Description Scope is a filter over the caller's OWN rows, never a widening: `unread` = not read and not archived, `all` = everything not archived, `archived` = archived only, `mentions` = unread mention-typed rows. A row exists at all only because some channel of its reason was enabled for this recipient at write time, and the read additionally requires the feed channel — so push-only rows never appear here.
// @Description
// @Description Paging is by descending id with an opaque cursor: pass back `nextCursor` from the previous page as `?cursor=`. `hasMore` is the authority on whether another page exists; an empty `items` with `hasMore=false` is the end. `limit` outside [1,100] is a 400 rather than a clamp, so a client never silently gets a different page size than it asked for.
// @Description
// @Description Some fields of a row are monitor-only and never appear here: `actorSessionId`, `commentId`, `bodyPreview` and `truncated` are filled by the agent poll path alone (GET /agent/v1/notifications/poll). `closeOutcome` does appear, on rows reporting a transition into «closed» whose outcome is known.
// @Description
// @Description Retention: rows are kept NOTIFICATIONS_RETENTION_DAYS days (90 by default). A background sweep, at least once an hour, deletes a row that is not a mention and is older than that window, read or unread, archived or not — EXCEPT a row whose web push is still owed (push queued or being delivered): it is kept, and returned by this endpoint like any other row, until the delivery is over. A mention row is never deleted, whatever its age.
// @Description
// @Description Side effects: none. Reading the inbox does NOT mark anything read — that is POST /api/v1/notifications/read.
// @Tags Notifications
// @Produce json
// @Security ApiKeyAuth
// @Param scope query string false "unread (default) | all | archived | mentions"
// @Param project query string false "Project UUID — narrow the page to one project"
// @Param cursor query string false "Opaque cursor from the previous page's nextCursor"
// @Param limit query int false "Page size, 1..100 (default 50)"
// @Success 200 {object} notifications.NotificationListResult "One page of the caller's inbox"
// @Failure 400 {object} map[string]any "invalid_scope | invalid_project | invalid_cursor | invalid_limit"
// @Failure 401 {object} map[string]any "unauthenticated — cookie or api key missing or malformed"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/notifications [get]
func (h *NotificationsHandler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()
	args := notifications.ListInboxArgs{UserID: c.UserID}

	scope := q.Get("scope")
	if scope == "" {
		scope = "unread"
	}
	// sk-37 — explicit allowlist so a typo returns 400 instead of bubbling
	// up as a 500 from the store's `invalid scope %q` error. Previously
	// the handler trusted the store layer to validate, which surfaced as
	// an opaque server error to clients.
	//
	// sk-38 — adds 'mentions' for the shell's Mentions tab (unread
	// comment.mention + issue.mention rows).
	switch scope {
	case "unread", "all", "archived", "mentions":
		// ok
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: unread, all, archived, mentions", nil)
		return
	}
	args.Scope = scope

	if raw := q.Get("project"); raw != "" {
		pid, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_project",
				"project must be a UUID", nil)
			return
		}
		args.ProjectID = &pid
	}

	if raw := q.Get("cursor"); raw != "" {
		cursorID, err := notifications.DecodeCursor(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", err.Error(), nil)
			return
		}
		args.Cursor = &cursorID
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be in [1, 100]", nil)
			return
		}
		args.Limit = n
	}

	res, err := h.store.ListInbox(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "list notifications failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// CountNotifications handles GET /api/v1/notifications/count.
//
// Query params:
//   - project: optional project uuid — returns the per-project count
//     (used by the per-project NotificationBadge). Omit for a global
//     count (header NotificationBadge).
func (h *NotificationsHandler) CountNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var projectID *uuid.UUID
	if raw := r.URL.Query().Get("project"); raw != "" {
		pid, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_project",
				"project must be a UUID", nil)
			return
		}
		projectID = &pid
	}
	out, err := h.store.Counts(ctx, c.UserID, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "count notifications failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// MarkRead handles POST /api/v1/notifications/{id}/read.
func (h *NotificationsHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	h.runIDMutator(w, r, "mark_read")
}

// MarkUnread handles POST /api/v1/notifications/{id}/unread.
func (h *NotificationsHandler) MarkUnread(w http.ResponseWriter, r *http.Request) {
	h.runIDMutator(w, r, "mark_unread")
}

// Archive handles POST /api/v1/notifications/{id}/archive.
func (h *NotificationsHandler) Archive(w http.ResponseWriter, r *http.Request) {
	h.runIDMutator(w, r, "archive")
}

// runIDMutator parses the {id} path param, checks auth, and dispatches
// to the right store method by op name. Centralises auth + error
// mapping so the three thin wrappers above stay one-liners.
func (h *NotificationsHandler) runIDMutator(
	w http.ResponseWriter, r *http.Request,
	opName string,
) {
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_id",
			"notification id must be a positive integer", nil)
		return
	}

	var execErr error
	switch opName {
	case "mark_read":
		execErr = h.store.MarkRead(r.Context(), id, c.UserID)
	case "mark_unread":
		execErr = h.store.MarkUnread(r.Context(), id, c.UserID)
	case "archive":
		execErr = h.store.Archive(r.Context(), id, c.UserID)
	}

	if execErr != nil {
		if errors.Is(execErr, notifications.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "notification_not_found",
				"notification not found", nil)
			return
		}
		slog.ErrorContext(r.Context(), "notification mutate failed",
			"op", opName, "userId", c.UserID.String(), "error", execErr.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MarkAllRead handles POST /api/v1/notifications/mark-all-read.
//
// Body: { scope?: string, project?: uuid }
type markAllReadRequest struct {
	Scope     string  `json:"scope,omitempty"`
	ProjectID *string `json:"project,omitempty"`
}

type markAllReadResponse struct {
	Updated int64 `json:"updated"`
}

func (h *NotificationsHandler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req markAllReadRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
			return
		}
	}
	// scope is reserved for forward-compat but mark-all-read in R1.6
	// always operates on unread non-archived rows. Rejecting unknown
	// values prevents silent no-ops where a client expects e.g. "archived"
	// to mean "mark every archived row as read".
	switch req.Scope {
	case "", "unread":
		// ok
	default:
		writeProblem(w, http.StatusBadRequest, "scope_not_supported",
			"scope must be omitted or 'unread' (only unread rows can be marked)", nil)
		return
	}
	args := notifications.MarkAllReadArgs{
		UserID: c.UserID,
		Scope:  req.Scope,
	}
	if req.ProjectID != nil && *req.ProjectID != "" {
		pid, err := uuid.Parse(*req.ProjectID)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_project",
				"project must be a UUID", nil)
			return
		}
		args.ProjectID = &pid
	}
	n, err := h.store.MarkAllRead(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "mark all read failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, markAllReadResponse{Updated: n})
}

// MarkReadByEntity handles POST /api/v1/notifications/mark-read-by-entity.
//
// Body: { entityType: 'issue', entityId: 'demo-42' }
type markReadByEntityRequest struct {
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
}

func (h *NotificationsHandler) MarkReadByEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req markReadByEntityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.EntityType == "" || req.EntityID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_fields",
			"entityType and entityId are required", nil)
		return
	}
	switch req.EntityType {
	case "issue", "comment", "project", "component":
		// ok — closed set of entity types we support in R1.6
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_entity_type",
			"entityType must be one of: issue, comment, project, component", nil)
		return
	}
	n, err := h.store.MarkReadByEntity(ctx, c.UserID, req.EntityType, req.EntityID)
	if err != nil {
		slog.ErrorContext(ctx, "mark read by entity failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, markAllReadResponse{Updated: n})
}

// ----------------- User notification prefs -----------------

// GetPrefs handles GET /api/v1/me/notification-prefs.
func (h *NotificationsHandler) GetPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.GetPrefs(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get prefs failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// updatePrefsRequest distinguishes "field omitted" vs "field set to
// null" via a parallel `json.RawMessage` decode for notifyEventTypes.
// Without this, `null` and missing both decode as nil and the user
// loses the ability to reset the filter back to "all events".
type updatePrefsRequest struct {
	NotifyEventTypes     json.RawMessage `json:"notifyEventTypes,omitempty"`
	DefaultInboxScope    *string         `json:"defaultInboxScope,omitempty"`
	SuppressAgentActions *bool           `json:"suppressAgentActions,omitempty"`
}

// ----------------- R3.3 — consolidated /me/watchers + custom watchers -----------------

// GetMyWatchers handles GET /api/v1/me/watchers — returns the
// consolidated 3-section view (issues, projects, custom) used by the
// /watching page.
func (h *NotificationsHandler) GetMyWatchers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	out, err := h.store.ListAllWatchers(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "list all watchers failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListMyCustomWatchers handles GET /api/v1/me/custom-watchers.
func (h *NotificationsHandler) ListMyCustomWatchers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	out, err := h.store.ListCustomWatchers(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "list custom watchers failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createCustomWatcherRequest is the body shape for POST
// /api/v1/me/custom-watchers. The handler validates filter fields against
// the closed event-type set + UUID parsing for project / component ids.
type createCustomWatcherRequest struct {
	Name   string                            `json:"name"`
	Filter notifications.CustomWatcherFilter `json:"filter"`
}

// CreateCustomWatcher handles POST /api/v1/me/custom-watchers.
//
// 422 too_many_watchers — caller has hit the per-user cap of 10.
// 422 duplicate_name      — name already used by the caller.
// 400 validation_error    — filter has unknown event type / bad uuid.
func (h *NotificationsHandler) CreateCustomWatcher(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req createCustomWatcherRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "validation_error", "name is required", nil)
		return
	}
	if len(req.Name) > 100 {
		writeProblem(w, http.StatusBadRequest, "validation_error", "name must be 100 characters or fewer", nil)
		return
	}
	// Validate filter event types against the subscription catalogue
	// (T001 / sk-303.13): only types that actually deliver to a subscriber
	// may be stored, so a filter can never become a toggle without effect.
	// A well-formed request naming an unknown or undeliverable type is a
	// BUSINESS error — 422, not 400 (the project guidelines).
	for _, t := range req.Filter.EventTypes {
		if !notifications.IsSubscriptionEventType(t) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_event_type",
				"unknown or undeliverable event type: "+t, nil)
			return
		}
	}
	// Validate UUIDs in projects / components — reject early instead of
	// storing junk that the SQL @> never matches.
	for _, p := range req.Filter.Projects {
		if _, err := uuid.Parse(p); err != nil {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"projects must be UUIDs: "+p, nil)
			return
		}
	}
	for _, comp := range req.Filter.Components {
		if _, err := uuid.Parse(comp); err != nil {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"components must be UUIDs: "+comp, nil)
			return
		}
	}
	if req.Filter.PriorityMax != nil && (*req.Filter.PriorityMax < 0 || *req.Filter.PriorityMax > 1000) {
		writeProblem(w, http.StatusBadRequest, "validation_error",
			"priority_max must be in [0, 1000]", nil)
		return
	}

	dto, err := h.store.AddCustomWatcher(ctx, c.UserID, notifications.CreateCustomWatcherInput{
		Name:   req.Name,
		Filter: req.Filter,
	})
	if err != nil {
		if errors.Is(err, notifications.ErrTooManyWatchers) {
			writeProblem(w, http.StatusUnprocessableEntity, "too_many_watchers",
				fmt.Sprintf("at most %d custom watchers per user", notifications.MaxCustomWatchersPerUser), nil)
			return
		}
		// Unique violation on (user_id, name) → 422 duplicate_name.
		if isUniqueViolation(err) {
			writeProblem(w, http.StatusUnprocessableEntity, "duplicate_name",
				"a custom watcher with this name already exists", nil)
			return
		}
		slog.ErrorContext(ctx, "create custom watcher failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

// DeleteCustomWatcher handles DELETE /api/v1/me/custom-watchers/{id}.
func (h *NotificationsHandler) DeleteCustomWatcher(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if err := h.store.DeleteCustomWatcher(ctx, c.UserID, id); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "watcher_not_found",
				"custom watcher not found", nil)
			return
		}
		slog.ErrorContext(ctx, "delete custom watcher failed",
			"userId", c.UserID.String(), "watcherId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// patchProjectWatcherRequest is the body shape for PATCH
// /api/v1/projects/{projectID}/watch — used to update an existing
// project_watcher's event_types whitelist. The watcher must already
// exist; callers get 404 watcher_not_found otherwise (POST /watch first).
type patchProjectWatcherRequest struct {
	// EventTypes is the strict whitelist of event types the watcher
	// wants to receive. nil / omitted → no change. Empty array clears
	// the filter (subscribe to all notifiable types).
	EventTypes *[]string `json:"eventTypes,omitempty"`
}

// PatchProjectWatcher handles PATCH /api/v1/projects/{projectID}/watch.
func (h *NotificationsHandler) PatchProjectWatcher(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	var req patchProjectWatcherRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	if req.EventTypes != nil {
		// Validate every entry against the closed set so the SQL filter
		// stays well-defined. An empty array is valid (clears the filter).
		for _, t := range *req.EventTypes {
			if !notifications.IsSubscriptionEventType(t) {
				writeProblem(w, http.StatusUnprocessableEntity, "invalid_event_type",
					"unknown or undeliverable event type: "+t, nil)
				return
			}
		}
		if err := h.store.SetProjectWatcherEventTypes(ctx, pid, c.UserID, *req.EventTypes); err != nil {
			if errors.Is(err, notifications.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "watcher_not_found",
					"caller is not currently watching this project", nil)
				return
			}
			slog.ErrorContext(ctx, "patch project watcher failed",
				"projectId", pid.String(), "userId", c.UserID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutPrefs handles PUT /api/v1/me/notification-prefs.
func (h *NotificationsHandler) PutPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req updatePrefsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.DefaultInboxScope != nil {
		switch *req.DefaultInboxScope {
		case "all", "watched_only", "assigned_only":
			// ok
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_scope",
				"defaultInboxScope must be one of: all, watched_only, assigned_only", nil)
			return
		}
	}
	input := notifications.UpdatePrefsInput{
		DefaultInboxScope:    req.DefaultInboxScope,
		SuppressAgentActions: req.SuppressAgentActions,
	}
	// Three cases for notifyEventTypes:
	//   - field absent      → req.NotifyEventTypes == nil      → leave alone
	//   - field is null     → req.NotifyEventTypes == "null"   → SetEventTypesNull
	//   - field is array    → unmarshal into []string          → whitelist
	if len(req.NotifyEventTypes) > 0 {
		trimmed := bytes.TrimSpace(req.NotifyEventTypes)
		if string(trimmed) == "null" {
			input.SetEventTypesNull = true
		} else {
			var list []string
			if err := json.Unmarshal(req.NotifyEventTypes, &list); err != nil {
				writeProblem(w, http.StatusBadRequest, "invalid_event_types",
					"notifyEventTypes must be null or an array of strings", nil)
				return
			}
			// Validate against the subscription catalogue (T001 /
			// sk-303.13): only event types with a real delivery path may be
			// whitelisted. The payload-aware notifiability of issue.updated
			// still fires per-row inside FanOut, not here. A malformed body
			// was already answered with 400 above; a well-formed body naming
			// an unknown or undeliverable type is a business error → 422.
			for _, t := range list {
				if !notifications.IsSubscriptionEventType(t) {
					writeProblem(w, http.StatusUnprocessableEntity, "invalid_event_type",
						"unknown or undeliverable event type: "+t, nil)
					return
				}
			}
			input.NotifyEventTypes = &list
		}
	}
	dto, err := h.store.UpdatePrefs(ctx, c.UserID, input)
	if err != nil {
		slog.ErrorContext(ctx, "update prefs failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// ----------------- Web Push prefs (sk-62.3.2) -----------------

// pushPrefsResponse is the GET /me/push-prefs payload: the caller's opted-out
// categories plus the full registry (key + RU label) so the UI can render a
// toggle per known category without hardcoding the list.
type pushPrefsResponse struct {
	// DisabledCategories is the set of push categories the caller has opted
	// out of. Always a JSON array (never null) — empty means every category
	// is delivered.
	DisabledCategories []string `json:"disabledCategories"`
	// Categories is the full push-category registry (display order preserved).
	Categories []push.Category `json:"categories"`
}

// putPushPrefsRequest is the PUT /me/push-prefs body — the full desired set of
// disabled categories (replace semantics, not a delta).
type putPushPrefsRequest struct {
	DisabledCategories []string `json:"disabledCategories"`
}

// GetPushPrefs handles GET /api/v1/me/push-prefs.
//
// @Summary Get the caller's web-push category preferences
// @Description Returns the caller's opted-out push categories plus the full registry of known categories (key + RU label). An empty disabledCategories array means every category is pushed. Push categories are the user-facing knob for web-push delivery — distinct from the bell/notify event-type filter.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} pushPrefsResponse "Disabled categories + registry"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/push-prefs [get]
func (h *NotificationsHandler) GetPushPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	disabled, err := h.store.GetPushDisabledCategories(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get push prefs failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, pushPrefsResponse{
		DisabledCategories: disabled,
		Categories:         push.Categories,
	})
}

// PutPushPrefs handles PUT /api/v1/me/push-prefs.
//
// @Summary Replace the caller's web-push category opt-outs
// @Description Persists the full set of push categories the caller opts OUT of (replace semantics). Every entry is validated against the category registry — an unknown category is rejected with 422 and nothing is written. push-only: disabling a category suppresses its web-push delivery but never the in-app bell notification.
// @Tags Push
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putPushPrefsRequest true "Desired disabled categories"
// @Success 200 {object} pushPrefsResponse "Updated prefs + registry"
// @Failure 400 {object} map[string]any "invalid_json"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "unknown_category"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/push-prefs [put]
func (h *NotificationsHandler) PutPushPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req putPushPrefsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	// Validate + dedup against the registry. Unknown → 422 (business error),
	// nothing persisted.
	seen := make(map[string]struct{}, len(req.DisabledCategories))
	cleaned := make([]string, 0, len(req.DisabledCategories))
	for _, cat := range req.DisabledCategories {
		if !push.IsKnownCategory(cat) {
			writeProblem(w, http.StatusUnprocessableEntity, "unknown_category",
				"unknown push category: "+cat, nil)
			return
		}
		if _, dup := seen[cat]; dup {
			continue
		}
		seen[cat] = struct{}{}
		cleaned = append(cleaned, cat)
	}
	if err := h.store.SetPushDisabledCategories(ctx, c.UserID, cleaned); err != nil {
		slog.ErrorContext(ctx, "set push prefs failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, pushPrefsResponse{
		DisabledCategories: cleaned,
		Categories:         push.Categories,
	})
}

// ----------------- sk-303.13 / C1 — reason registry and channel settings -----------------

// putReasonPrefsRequest is the PUT /me/notification-reasons body.
//
// The update is PARTIAL by contract: a client sends only the rows it touched,
// and every reason it did not name keeps its stored state. A full-replace body
// would make a form built against an older registry silently reset the
// preferences it cannot render.
type putReasonPrefsRequest struct {
	// Reasons lists the desired state of the named reasons. Omitted channels
	// keep their stored value, so {"reason":"comment","feed":true} switches the
	// feed on without touching the push side.
	Reasons []struct {
		Reason string `json:"reason"`
		Feed   *bool  `json:"feed,omitempty"`
		Push   *bool  `json:"push,omitempty"`
	} `json:"reasons"`
	// SuppressAgentActions lives on the page beside the reason table, not
	// inside it: it is not a reason but a cross-cutting filter of the
	// subscription path (sk-335 — any agent's actions, never a personal call).
	SuppressAgentActions *bool `json:"suppressAgentActions,omitempty"`
	// UnwatchOnClose — sk-551.10: when an issue is closed, drop the
	// caller's automatic subscriptions on it (creator / assignee); manual
	// ones stay; a reopen brings the automatic ones back. Switching it on
	// also applies it to issues already closed (sk-622.21, R23); switching
	// it off gives nothing back. Omitted = keep.
	UnwatchOnClose *bool `json:"unwatchOnClose,omitempty"`
}

// GetReasonPrefs handles GET /api/v1/me/notification-reasons.
//
// @Summary Get the caller's notification settings — one row per reason, two channels
// @Description Returns the server-side reason registry with the caller's channel states. One row = one reason ("Issue assignment", "Comment", …) with two INDEPENDENT channels: feed and push. A channel carries `applicable` separately from `enabled`: applicable=false means the reason has no such channel at all (the form draws a dash with an explanation and the user cannot switch it on), which is a different statement from "exists but is off". Each channel also names its `audience`, because the two channels of one reason legitimately reach different people — the feed of "Delegated-issue status" goes to every subscriber, its push only to the delegating creator. Reserved reasons (the ones whose mechanics arrive in later waves) are NOT returned: a toggle without an effect is the defect this contract removes. This endpoint replaces the previous notify-event-type and push-category settings contracts. Two flags ride beside the table: `suppressAgentActions` and `unwatchOnClose` (sk-551.10 — when an issue is closed, the caller's automatic creator/assignee subscriptions on it are dropped, manual ones stay, a reopen restores them; on by default — a user who never saved the setting has it on, sk-622.21 US-2).
// @Tags Notifications
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} notifications.ReasonPrefsDTO "Active reasons with the caller's channel states"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/notification-reasons [get]
func (h *NotificationsHandler) GetReasonPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.GetReasonPrefs(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get reason prefs failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// PutReasonPrefs handles PUT /api/v1/me/notification-reasons.
//
// @Summary Update the caller's notification settings (partial, per reason and channel)
// @Description Applies a PARTIAL update: only the reasons named in the body change, and within them only the channels named. Everything else keeps its stored state — a client that does not know a newer reason must not be able to reset it. Errors follow the house rule: a malformed body is 400, a well-formed body carrying a business mistake is 422. Three business mistakes exist here — a reason outside the closed registry (`unknown_reason`), a reason the registry reserves for a later wave and whose mechanics do not exist yet (`reason_not_settable`, the same reason GET does not return), and switching ON a channel the reason does not have (`channel_not_applicable`); in all three cases NOTHING is written, because a half-applied settings change leaves the user in a state they never asked for. `suppressAgentActions` and `unwatchOnClose` are optional booleans beside the table; an omitted flag keeps its stored value, and the whole update is one transaction. Turning `unwatchOnClose` on (from off) also applies it to issues that are ALREADY closed, in the same transaction: on every closed issue the caller's automatic subscription reasons (creator, assignee) are removed — a subscription made only of them is deleted, one that also carries a manual watch keeps the manual one; no opt-out is written, no Watching card is cleared, and a reopen gives the automatic subscription back. Turning it off gives nothing back. Re-sending the value already in effect changes nothing. Returns the same payload as GET, so the form can render the result without a second call.
// @Tags Notifications
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putReasonPrefsRequest true "Reasons to change (partial) + optional agent-action suppression and unwatch-on-close flags"
// @Success 200 {object} notifications.ReasonPrefsDTO "Updated settings"
// @Failure 400 {object} map[string]any "invalid_json / validation_error"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "unknown_reason / reason_not_settable / channel_not_applicable"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/notification-reasons [put]
func (h *NotificationsHandler) PutReasonPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req putReasonPrefsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	input := notifications.UpdateReasonPrefsInput{
		SuppressAgentActions: req.SuppressAgentActions,
		UnwatchOnClose:       req.UnwatchOnClose,
	}
	for _, row := range req.Reasons {
		if row.Reason == "" {
			// An empty required field is a malformed request, not a business
			// decision about a named reason.
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"every entry of reasons must carry a non-empty reason", nil)
			return
		}
		input.Reasons = append(input.Reasons, notifications.ReasonChannelUpdate{
			Reason: row.Reason,
			Feed:   row.Feed,
			Push:   row.Push,
		})
	}

	dto, err := h.store.UpdateReasonPrefs(ctx, c.UserID, input)
	if err != nil {
		switch {
		case errors.Is(err, notifications.ErrUnknownReason):
			writeProblem(w, http.StatusUnprocessableEntity, "unknown_reason", err.Error(), nil)
		case errors.Is(err, notifications.ErrReasonNotSettable):
			writeProblem(w, http.StatusUnprocessableEntity, "reason_not_settable", err.Error(), nil)
		case errors.Is(err, notifications.ErrChannelNotApplicable):
			writeProblem(w, http.StatusUnprocessableEntity, "channel_not_applicable", err.Error(), nil)
		default:
			slog.ErrorContext(ctx, "update reason prefs failed",
				"userId", c.UserID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// subscriptionEventTypesResponse is the GET /notification-event-types payload.
type subscriptionEventTypesResponse struct {
	// EventTypes is the catalogue a subscription filter may choose from.
	EventTypes []notifications.SubscriptionEventType `json:"eventTypes"`
}

// GetSubscriptionEventTypes handles GET /api/v1/notification-event-types.
//
// @Summary List the event types a subscription filter may whitelist
// @Description Subscription filters (the event-type whitelist on a project subscription and on custom subscriptions) keep speaking the language of event types: they are a property of the SUBSCRIPTION, and they multiply with the per-reason settings rather than replacing them — a notification is created when the reason is enabled AND the subscription filter lets the event through. This endpoint is that catalogue, and it lists ONLY types that actually deliver to a subscriber: a type with no delivery would be a toggle without an effect on yet another surface. Personally addressed reasons (mention, reminder) are deliberately absent — they reach their recipient regardless of any subscription, so filtering them would promise something the system does not do. Each entry also names the reason it belongs to, so the filter UI can group by the same vocabulary the settings form uses. The catalogue is identical for every caller and carries no private data.
// @Tags Notifications
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} subscriptionEventTypesResponse "Deliverable subscription event types"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Router /api/v1/notification-event-types [get]
func (h *NotificationsHandler) GetSubscriptionEventTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(r); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, subscriptionEventTypesResponse{
		EventTypes: notifications.SubscriptionEventTypes(),
	})
}

package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/redact"
)

// ActivityHandler hosts the R3.4 activity-feed endpoints:
//
//   - GET /api/v1/projects/{projectID}/activity — per-project feed,
//     gated by EnsureProjectVisible.
//   - GET /api/v1/me/activity                  — cross-project feed
//     restricted to the caller's visible project set (admin sees all).
//
// Both endpoints return ActivityListResult (items + opaque
// nextCursor). Cursor encoding mirrors notifications: base64url over
// the int64 events.id.
type ActivityHandler struct {
	pool        *pgxpool.Pool
	issuesStore *issues.Store
}

// NewActivityHandler wires the handler. issuesStore exposes the new
// ListProjectActivity / ListUserActivity methods; the caller's visible
// set comes from authz.
func NewActivityHandler(pool *pgxpool.Pool, issuesStore *issues.Store) *ActivityHandler {
	return &ActivityHandler{
		pool:        pool,
		issuesStore: issuesStore,
	}
}

// activityCursorEncode wraps an events.id in a base64url string.
// Mirrors notifications.encodeIDCursor — kept local to avoid pulling
// the notifications package into the handler just for this helper.
func activityCursorEncode(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

// activityCursorDecode parses a wire cursor back into the bigint id.
// Returns a friendly error suitable for a 400 response on malformed
// input.
func activityCursorDecode(s string) (int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	id, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor payload: %w", err)
	}
	if id <= 0 {
		return 0, errors.New("invalid cursor payload: must be positive")
	}
	return id, nil
}

// parseActivityArgs reads the shared ?limit= / ?cursor= query params
// and validates them. limit must fall in [1, 100]; cursor (when
// supplied) decodes via activityCursorDecode. The returned (limit,
// cursor) pair is fed straight to the store.
func parseActivityArgs(r *http.Request) (int, *int64, error) {
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return 0, nil, fmt.Errorf("limit must be in [1, 100]")
		}
		limit = n
	}
	var cursor *int64
	if raw := q.Get("cursor"); raw != "" {
		id, err := activityCursorDecode(raw)
		if err != nil {
			return 0, nil, err
		}
		cursor = &id
	}
	return limit, cursor, nil
}

// writeActivityResult emits the ActivityListResult JSON, ensuring an
// empty items slice serialises as `[]` (not null) and rewriting the
// raw int64 cursor as the opaque base64url wire form.
func writeActivityResult(w http.ResponseWriter, items []issues.ActivityEntry, nextCursor *int64) {
	type wireResult struct {
		Items      []issues.ActivityEntry `json:"items"`
		NextCursor *string                `json:"nextCursor"`
	}
	if items == nil {
		items = []issues.ActivityEntry{}
	}
	res := wireResult{Items: items}
	if nextCursor != nil {
		c := activityCursorEncode(*nextCursor)
		res.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, res)
}

// ListProjectActivity handles GET /api/v1/projects/{projectID}/activity.
//
// Query params:
//   - limit:  1..100 (default 50)
//   - cursor: opaque cursor returned in nextCursor of the previous page
//
// Visibility: same gate as every other /projects/{id}/* endpoint —
// EnsureProjectVisible returns 404 (not 403) for non-members so we
// don't leak project existence.
func (h *ActivityHandler) ListProjectActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_project_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "activity: project visibility check failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	limit, cursor, err := parseActivityArgs(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_query", err.Error(), nil)
		return
	}

	items, next, err := h.issuesStore.ListProjectActivity(ctx, pid, nil, cursor, limit)
	if err != nil {
		slog.ErrorContext(ctx, "list project activity failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeActivityResult(w, items, next)
}

// ListIssueActivity handles GET /api/v1/projects/{projectID}/issues/{issueID}/activity.
//
// Per-issue activity feed: same shape as the project feed, but scoped to
// events whose entity is the issue itself, a comment on the issue, or a
// dependency edge involving the issue (see issues.Store.ListIssueActivity
// for the predicate). Replaces the F5 stop-gap where the frontend pulled
// the project's top-100 events and filtered client-side; that broke for
// busy projects whose issue's events fell off the first page.
//
// Query params:
//   - limit:  1..100 (default 50)
//   - cursor: opaque cursor returned in nextCursor of the previous page
//
// Visibility: same gate as every other /projects/{id}/* endpoint —
// EnsureProjectVisible returns 404 (not 403) for non-members. We then
// resolve the issue and confirm it belongs to projectID — a cross-
// project URL pretends the issue doesn't exist (matches IssuesHandler.Get
// at handler/issues.go:610).
func (h *ActivityHandler) ListIssueActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_project_id", err.Error(), nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "issue activity: project visibility check failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	// Resolve the issue to verify (a) it exists and (b) belongs to this
	// project. Mirrors IssuesHandler.Get's anti-leak pattern: a stranger
	// who knows a foreign issue's id should not learn it exists by
	// guessing the URL under their own project.
	dto, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "issue activity: get issue failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if dto.ProjectID != pid {
		writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
		return
	}

	limit, cursor, err := parseActivityArgs(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_query", err.Error(), nil)
		return
	}

	items, next, err := h.issuesStore.ListIssueActivity(ctx, pid, issueID, nil, cursor, limit)
	if err != nil {
		slog.ErrorContext(ctx, "list issue activity failed",
			"projectId", pid.String(), "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeActivityResult(w, items, next)
}

// ListMyActivity handles GET /api/v1/me/activity. Caller-scoped: the
// rows are cut to the caller's visible set of the access relation
// (authz.VisibleProjectIDs) — an instance admin included, who sees what
// the relation grants him, not every project (sk-674.18 T017). Only
// declared internal work (authz.WithUnrestricted) reads without the
// filter; that is what the second return value of VisibleProjectIDs
// means. An empty visible set returns `{items:[], nextCursor:null}` —
// same shape as a populated empty page.
func (h *ActivityHandler) ListMyActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	visible, isAdmin, err := authz.VisibleProjectIDs(ctx, h.pool, authz.FromContext(ctx))
	if err != nil {
		slog.ErrorContext(ctx, "load visible projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	limit, cursor, err := parseActivityArgs(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_query", err.Error(), nil)
		return
	}

	// /me/activity is scoped, not global: globalScope=false CUTS rows to
	// the caller's visible set, and redact.None means no row is redacted
	// (there are no invisible rows to strip). Behaviour is unchanged.
	items, next, err := h.issuesStore.ListUserActivity(
		ctx, c.UserID, visible, isAdmin, nil, cursor, limit, nil /*reach*/, redact.None)
	if err != nil {
		slog.ErrorContext(ctx, "list user activity failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeActivityResult(w, items, next)
}

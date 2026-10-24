package handler

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// ReadyHandler exposes GET /api/v1/projects/{projectID}/ready.
type ReadyHandler struct {
	pool      *pgxpool.Pool
	depsStore *dependencies.Store
}

// NewReadyHandler returns a handler that shares the supplied dependencies
// store. The same Store instance is reused by DependenciesHandler so the
// pool is opened once.
func NewReadyHandler(pool *pgxpool.Pool, depsStore *dependencies.Store) *ReadyHandler {
	return &ReadyHandler{pool: pool, depsStore: depsStore}
}

// Ready handles GET /api/v1/projects/{projectID}/ready.
//
// Query params:
//   - limit:    1..100, default 50
//   - assignee: exact match
//   - priority: upper bound (≤ N), 0..4
//   - type:     one of bug | feature | task | epic | chore | decision | idea | note
//
// Permission: caller must see the project (route group already enforces
// RequireUser; we add EnsureProjectVisible here to surface 404 on the
// "exists-but-hidden" branch instead of leaking the project's existence).
//
// Algorithm: see dependencies.Store.ListReady — vision §6 / db-schema §4.3.
func (h *ReadyHandler) Ready(w http.ResponseWriter, r *http.Request) {
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

	q := r.URL.Query()
	filters := dependencies.ReadyFilters{}
	if v := q.Get("assignee"); v != "" {
		filters.Assignee = &v
	}
	if v := q.Get("priority"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_priority",
				"priority must be an integer", nil)
			return
		}
		// Reuse the issues package validator so we share the 0..4 range
		// with the Issues endpoints.
		if err := issues.ValidatePriority(p); err != nil {
			if ve, ok := issues.IsValidationError(err); ok {
				writeProblem(w, http.StatusBadRequest, ve.Code, ve.Message, nil)
				return
			}
			writeProblem(w, http.StatusBadRequest, "invalid_priority", err.Error(), nil)
			return
		}
		filters.Priority = &p
	}
	if v := q.Get("type"); v != "" {
		canon, err := issues.ValidateType(v)
		if err != nil {
			if ve, ok := issues.IsValidationError(err); ok {
				writeProblem(w, http.StatusBadRequest, ve.Code, ve.Message, nil)
				return
			}
			writeProblem(w, http.StatusBadRequest, "invalid_type", err.Error(), nil)
			return
		}
		filters.Type = &canon
	}

	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be in [1, 100]", nil)
			return
		}
		limit = n
	}

	out, err := h.depsStore.ListReady(ctx, pid, filters, limit)
	if err != nil {
		slog.ErrorContext(ctx, "ready query failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

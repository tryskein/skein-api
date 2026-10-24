package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/projects"
)

// ComponentsHandler is the per-project components endpoint.
type ComponentsHandler struct {
	pool  *pgxpool.Pool
	store *projects.Store
}

func NewComponentsHandler(pool *pgxpool.Pool) *ComponentsHandler {
	return &ComponentsHandler{pool: pool, store: projects.NewStore(pool)}
}

var _ repoLister = (*projects.Store)(nil)

// componentPathRequest mirrors projects.ComponentPath but uses string for
// repoId on the wire so we can give a precise 400 if the UUID is malformed.
type componentPathRequest struct {
	RepoID string `json:"repoId"`
	Glob   string `json:"glob"`
}

type createComponentRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Paths       []componentPathRequest `json:"paths"`
}

// repoLister is the consumer-side view of the projects store that path
// validation needs: one read of the project's declared git_repos[].
// *projects.Store satisfies it.
type repoLister interface {
	ListRepos(ctx context.Context, projectID uuid.UUID) ([]projects.Repo, error)
}

// validatePaths validates each path's glob and repo_id. Returns the
// canonical []ComponentPath on success.
//
// The project's git_repos[] is read AT MOST ONCE per call regardless of
// how many paths the request carries — the row is identical for every
// path, so one SELECT feeds an in-memory id set. The read is lazy (it
// happens only when the first path has already passed glob + UUID
// validation) so the per-path error precedence is unchanged: a bad glob
// or a malformed repoId in paths[0] still short-circuits before any
// database access, and an empty paths[] issues no query at all.
func validatePaths(
	ctx context.Context,
	lister repoLister,
	projectID uuid.UUID,
	raw []componentPathRequest,
) ([]projects.ComponentPath, error) {
	out := make([]projects.ComponentPath, 0, len(raw))
	var known map[uuid.UUID]struct{}
	for i, p := range raw {
		if err := projects.ValidateGlob(p.Glob); err != nil {
			return nil, err
		}
		repoID, err := uuid.Parse(p.RepoID)
		if err != nil {
			return nil, &projects.ValidationError{
				Code:    "invalid_repo_id",
				Message: "paths[" + strconv.Itoa(i) + "].repoId is not a UUID",
			}
		}
		if known == nil {
			repos, err := lister.ListRepos(ctx, projectID)
			if err != nil {
				// Includes projects.ErrNotFound for a missing project —
				// propagated raw, exactly as the per-path lookup did.
				return nil, err
			}
			known = make(map[uuid.UUID]struct{}, len(repos))
			for _, r := range repos {
				known[r.ID] = struct{}{}
			}
		}
		// Verify repo exists in this project's git_repos[].
		if _, ok := known[repoID]; !ok {
			return nil, &projects.ValidationError{
				Code:    "repo_not_found",
				Message: "paths[" + strconv.Itoa(i) + "].repoId does not exist in project's git_repos",
			}
		}
		out = append(out, projects.ComponentPath{
			RepoID: repoID,
			Glob:   p.Glob,
		})
	}
	return out, nil
}

// Create handles POST /api/v1/projects/{projectID}/components. Owner-only.
func (h *ComponentsHandler) Create(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	var req createComponentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_name", "name is required", nil)
		return
	}

	paths, err := validatePaths(ctx, h.store, pid, req.Paths)
	if err != nil {
		if mapValidation(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	dto, err := h.store.CreateComponent(ctx, pid, c.UserID, projects.CreateComponentArgs{
		Name:        req.Name,
		Description: req.Description,
		Paths:       paths,
	}, c.Actor)
	if err != nil {
		if mapConflict(w, err) {
			return
		}
		slog.ErrorContext(ctx, "create component failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

type patchComponentRequest struct {
	Name        *string                 `json:"name"`
	Description *string                 `json:"description"`
	Paths       *[]componentPathRequest `json:"paths"`
}

// Patch handles PATCH /api/v1/projects/{projectID}/components/{componentID}.
func (h *ComponentsHandler) Patch(w http.ResponseWriter, r *http.Request) {
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
	cid, err := parseUUIDParam(r, "componentID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_component_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	var req patchComponentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	args := projects.UpdateComponentArgs{
		Name:        req.Name,
		Description: req.Description,
	}
	if req.Paths != nil {
		paths, err := validatePaths(ctx, h.store, pid, *req.Paths)
		if err != nil {
			if mapValidation(w, err) {
				return
			}
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
		args.Paths = &paths
	}

	dto, err := h.store.UpdateComponent(ctx, pid, cid, args, c.Actor)
	if errors.Is(err, projects.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "component_not_found", "component not found", nil)
		return
	}
	if err != nil {
		if mapConflict(w, err) {
			return
		}
		slog.ErrorContext(ctx, "patch component failed",
			"projectId", pid.String(), "componentId", cid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Get handles GET /api/v1/projects/{projectID}/components/{componentID}.
func (h *ComponentsHandler) Get(w http.ResponseWriter, r *http.Request) {
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
	cid, err := parseUUIDParam(r, "componentID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_component_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	dto, err := h.store.GetComponent(ctx, pid, cid)
	if errors.Is(err, projects.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "component_not_found", "component not found", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get component failed",
			"projectId", pid.String(), "componentId", cid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// List handles GET /api/v1/projects/{projectID}/components?archived=...
//
// Counters (sk-423): blockedCount / myOutboxBlockedCount / onMeBlockedCount
// and their open / in_progress complements count an issue as blocked only
// by a blocks / waits-for blocker in a project the CALLER can see; a
// blocker outside the caller's horizon never moves a count (it would reveal
// whether that hidden issue is closed). The lens filters follow the same
// rule.
func (h *ComponentsHandler) List(w http.ResponseWriter, r *http.Request) {
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
	archived := q.Get("archived")
	// `lens` (sk-30) narrows the row set to components with at least
	// one matching issue under the active lens. Same set of values as
	// the projects list endpoint.
	var lens string
	if l := q.Get("lens"); l != "" {
		switch l {
		case "on-me", "delegated":
			lens = l
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_lens",
				"lens must be 'on-me' or 'delegated'", nil)
			return
		}
	}
	out, err := h.store.ListComponentsWithLens(ctx, projects.ListComponentsArgs{
		ProjectID: pid,
		Archived:  archived,
		CallerID:  c.UserID,
		Lens:      lens,
	})
	if err != nil {
		slog.ErrorContext(ctx, "list components failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Archive handles POST /api/v1/projects/{projectID}/components/{componentID}/archive.
func (h *ComponentsHandler) Archive(w http.ResponseWriter, r *http.Request) {
	h.archive(w, r, true)
}

// Unarchive handles POST /api/v1/projects/{projectID}/components/{componentID}/unarchive.
func (h *ComponentsHandler) Unarchive(w http.ResponseWriter, r *http.Request) {
	h.archive(w, r, false)
}

// Delete is the alias for Archive — DELETE on /components/{componentID}
// soft-archives the component (no hard-delete).
func (h *ComponentsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	h.archive(w, r, true)
}

func (h *ComponentsHandler) archive(w http.ResponseWriter, r *http.Request, archive bool) {
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
	cid, err := parseUUIDParam(r, "componentID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_component_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	if archive {
		err = h.store.ArchiveComponent(ctx, pid, cid, c.Actor)
		if errors.Is(err, projects.ErrAlreadyArchived) {
			writeProblem(w, http.StatusUnprocessableEntity, "already_archived",
				"component is already archived", nil)
			return
		}
	} else {
		err = h.store.UnarchiveComponent(ctx, pid, cid, c.Actor)
		if errors.Is(err, projects.ErrNotArchived) {
			writeProblem(w, http.StatusUnprocessableEntity, "not_archived",
				"component is not archived", nil)
			return
		}
	}
	if errors.Is(err, projects.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "component_not_found", "component not found", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "archive component failed",
			"projectId", pid.String(), "componentId", cid.String(),
			"archive", archive, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

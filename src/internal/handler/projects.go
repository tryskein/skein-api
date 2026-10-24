package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
)

// ProjectsHandler bundles the shared dependencies for every
// /api/v1/projects/* handler. Build once at startup; methods are
// goroutine-safe (Store is itself stateless beyond the pool).
//
// embedder is optional — wired by main() when OLLAMA_URL is set so the
// list endpoint can do semantic rerank when FTS results come back thin.
// nil falls back to FTS-only. We use the package-local
// projects.QueryEmbedder interface so the handler stays free of an
// import on `search`.
type ProjectsHandler struct {
	pool     *pgxpool.Pool
	store    *projects.Store
	embedder projects.QueryEmbedder
	// defaultVisibility is SKEIN_PROJECT_DEFAULT_VISIBILITY — the visibility
	// of a new project whose request names none (sk-674.18 T024).
	defaultVisibility string
	// prefixLimiter bounds GET /projects/prefix-check and
	// /prefix-suggestions per caller (sk-674.18 T025, US-15): the in-pod
	// window of loginLimiter; the primary limit of public paths is web's
	// (plan "Samples and their limits", handler/loginlimiter.go).
	prefixLimiter *loginLimiter
}

// Prefix-check rate limit (US-15 "the check rate is limited"): per caller,
// per route — a form checking as the person types stays far below it.
const (
	prefixChecksPerWindow = 30
	prefixCheckWindow     = time.Minute
)

// NewProjectsHandler constructs a handler with a fresh Store wrapping the
// supplied pool. Pass nil for embedder to disable semantic rerank. The
// default visibility of new projects is members (the variable's default)
// until SetDefaultVisibility.
func NewProjectsHandler(pool *pgxpool.Pool, embedder projects.QueryEmbedder) *ProjectsHandler {
	return &ProjectsHandler{pool: pool, store: projects.NewStore(pool), embedder: embedder,
		defaultVisibility: spaces.VisibilityMembers,
		prefixLimiter:     newLoginLimiter(prefixChecksPerWindow, prefixChecksPerWindow, prefixCheckWindow)}
}

// SetProjectQuotas sets SKEIN_QUOTA_PERSONAL_PROJECTS and
// SKEIN_QUOTA_SPACE_PROJECTS (nil = none) for new projects (sk-674.18 T026).
func (h *ProjectsHandler) SetProjectQuotas(personal, space *int) {
	h.store.SetProjectQuotas(personal, space)
}

// SetPrefixMinLength sets SKEIN_PROJECT_PREFIX_MIN_LENGTH (nil = none) for
// new projects and suggestions (sk-674.18 T025).
func (h *ProjectsHandler) SetPrefixMinLength(n *int) { h.store.SetPrefixMinLength(n) }

// SetDefaultVisibility sets SKEIN_PROJECT_DEFAULT_VISIBILITY (validated by
// config.Load to space | members).
func (h *ProjectsHandler) SetDefaultVisibility(v string) { h.defaultVisibility = v }

// writeSpacesRefusal writes a refusal of the spaces code registry; false
// when err is not one.
func writeSpacesRefusal(w http.ResponseWriter, err error) bool {
	var ref *spaces.Refusal
	if errors.As(err, &ref) {
		writeProblem(w, ref.Status, ref.Code, ref.Message, ref.Extra)
		return true
	}
	return false
}

// validVisibility checks a requested project visibility; nil is "not
// given".
func validVisibility(v *string) bool {
	return v == nil || *v == spaces.VisibilitySpace || *v == spaces.VisibilityMembers
}

// ----------------- Helpers (caller identity & error mapping) -----------------

type callerCtx struct {
	UserID     uuid.UUID
	Username   string
	Role       string
	IsExternal bool
	Actor      projects.Actor
}

// Principal returns the centralized identity primitive for this caller
// (sk-163 S1). Use cc.Principal().IsAdmin() for the admin-ceiling decision
// rather than comparing cc.Role to "admin" directly — external users are
// clamped to non-admin in one place. (S1 adds this; the raw role== sites
// are migrated in S2.)
func (cc callerCtx) Principal() auth.Principal {
	return auth.Principal{
		UserID:     cc.UserID,
		Role:       cc.Role,
		IsExternal: cc.IsExternal,
	}
}

// caller pulls the authenticated principal from the request context and
// returns false if anything is missing — handler must already be wrapped
// in RequireUser, but defence in depth prevents nil-uuid lookups.
func caller(r *http.Request) (callerCtx, bool) {
	id, ok := auth.UserIDFrom(r.Context())
	if !ok {
		return callerCtx{}, false
	}
	username, _ := auth.UsernameFrom(r.Context())
	role, _ := auth.UserRoleFrom(r.Context())
	if role == "" {
		role = "user"
	}
	return callerCtx{
		UserID:     id,
		Username:   username,
		Role:       role,
		IsExternal: auth.IsExternalFrom(r.Context()),
		Actor:      projects.HumanActor(id, username),
	}, true
}

// mapAccessErr translates the projects.access sentinel errors into the
// right HTTP status. The "not visible" branch returns 404 to avoid
// leaking project existence (vision §10).
func mapAccessErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, projects.ErrProjectNotVisible) || errors.Is(err, projects.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "project not found", nil)
		return true
	}
	if errors.Is(err, projects.ErrProjectNotOwner) {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"only the project owner, the space owner or the instance admin can perform this action", nil)
		return true
	}
	if errors.Is(err, projects.ErrProjectActionForbidden) {
		// sk-674.18 T066: visible, but the caller's level does not allow the
		// action (US-6). Since sk-741 no level of a visible project lacks
		// ActionWrite / ActionAssign; the answer stays for the action table.
		writeProblem(w, http.StatusForbidden, "forbidden",
			"your access to this project does not allow this action", nil)
		return true
	}
	return false
}

// mapConflict translates store unique-constraint conflicts into business
// 422s with stable error codes the BFF/frontend can branch on.
func mapConflict(w http.ResponseWriter, err error) bool {
	var conflict *projects.ErrConflict
	if !errors.As(err, &conflict) {
		return false
	}
	switch {
	case strings.Contains(conflict.Constraint, "name_lower"):
		// sk-674.18 T025, D-4: names are unique per space.
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeProjectNameTaken,
			"a project with this name already exists in the space", nil)
	case strings.Contains(conflict.Constraint, "id_prefix_lower"):
		// Neutral (US-15): never the constraint, the holder or its space.
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodePrefixTaken,
			"this prefix is taken", map[string]any{"suggestions": []string{}})
	case strings.Contains(conflict.Constraint, "components_project_id_name"):
		writeProblem(w, http.StatusUnprocessableEntity, "component_name_taken",
			"a component with this name already exists in the project", nil)
	default:
		writeProblem(w, http.StatusUnprocessableEntity, "conflict",
			"unique constraint violation: "+conflict.Constraint, nil)
	}
	return true
}

// mapSpaceWriteErr maps the space errors of a project write: the space gone
// meanwhile (404 space_not_found), the creator no longer its participant
// (422 not_space_member with spaceId), a lost serialisation race (422
// concurrent_change).
func mapSpaceWriteErr(w http.ResponseWriter, err error, spaceShort string) bool {
	switch {
	case errors.Is(err, spaces.ErrSpaceNotFound):
		writeProblem(w, http.StatusNotFound, spaces.CodeSpaceNotFound, "space not found", nil)
	case errors.Is(err, projects.ErrNotSpaceMember):
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeNotSpaceMember,
			"add yourself as a member of the space to create projects there",
			map[string]any{"spaceId": spaceShort})
	case errors.Is(err, spaces.ErrConcurrentChange):
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeConcurrentChange,
			"the project was changed concurrently; nothing was changed, retry the request", nil)
	default:
		return false
	}
	return true
}

func mapValidation(w http.ResponseWriter, err error) bool {
	if ve, ok := projects.IsValidationError(err); ok && ve.Code == projects.CodePrefixInvalid {
		// A business refusal of the code registry (spaces.CodePrefixInvalid).
		writeProblem(w, http.StatusUnprocessableEntity, ve.Code, ve.Message, nil)
		return true
	}
	if ve, ok := projects.IsValidationError(err); ok {
		writeProblem(w, http.StatusBadRequest, ve.Code, ve.Message, nil)
		return true
	}
	return false
}

// ----------------- Endpoints -----------------

// createProjectRequest is the wire shape for POST /projects.
type createProjectRequest struct {
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	IDPrefix      string  `json:"idPrefix"`
	DefaultIDMode string  `json:"defaultIdMode"`
	GitlabGroup   *string `json:"gitlabGroup"`
	// SpaceID — the short id of the space (sk-674.18 T024); omitted: the
	// one space the caller can create projects in (see below).
	SpaceID *string `json:"spaceId"`
	// Visibility — "space" | "members"; omitted:
	// SKEIN_PROJECT_DEFAULT_VISIBILITY (always members in a "Personal").
	Visibility *string `json:"visibility"`
}

// Create handles POST /api/v1/projects. A participant of a space creates a
// project in it and becomes its owner inside the same transaction
// (Store.CreateProject). The space is chosen by spaces.ChooseForProject
// (contract «Choosing the space of a new project»): spaceId given — that
// space (404 space_not_found if the caller does not see it, 422
// not_space_member if he sees it only as the admin); omitted — the one
// eligible space, or 422 space_required with `spaces` when there are
// several; an external user — 422 external_cannot_create_project. A
// `space` project gives every participant the membership "from
// the space".
//
// @Summary Create a project
// @Description Creates a project in a space; the caller becomes its owner in the same transaction. The space: spaceId (a short id) names it — a space the caller does not see answers 404 space_not_found, one he sees only as the instance admin 422 not_space_member (add yourself as a member first); omitted — the one space the caller can create projects in (GET /me/spaces, canCreateProject), or 422 space_required carrying `spaces` (every such space) when there are several. An external user creates no project (422 external_cannot_create_project).
// @Description
// @Description visibility: space — every member of the space sees the project and gets a membership from the space; members — only explicit members and the space owner. Omitted: SKEIN_PROJECT_DEFAULT_VISIBILITY; a project of a personal space is always members. idPrefix omitted — the first free suggestion for the name (GET /projects/prefix-suggestions); given — it must start with a letter and be at least SKEIN_PROJECT_PREFIX_MIN_LENGTH long.
// @Description
// @Description Side effects: the project row, the owner membership (and the space memberships for visibility space), aliases and repos given, one project.created event; signals to everyone of the space who sees the new project. The space is locked for the transaction (K-7): a member joining or leaving meanwhile is ordered with it.
// @Tags Projects
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body createProjectRequest true "name (required), idPrefix, defaultIdMode, description, gitlabGroup, spaceId, visibility"
// @Success 201 {object} projects.ProjectDTO "The new project"
// @Failure 400 {object} map[string]any "invalid_json, validation_error (visibility not space|members), name/prefix/mode validation codes"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "space_required (spaces), not_space_member (spaceId), project_quota (spaceId, projectCount, projectQuota — archived projects count), external_cannot_create_project, prefix_invalid (a digit first, pattern, shorter than SKEIN_PROJECT_PREFIX_MIN_LENGTH), prefix_taken (suggestions: 2–3 free prefixes; neutral — taken anywhere on the installation or reserved by a deleted project), project_name_taken (names are unique per space), concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/projects [post]
func (h *ProjectsHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	var req createProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	name, err := projects.ValidateProjectName(req.Name)
	if err != nil {
		mapValidation(w, err)
		return
	}

	// sk-674.18 T025: an omitted prefix is the first free suggestion of the
	// name (plan 4.4); a given one keeps the prefix rule.
	idPrefix := req.IDPrefix
	if strings.TrimSpace(idPrefix) == "" {
		sugg, err := h.store.SuggestPrefixes(ctx, name)
		if err != nil {
			slog.ErrorContext(ctx, "suggesting a prefix failed", "userId", c.UserID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
		if len(sugg) == 0 {
			h.writePrefixTaken(w, r, name)
			return
		}
		idPrefix = sugg[0]
	} else if idPrefix, err = h.store.CheckPrefixRules(idPrefix); err != nil {
		mapValidation(w, err)
		return
	}

	mode := req.DefaultIDMode
	if mode == "" {
		mode = "counter"
	}
	if _, err := projects.ValidateIDMode(mode); err != nil {
		mapValidation(w, err)
		return
	}
	if !validVisibility(req.Visibility) {
		writeProblem(w, http.StatusBadRequest, "validation_error", "visibility must be space or members", nil)
		return
	}
	short, requested := "", ""
	if req.SpaceID != nil {
		short = strings.TrimSpace(*req.SpaceID)
	}
	if req.Visibility != nil {
		requested = *req.Visibility
	}
	space, err := spaces.ChooseForProject(ctx, h.pool, c.UserID, short)
	if err != nil {
		if writeSpacesRefusal(w, err) {
			return
		}
		slog.ErrorContext(ctx, "choosing the space of a project failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	dto, err := h.store.CreateProject(ctx, projects.CreateProjectArgs{
		SpaceID:       space.ID,
		Visibility:    spaces.ProjectVisibility(space.Kind, requested, h.defaultVisibility),
		Name:          name,
		IDPrefix:      idPrefix,
		Description:   req.Description,
		DefaultIDMode: mode,
		GitlabGroup:   req.GitlabGroup,
		CreatorID:     c.UserID,
		CreatorRole:   c.Role,
		CreatorName:   c.Username,
		Actor:         projects.HumanActor(c.UserID, c.Username),
	})
	if err != nil {
		if errors.Is(err, projects.ErrPrefixTaken) {
			h.writePrefixTaken(w, r, name)
			return
		}
		var quota *projects.ErrProjectQuota
		if errors.As(err, &quota) {
			writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeProjectQuota,
				fmt.Sprintf("the space has reached its project limit: %d of %d", quota.ProjectCount, quota.ProjectQuota),
				map[string]any{"spaceId": quota.SpaceID, "projectCount": quota.ProjectCount, "projectQuota": quota.ProjectQuota})
			return
		}
		if mapConflict(w, err) {
			return
		}
		if errors.Is(err, projects.ErrExternalCannotCreateProject) {
			// sk-163 S3 — external user would become owner of the new
			// project; refused cleanly (mirrors the 422 external_cannot_be_owner
			// member path) rather than tripping the owner trigger as a 500.
			writeProblem(w, http.StatusUnprocessableEntity, "external_cannot_create_project",
				"external users cannot create projects", nil)
			return
		}
		if mapSpaceWriteErr(w, err, space.ShortID) {
			return
		}
		slog.ErrorContext(ctx, "create project failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	slog.InfoContext(ctx, "project created",
		"userId", c.UserID.String(), "projectId", dto.ID.String(), "idPrefix", dto.IDPrefix)
	writeJSON(w, http.StatusCreated, dto)
}

// List handles GET /api/v1/projects with cursor pagination and search.
//
// Query params:
//   - archived: false (default) | true | all
//   - search: free-form legacy trigram filter (kept for back-compat)
//   - q: free-form FTS+semantic search. When non-empty the response
//     items are ordered by relevance score instead of created_at; the
//     `total` field still reflects the caller-visible row count under
//     the same WHERE clause. Empty q falls through to the alphabetic
//     listing.
//   - cursor: opaque base64 from a previous response's nextCursor
//   - limit: 1..100, default 50
//   - mineOnly: bool — narrow admin view to "projects I'm in"
//
// Counters (sk-423): blockedCount / myOutboxBlockedCount / onMeBlockedCount
// and their open / in_progress complements count an issue as blocked only
// by a blocks / waits-for blocker in a project the CALLER can see; a
// blocker outside the caller's horizon never moves a count (it would reveal
// whether that hidden issue is closed). The lens filters follow the same
// rule.
//
// componentsCount (sk-551.53) is the project's number of NON-archived
// components — caller-independent, the same figure GET /projects/{id}
// reports.
//
// @Summary List projects
// @Description The projects the caller may access, cursor-paginated. Rows are ordered by space — the personal space, the common space, the others by name — and inside a space by project name, or by relevance with q (K-20); the cursor carries that order (a cursor of the former order is refused with 400 invalid_cursor). space=<short id> keeps only that space's projects; a space the caller does not see gives an empty list. Every row carries spaceId and visibility. Read-only.
// @Tags Projects
// @Produce json
// @Security ApiKeyAuth
// @Param archived query string false "false (default) | true | all"
// @Param q query string false "Full-text / semantic search"
// @Param space query string false "Space short id: only its projects"
// @Param lens query string false "on-me | delegated"
// @Param cursor query string false "Opaque cursor from nextCursor"
// @Param limit query int false "1..100, default 50"
// @Param mineOnly query bool false "Only projects the caller is a member of"
// @Success 200 {object} projects.ListResult "A page of projects"
// @Failure 400 {object} map[string]any "invalid_cursor, invalid_limit, invalid_lens"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/projects [get]
func (h *ProjectsHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()
	args := projects.ListProjectsArgs{
		CallerID: c.UserID,
		Archived: q.Get("archived"),
		Search:   q.Get("search"),
		Query:    q.Get("q"),
		Embedder: h.embedder,
		MineOnly: q.Get("mineOnly") == "true" || q.Get("mine_only") == "true",
	}

	// `lens` (sk-30) narrows the row set to projects with at least one
	// matching issue under the active lens. Only "on-me", "delegated",
	// and "" are accepted — anything else is a client bug, reject it
	// with 400 instead of silently degrading to the unfiltered list.
	if lens := q.Get("lens"); lens != "" {
		switch lens {
		case "on-me", "delegated":
			args.Lens = lens
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_lens",
				"lens must be 'on-me' or 'delegated'", nil)
			return
		}
	}

	if cursor := q.Get("cursor"); cursor != "" {
		cur, err := projects.DecodeListCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", err.Error(), nil)
			return
		}
		args.Cursor = &cur
	}
	// sk-674.18 T024 — only that space's projects (short id).
	args.Space = strings.TrimSpace(q.Get("space"))
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 100 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be an integer in [1, 100]", nil)
			return
		}
		args.Limit = n
	}

	res, err := h.store.ListProjects(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "list projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Get handles GET /api/v1/projects/{projectID}. 404 for not visible.
//
// Stats (sk-423): stats.open / stats.inProgress / stats.blocked count an
// issue as blocked only by a blocker in a project the CALLER can see — the
// same rule as the project row counters.
func (h *ProjectsHandler) Get(w http.ResponseWriter, r *http.Request) {
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
	// GetProjectWithStats fills the caller-scoped MyAssigned / MyCreated
	// fields. Anonymous callers don't reach this branch (caller() guards
	// above) so c.UserID is always real here.
	dto, err := h.store.GetProjectWithStats(ctx, pid, c.UserID)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// patchProjectRequest captures the partial-update shape. Pointers are
// crucial — nil means "leave field alone", "" means "clear".
type patchProjectRequest struct {
	Name          *string `json:"name"`
	Description   *string `json:"description"`
	DefaultIDMode *string `json:"defaultIdMode"`
	GitlabGroup   *string `json:"gitlabGroup"`
	// Visibility — "space" | "members" (sk-674.18 T024, US-17).
	Visibility *string `json:"visibility"`
	// SpaceID is accepted only to be refused: a project never changes its
	// space (US-12, E-31) — any value but null is 422 space_change_forbidden,
	// nothing changed; null counts as absent.
	SpaceID json.RawMessage `json:"spaceId"`
}

// Patch handles PATCH /api/v1/projects/{projectID}. Owner-only (the
// project's owner, its space's owner or the instance admin —
// authz.ActionManageProject, the same levels as ActionChangeVisibility).
// A visibility switch runs in the
// same transaction as the other fields: space → members takes the project
// away from everyone left without a source (INV-12), members → space gives
// it to every participant. spaceId in the body → 422
// space_change_forbidden before anything is changed.
//
// @Summary Update a project
// @Description Patches name, description, defaultIdMode, gitlabGroup and visibility (the project owner, the space owner and the instance admin). A visibility switch runs in the same transaction: members → space gives every member of the space a membership from the space; space → members removes the memberships that had no other source, and everyone who loses the project loses with it their open assignments, claims, watches and reminders there (INV-12). A project never changes its space: a spaceId in the body (any value but null, which counts as absent) is refused with 422 space_change_forbidden and nothing changes.
// @Tags Projects
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param body body patchProjectRequest true "Fields to change"
// @Success 200 {object} projects.ProjectDTO "The updated project"
// @Failure 400 {object} map[string]any "invalid_id, invalid_json, validation_error"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: not an owner"
// @Failure 404 {object} map[string]any "not_found"
// @Failure 422 {object} map[string]any "space_change_forbidden, project_name_taken, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/projects/{projectID} [patch]
func (h *ProjectsHandler) Patch(w http.ResponseWriter, r *http.Request) {
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

	var req patchProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	// A null spaceId names no space — the «not set» of a client that
	// serialises absent fields as null; any value is the
	// move E-31 refuses.
	if req.SpaceID != nil && strings.TrimSpace(string(req.SpaceID)) != "null" {
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeSpaceChangeForbidden,
			"a project cannot be moved to another space", nil)
		return
	}
	if !validVisibility(req.Visibility) {
		writeProblem(w, http.StatusBadRequest, "validation_error", "visibility must be space or members", nil)
		return
	}

	if req.Name != nil {
		v, err := projects.ValidateProjectName(*req.Name)
		if err != nil {
			mapValidation(w, err)
			return
		}
		req.Name = &v
	}
	if req.DefaultIDMode != nil {
		if _, err := projects.ValidateIDMode(*req.DefaultIDMode); err != nil {
			mapValidation(w, err)
			return
		}
	}

	dto, err := h.store.UpdateProject(ctx, pid, projects.UpdateProjectArgs{
		Name:          req.Name,
		Description:   req.Description,
		DefaultIDMode: req.DefaultIDMode,
		GitlabGroup:   req.GitlabGroup,
		Visibility:    req.Visibility,
	}, c.Actor)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		if mapConflict(w, err) {
			return
		}
		if mapSpaceWriteErr(w, err, "") {
			return
		}
		slog.ErrorContext(ctx, "patch project failed",
			"userId", c.UserID.String(), "projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Archive handles POST /api/v1/projects/{projectID}/archive. Owner-only.
// Idempotent in the sense of "already archived → 422" — a true 200 second
// time would be misleading.
func (h *ProjectsHandler) Archive(w http.ResponseWriter, r *http.Request) {
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
	err = h.store.ArchiveProject(ctx, pid, c.Actor)
	if errors.Is(err, projects.ErrAlreadyArchived) {
		writeProblem(w, http.StatusUnprocessableEntity, "already_archived",
			"project is already archived", nil)
		return
	}
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "archive project failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Unarchive is the mirror of Archive.
func (h *ProjectsHandler) Unarchive(w http.ResponseWriter, r *http.Request) {
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
	err = h.store.UnarchiveProject(ctx, pid, c.Actor)
	if errors.Is(err, projects.ErrNotArchived) {
		writeProblem(w, http.StatusUnprocessableEntity, "not_archived",
			"project is not archived", nil)
		return
	}
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "unarchive project failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writePrefixTaken answers the neutral «taken» (US-15, E-17): one code,
// 2–3 free alternatives derived from name, nothing about the holder.
func (h *ProjectsHandler) writePrefixTaken(w http.ResponseWriter, r *http.Request, name string) {
	alts, err := h.store.PrefixAlternatives(r.Context(), name)
	if err != nil {
		slog.ErrorContext(r.Context(), "prefix alternatives failed", "error", err.Error())
		alts = nil
	}
	if alts == nil {
		alts = []string{}
	}
	writeProblem(w, http.StatusUnprocessableEntity, spaces.CodePrefixTaken,
		"this prefix is taken", map[string]any{"suggestions": alts})
}

// prefixAdmitted applies the per-caller prefix limit; false when the 429 was
// written. Checked before anything else, so a throttled caller costs nothing.
func (h *ProjectsHandler) prefixAdmitted(w http.ResponseWriter, route string, c callerCtx) bool {
	if h.prefixLimiter.tryAcquire(route + ":" + c.UserID.String()) {
		return true
	}
	writeProblem(w, http.StatusTooManyRequests, "rate_limited",
		"too many prefix checks; try again in a minute", nil)
	return false
}

// PrefixSuggestions handles GET /api/v1/projects/prefix-suggestions.
//
// @Summary Suggest project prefixes
// @Description Free prefixes for a project named `name`, best first: the first goes into the field, the next 3–4 are offered (plan 4.4, after OpenProject's generator). The name is transliterated (go-unidecode) and lowercased; several words give their initials (up to 5) lengthened word by word in turn, one word its first 3 characters lengthened by one; never shorter than SKEIN_PROJECT_PREFIX_MIN_LENGTH, never longer than 10, never starting with a digit; when all are taken — the first with a number (ps2, ps3). Prefixes are unique on the whole installation and the prefixes of deleted projects are never given again, so spaceId does not narrow anything (accepted for the form's convenience). A suggestion reserves nothing. Rate-limited per caller.
// @Tags Projects
// @Produce json
// @Security ApiKeyAuth
// @Param name query string true "The project name"
// @Param spaceId query string false "The space of the project-to-be (does not affect the answer)"
// @Success 200 {object} spaces.PrefixSuggestionsResponse "Free prefixes, best first"
// @Failure 400 {object} map[string]any "validation_error: no name"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/projects/prefix-suggestions [get]
func (h *ProjectsHandler) PrefixSuggestions(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if !h.prefixAdmitted(w, "suggest", c) {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeProblem(w, http.StatusBadRequest, "validation_error", "name is required", nil)
		return
	}
	sugg, err := h.store.SuggestPrefixes(r.Context(), name)
	if err != nil {
		slog.ErrorContext(r.Context(), "prefix suggestions failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if sugg == nil {
		sugg = []string{}
	}
	writeJSON(w, http.StatusOK, spaces.PrefixSuggestionsResponse{Suggestions: sugg})
}

// PrefixCheck handles GET /api/v1/projects/prefix-check.
//
// @Summary Check a project prefix
// @Description Whether `prefix` can be used for a new project now. Breaking the prefix rule (pattern, a digit first, shorter than SKEIN_PROJECT_PREFIX_MIN_LENGTH) — available false, reason prefix_invalid. Taken by any project of the installation or reserved by a deleted one — available false, reason prefix_taken and 2–3 free alternatives; the answer is neutral: it never says where, in whose space or by whom. Read-only; rate-limited per caller (the limit counts the caller, whatever prefix is checked).
// @Tags Projects
// @Produce json
// @Security ApiKeyAuth
// @Param prefix query string true "The prefix to check"
// @Success 200 {object} spaces.PrefixCheckResponse "available, reason, suggestions"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/projects/prefix-check [get]
func (h *ProjectsHandler) PrefixCheck(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if !h.prefixAdmitted(w, "check", c) {
		return
	}
	resp := spaces.PrefixCheckResponse{Suggestions: []string{}}
	prefix, err := h.store.CheckPrefixRules(r.URL.Query().Get("prefix"))
	if err != nil {
		reason := spaces.CodePrefixInvalid
		resp.Reason = &reason
		writeJSON(w, http.StatusOK, resp)
		return
	}
	taken, err := h.store.PrefixTaken(r.Context(), prefix)
	if err == nil && taken {
		var alts []string
		if alts, err = h.store.PrefixAlternatives(r.Context(), prefix); err == nil {
			reason := spaces.CodePrefixTaken
			resp.Reason = &reason
			if alts != nil {
				resp.Suggestions = alts
			}
		}
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "prefix check failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	resp.Available = !taken
	writeJSON(w, http.StatusOK, resp)
}

// Resolve handles GET /api/v1/projects/resolve?q=...
//
// @Summary Resolve a project by a string
// @Description Resolves q among the projects the caller may access (INV-29): exact paths first in the order id → prefix → alias → name — a prefix, alias or id hit wins even when q is also another project's name; one accessible project with that exact name wins, several are the final 422 ambiguous_project with q and candidates (prefix, name, space of each), which no later form bypasses. Without an exact hit: the repo URL, then fuzzy relevance with a confident single pick, as before. No match is 200 with match null. Inaccessible projects take part in no form. Read-only.
// @Tags Projects
// @Produce json
// @Security ApiKeyAuth
// @Param q query string true "Project id, prefix, alias, name, repo URL or a fuzzy string"
// @Success 200 {object} projects.ResolveResult "match or candidates"
// @Failure 400 {object} map[string]any "missing_query"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 422 {object} map[string]any "ambiguous_project (q, candidates)"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/projects/resolve [get]
func (h *ProjectsHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeProblem(w, http.StatusBadRequest, "missing_query", "q is required", nil)
		return
	}
	res, err := h.store.Resolve(ctx, q, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "resolve failed", "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if res.Ambiguous {
		// INV-29: the exact name of several accessible projects — final.
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeAmbiguousProject,
			"the name matches several projects; specify one by its prefix",
			map[string]any{"q": strings.TrimSpace(q), "candidates": projects.AmbiguousCandidates(res.Candidates)})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ----------------- Repos sub-resource -----------------

type addRepoRequest struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	DefaultBranch string `json:"defaultBranch"`
}

// AddRepo handles POST /api/v1/projects/{projectID}/repos. Owner-only.
func (h *ProjectsHandler) AddRepo(w http.ResponseWriter, r *http.Request) {
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

	var req addRepoRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_name", "name is required", nil)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_url", "url is required", nil)
		return
	}

	repo, err := h.store.AddRepo(ctx, pid, projects.AddRepoArgs{
		Name:          req.Name,
		URL:           req.URL,
		DefaultBranch: req.DefaultBranch,
	}, c.Actor)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "add repo failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}

type updateRepoRequest struct {
	Name          *string `json:"name"`
	URL           *string `json:"url"`
	DefaultBranch *string `json:"defaultBranch"`
}

// UpdateRepo handles PATCH /api/v1/projects/{projectID}/repos/{repoID}.
func (h *ProjectsHandler) UpdateRepo(w http.ResponseWriter, r *http.Request) {
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
	rid, err := parseUUIDParam(r, "repoID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_repo_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	var req updateRepoRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	repo, err := h.store.UpdateRepo(ctx, pid, rid, projects.UpdateRepoArgs{
		Name:          req.Name,
		URL:           req.URL,
		DefaultBranch: req.DefaultBranch,
	}, c.Actor)
	if errors.Is(err, projects.ErrRepoNotFound) {
		writeProblem(w, http.StatusNotFound, "repo_not_found", "repo not found", nil)
		return
	}
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "update repo failed",
			"projectId", pid.String(), "repoId", rid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

// RemoveRepo handles DELETE /api/v1/projects/{projectID}/repos/{repoID}.
// 422 with components list if any component still references the repo.
func (h *ProjectsHandler) RemoveRepo(w http.ResponseWriter, r *http.Request) {
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
	rid, err := parseUUIDParam(r, "repoID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_repo_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	err = h.store.RemoveRepo(ctx, pid, rid, c.Actor)
	if errors.Is(err, projects.ErrRepoNotFound) {
		writeProblem(w, http.StatusNotFound, "repo_not_found", "repo not found", nil)
		return
	}
	var inUse *projects.ErrRepoInUse
	if errors.As(err, &inUse) {
		writeProblem(w, http.StatusUnprocessableEntity, "repo_in_use",
			"repo is referenced by component(s)",
			map[string]any{"components": inUse.Components})
		return
	}
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "remove repo failed",
			"projectId", pid.String(), "repoId", rid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------- Aliases sub-resource -----------------

type addAliasRequest struct {
	Alias string `json:"alias"`
}

// AddAlias handles POST /api/v1/projects/{projectID}/aliases. Owner-only.
func (h *ProjectsHandler) AddAlias(w http.ResponseWriter, r *http.Request) {
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
	var req addAliasRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	alias, err := projects.ValidateAlias(req.Alias)
	if err != nil {
		mapValidation(w, err)
		return
	}
	dto, err := h.store.AddAlias(ctx, pid, c.UserID, alias, c.Actor)
	if err != nil {
		var taken *projects.ErrAliasTaken
		if errors.As(err, &taken) {
			extra := map[string]any{}
			if taken.OwnerProjectID != nil {
				extra["existingProjectId"] = taken.OwnerProjectID.String()
			}
			if taken.OwnerProjectName != nil {
				extra["existingProjectName"] = *taken.OwnerProjectName
			}
			writeProblem(w, http.StatusUnprocessableEntity, "alias_taken",
				"alias is already taken", extra)
			return
		}
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "add alias failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

// RemoveAlias handles DELETE /api/v1/projects/{projectID}/aliases/{alias}.
func (h *ProjectsHandler) RemoveAlias(w http.ResponseWriter, r *http.Request) {
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
	rawAlias := chi.URLParam(r, "alias")
	if rawAlias == "" {
		writeProblem(w, http.StatusBadRequest, "missing_alias", "alias path param is required", nil)
		return
	}
	alias, err := projects.ValidateAlias(rawAlias)
	if err != nil {
		mapValidation(w, err)
		return
	}
	err = h.store.RemoveAlias(ctx, pid, alias, c.Actor)
	if errors.Is(err, projects.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "alias_not_found", "alias not found in this project", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "remove alias failed",
			"projectId", pid.String(), "alias", alias, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAliases handles GET /api/v1/projects/{projectID}/aliases.
func (h *ProjectsHandler) ListAliases(w http.ResponseWriter, r *http.Request) {
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
	out, err := h.store.ListAliases(ctx, pid)
	if err != nil {
		slog.ErrorContext(ctx, "list aliases failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ----------------- Members sub-resource -----------------

type upsertMemberRequest struct {
	Role string `json:"role"`
}

// UpsertMember handles PUT /api/v1/projects/{projectID}/members/{userID}.
// Idempotent — re-PUT with the same role is a no-op (no event written).
func (h *ProjectsHandler) UpsertMember(w http.ResponseWriter, r *http.Request) {
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
	uid, err := parseUUIDParam(r, "userID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_user_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	var req upsertMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.Role != "owner" && req.Role != "member" {
		writeProblem(w, http.StatusBadRequest, "invalid_role",
			"role must be 'owner' or 'member'", nil)
		return
	}

	dto, err := h.store.UpsertMember(ctx, pid, uid, req.Role, c.UserID, c.Actor)
	if errors.Is(err, projects.ErrUserNotFound) {
		writeProblem(w, http.StatusUnprocessableEntity, "user_not_found",
			"target user does not exist", nil)
		return
	}
	if errors.Is(err, projects.ErrExternalCannotBeOwner) {
		// sk-163 S3 — external user capped at 'member'. Business rule → 422.
		writeProblem(w, http.StatusUnprocessableEntity, "external_cannot_be_owner",
			"external users cannot be project owners", nil)
		return
	}
	if errors.Is(err, spaces.ErrConcurrentChange) {
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeConcurrentChange,
			"a concurrent membership change won; retry", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "upsert member failed",
			"projectId", pid.String(), "targetUserId", uid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// RemoveMember handles DELETE /api/v1/projects/{projectID}/members/{userID}.
// 422 member_via_space if the member keeps the project through its space
// (US-17); 422 concurrent_change when a concurrent membership change won
// (K-7). The last explicit owner may go: the project stays under the
// implicit owner of its space (INV-4).
//
// Self-leave (R3.1 Q-7): when callerID == targetID, the EnsureProjectOwner
// gate is bypassed — a plain member must be able to remove themselves
// from a project without bothering the owner.
func (h *ProjectsHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
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
	uid, err := parseUUIDParam(r, "userID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_user_id", err.Error(), nil)
		return
	}
	selfLeave := c.UserID == uid
	if selfLeave {
		// Self-leave: we still need visibility (so non-members get 404 like
		// any other invisible-project request), but skip the owner gate.
		if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
			if mapAccessErr(w, err) {
				return
			}
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
	} else {
		if _, err := projects.EnsureProjectOwner(ctx, h.pool, pid, c.UserID); err != nil {
			if mapAccessErr(w, err) {
				return
			}
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
	}
	err = h.store.RemoveMember(ctx, pid, uid, c.Actor)
	if errors.Is(err, projects.ErrMemberViaSpace) {
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeMemberViaSpace,
			"the member has access to the project through the space; switch the project's visibility to members or remove them from the space", nil)
		return
	}
	if errors.Is(err, spaces.ErrConcurrentChange) {
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeConcurrentChange,
			"a concurrent membership change won; retry", nil)
		return
	}
	if errors.Is(err, projects.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "member_not_found",
			"user is not a member of this project", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "remove member failed",
			"projectId", pid.String(), "targetUserId", uid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListMembers handles GET /api/v1/projects/{projectID}/members.
func (h *ProjectsHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
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
	out, err := h.store.ListMembers(ctx, pid)
	if err != nil {
		slog.ErrorContext(ctx, "list members failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

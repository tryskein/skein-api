package publicapi

// projects.go — the project resources of the public API (spec sk-878.3.1,
// "API surface": getMe, the projects, their members and components). Every
// operation calls the writers and readers the MCP tool of the same name
// calls and refuses with the same code (R-3, INV-4); the status comes from
// the code's class and the origin of the failure (problem.go).
//
// {project} in a path is a unique handle only — the UUID, the prefix (case
// does not matter) or an alias, in that order, among the projects the
// caller may access inside the key's scope (projects.Store.ResolvePathHandle).
// A name, a repository URL or a fuzzy string is resolved by resolveProject,
// after which the client works with the UUID.

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/errcode"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// The operationIds of the project resources.
const (
	OperationGetMe               = "getMe"
	OperationListProjects        = "listProjects"
	OperationResolveProject      = "resolveProject"
	OperationCreateProject       = "createProject"
	OperationGetProject          = "getProject"
	OperationUpdateProject       = "updateProject"
	OperationListProjectMembers  = "listProjectMembers"
	OperationPutProjectMember    = "putProjectMember"
	OperationDeleteProjectMember = "deleteProjectMember"
	OperationListComponents      = "listComponents"
	OperationCreateComponent     = "createComponent"
	OperationUpdateComponent     = "updateComponent"
	OperationArchiveComponent    = "archiveComponent"
)

// projectListLimit is the range of limit= of listProjects: the MCP tool's
// default and the store's ceiling.
var projectListLimit = Limit{Min: 1, Max: projects.ListProjectsMaxLimit, Default: tools.ProjectListDefaultLimit}

// projectsAPI serves the project resources.
type projectsAPI struct {
	pool              *pgxpool.Pool
	projects          *projects.Store
	users             *users.Store
	embedder          projects.QueryEmbedder
	defaultVisibility string
}

// callerOf is the owner of the request's key; the request chain set it.
func callerOf(r *http.Request) uuid.UUID {
	id, _ := auth.UserIDFrom(r.Context())
	return id
}

// writeCode answers a refusal with code from origin, its extensions the
// members the MCP tool adds to the same refusal.
func writeCode(w http.ResponseWriter, code errcode.Code, origin Origin, detail string, ext map[string]any) {
	WriteProblem(w, Problem{Code: code, Origin: origin, Detail: detail, Extensions: ext})
}

// pathProject resolves the {project} of the path. A handle that does not
// name a project the caller may access in the key's scope — a reserved word
// included, whose project is addressed by its UUID — is answered 404
// project_not_found, as for a missing one (INV-2).
func (a *projectsAPI) pathProject(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "project")
	handle, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(handle) == "" {
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", nil)
		return uuid.Nil, false
	}
	if _, perr := uuid.Parse(handle); perr != nil && projects.IsReservedHandle(handle) {
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", nil)
		return uuid.Nil, false
	}
	id, err := a.projects.ResolvePathHandle(r.Context(), handle, callerOf(r))
	if errors.Is(err, projects.ErrNotFound) {
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", nil)
		return uuid.Nil, false
	}
	if err != nil {
		WriteInternalError(w, r, "resolving the project of the path", err)
		return uuid.Nil, false
	}
	return id, true
}

// requireOwner is the gate of the operations that manage a project: its
// owner, the owner of its space or the instance admin (US-6) — the gate of
// the MCP tools. A project out of reach is 404, as for a missing one.
func (a *projectsAPI) requireOwner(w http.ResponseWriter, r *http.Request, projectID uuid.UUID, what string) bool {
	_, err := projects.EnsureProjectOwner(r.Context(), a.pool, projectID, callerOf(r))
	switch {
	case err == nil:
		return true
	case errors.Is(err, projects.ErrProjectNotVisible):
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", nil)
	case errors.Is(err, projects.ErrProjectNotOwner):
		writeCode(w, errcode.Forbidden, OriginDomain,
			"only the project owner, the space owner or the instance admin can "+what, nil)
	default:
		WriteInternalError(w, r, "checking the project owner", err)
	}
	return false
}

// requireVisible is the gate of the reads of a project.
func (a *projectsAPI) requireVisible(w http.ResponseWriter, r *http.Request, projectID uuid.UUID) bool {
	_, err := projects.EnsureProjectVisible(r.Context(), a.pool, projectID, callerOf(r))
	switch {
	case err == nil:
		return true
	case errors.Is(err, projects.ErrProjectNotVisible):
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", nil)
	default:
		WriteInternalError(w, r, "checking the project's visibility", err)
	}
	return false
}

// writeProjectsValidation answers a projects.ValidationError: prefix_invalid
// for the prefix rule, validation_error with the store's own code otherwise —
// the refusal of the MCP tools (projectsValidationToolError).
func writeProjectsValidation(w http.ResponseWriter, err error) {
	if ve, ok := projects.IsValidationError(err); ok {
		if ve.Code == projects.CodePrefixInvalid {
			writeCode(w, errcode.PrefixInvalid, OriginDomain, ve.Message, nil)
			return
		}
		writeCode(w, errcode.ValidationError, OriginDomain, ve.Message, map[string]any{"validation": ve.Code})
		return
	}
	writeCode(w, errcode.ValidationError, OriginDomain, err.Error(), nil)
}

// writeSpacesRefusal answers a refusal of the spaces registry with its own
// code and members, as the MCP tools do; false when err is not one.
func writeSpacesRefusal(w http.ResponseWriter, err error) bool {
	var ref *spaces.Refusal
	if !errors.As(err, &ref) {
		return false
	}
	code, ok := errcode.Lookup(ref.Code)
	if !ok {
		return false
	}
	writeCode(w, code, OriginDomain, ref.Message, ref.Extra)
	return true
}

// gitRepoInput is one repository of a project in a request.
type gitRepoInput struct {
	// Name of the repository within the project. Required.
	Name string `json:"name"`
	// URL is the git remote (https, ssh or git). Required.
	URL string `json:"url"`
	// DefaultBranch is the default branch, e.g. "main". Optional.
	DefaultBranch string `json:"defaultBranch,omitempty"`
}

// repoArgs validates repos the way the MCP tools do: a non-empty name and
// URL each, trimmed. field names the list in a refusal.
func repoArgs(field string, repos []gitRepoInput) ([]projects.AddRepoArgs, error) {
	out := make([]projects.AddRepoArgs, 0, len(repos))
	for i, rp := range repos {
		if strings.TrimSpace(rp.Name) == "" {
			return nil, fmt.Errorf("%s[%d].name is required", field, i)
		}
		if strings.TrimSpace(rp.URL) == "" {
			return nil, fmt.Errorf("%s[%d].url is required", field, i)
		}
		out = append(out, projects.AddRepoArgs{
			Name:          strings.TrimSpace(rp.Name),
			URL:           strings.TrimSpace(rp.URL),
			DefaultBranch: strings.TrimSpace(rp.DefaultBranch),
		})
	}
	return out, nil
}

// checkKind validates the optional kind of a write. The project resources
// record no origin, so a valid kind has no further effect on them; an
// invalid one is refused like on every write (400).
func checkKind(w http.ResponseWriter, kind *string) bool {
	if _, err := ParseKind(kind); err != nil {
		WriteParseError(w, err)
		return false
	}
	return true
}

// getMe answers who the key belongs to.
//
// @Summary Get the caller
// @Description Returns the identity the API key acts as: the user (id, email, username, name, role), the executor of this request (agent: sessionId is the lock holder api:<keyId>[:<workerId>] of X-Skein-Worker-Id, with when it was first and last seen), the projects the user sees within the key's scope (at most 100, by name, with the user's role and the space; projectsTotal counts all of them and projectsTruncated marks the cut), the spaces the user is in and whether the user is an instance admin.
// @Description
// @Description The same answer as the MCP tool skein_whoami. Any key may call it, whatever its rights. Read-only; the request still counts as activity of its executor.
// @Tags Me
// @Produce json
// @Security SkeinApiKey
// @Success 200 {object} tools.WhoamiResponse "The caller"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked, or its owner is deactivated (code unauthenticated)"
// @Failure 404 {object} ProblemDocument "The key's owner no longer exists (code user_not_found)"
// @Failure 429 {object} ProblemDocument "Request limit of the key or failed-authentication limit of the address exceeded (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/me [get]
// @ID getMe
func (a *projectsAPI) getMe(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	resp, err := tools.LoadWhoami(r.Context(), a.pool, callerOf(r))
	if errors.Is(err, tools.ErrWhoamiUserGone) {
		writeCode(w, errcode.UserNotFound, OriginPath, "the user of the API key no longer exists", nil)
		return
	}
	if err != nil {
		WriteInternalError(w, r, "loading the caller", err)
		return
	}
	WriteJSON(w, r, http.StatusOK, resp)
}

// listProjects lists the projects the caller sees.
//
// @Summary List projects
// @Description Returns a page of the projects the key's owner can see within the key's scope: the instance admin sees every project but other users' personal spaces; anyone else the projects of the spaces he takes part in that he has access to, his own personal space and the projects he was added to. Each item carries the project's settings, its space and visibility, the caller's role and the issue counters.
// @Description
// @Description Without q the items are ordered by recent activity; with q they are ordered by relevance (full-text search over the name, the description, the prefix and the aliases, reranked by meaning when the instance has an embedder). The page is a keyset cursor: pass nextCursor back as cursor while hasMore is true.
// @Description
// @Description The same set as the MCP tool skein_project_list, whose rows cut the description; this list carries it whole. Read-only.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[read]
// @Param archived query string false "Which projects: false (active, the default), true (archived only) or all" Enums(false, true, all)
// @Param q query string false "Free-form search query; orders the items by relevance"
// @Param limit query int false "Page size, 1..100 (default 50); a value outside the range is refused, never clamped"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[projects.ProjectListItem] "A page of projects"
// @Failure 400 {object} ProblemDocument "Unknown parameter, limit out of range, a cursor this list did not issue, an unknown field in fields (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects [get]
// @ID listProjects
func (a *projectsAPI) listProjects(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "archived", "q", "limit", "cursor") {
		return
	}
	q := r.URL.Query()
	archived := "false"
	if vals, ok := q["archived"]; ok {
		if len(vals) != 1 || (vals[0] != "false" && vals[0] != "true" && vals[0] != "all") {
			WriteParseError(w, errors.New("archived must be one of false, true, all"))
			return
		}
		archived = vals[0]
	}
	limit, err := ParseLimit(r, projectListLimit)
	if err != nil {
		WriteParseError(w, err)
		return
	}
	args := projects.ListProjectsArgs{
		CallerID: callerOf(r),
		Archived: archived,
		Query:    strings.TrimSpace(q.Get("q")),
		Embedder: a.embedder,
		Limit:    limit,
	}
	if c := q.Get("cursor"); c != "" {
		cur, cerr := projects.DecodeListCursor(c)
		if cerr != nil {
			WriteParseError(w, ErrBadCursor)
			return
		}
		args.Cursor = &cur
	}
	res, err := a.projects.ListProjects(r.Context(), args)
	if err != nil {
		WriteInternalError(w, r, "listing projects", err)
		return
	}
	items := res.Items
	if items == nil {
		items = []projects.ProjectListItem{}
	}
	WriteJSON(w, r, http.StatusOK, List[projects.ProjectListItem]{Items: items, NextCursor: res.NextCursor, HasMore: res.HasMore})
}

// ResolveCandidate is one project a handle resolves to.
type ResolveCandidate struct {
	// ID is the project's UUID; address the project by it.
	ID string `json:"id"`
	// Name is the project's name.
	Name string `json:"name"`
	// IDPrefix is the project's prefix of issue ids.
	IDPrefix string `json:"idPrefix"`
	// Score is the relevance of the candidate, 1 for an exact match.
	Score float64 `json:"score"`
	// MatchedVia is the form that matched: id, id_prefix, alias, name,
	// repo_url or search.
	MatchedVia string `json:"matchedVia"`
}

// ResolveResult is the answer of resolveProject.
type ResolveResult struct {
	// Match is the project the handle names when the resolver is
	// confident; null otherwise.
	Match *ResolveCandidate `json:"match"`
	// Candidates are up to 10 alternatives when there is no confident
	// match.
	Candidates []ResolveCandidate `json:"candidates"`
}

// resolveProject resolves a free-form handle into a project.
//
// @Summary Resolve a project handle
// @Description Resolves a free-form string naming a project — its UUID, prefix, alias, name, a repository URL or a fuzzy, mistyped name — among the projects the key's owner may access within the key's scope. Exact forms are tried first, in the order UUID → prefix → alias → name: a hit on a unique form wins; the exact name of several accessible projects is refused as ambiguous. Then an exact repository URL, then lexical relevance, where a single confident candidate is the match.
// @Description
// @Description Returns match (the project) or match: null with up to 10 candidates. No match at all is not an error: match is null and candidates are empty. This path is always this operation: a project whose prefix or alias is "resolve" is addressed by its UUID. The same answer as the MCP tool skein_project_resolve. Read-only.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[read]
// @Param q query string true "The handle to resolve"
// @Param fields query string false "Comma-separated fields of the answer to return"
// @Success 200 {object} ResolveResult "The match or the candidates"
// @Failure 400 {object} ProblemDocument "q missing, an unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "The name matches several accessible projects (code ambiguous_project, with their candidates: prefix, name and space of each)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/resolve [get]
// @ID resolveProject
func (a *projectsAPI) resolveProject(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "q") {
		return
	}
	vals, ok := r.URL.Query()["q"]
	if !ok || len(vals) != 1 {
		WriteParseError(w, errors.New("q is required, once"))
		return
	}
	q := vals[0]
	res, err := a.projects.Resolve(r.Context(), q, callerOf(r))
	if err != nil {
		WriteInternalError(w, r, "resolving a project handle", err)
		return
	}
	if res.Ambiguous {
		writeCode(w, errcode.AmbiguousProject, OriginDomain,
			"the name matches several projects; name one by its prefix",
			map[string]any{"q": strings.TrimSpace(q), "candidates": projects.AmbiguousCandidates(res.Candidates)})
		return
	}
	out := ResolveResult{Candidates: make([]ResolveCandidate, 0, len(res.Candidates))}
	if m := res.Match; m != nil {
		out.Match = &ResolveCandidate{ID: m.ID.String(), Name: m.Name, IDPrefix: m.IDPrefix, Score: m.Similarity, MatchedVia: m.MatchedVia}
	}
	for _, c := range res.Candidates {
		out.Candidates = append(out.Candidates, ResolveCandidate{ID: c.ID.String(), Name: c.Name, IDPrefix: c.IDPrefix, Score: c.Similarity, MatchedVia: c.MatchedVia})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// createProjectBody is the body of createProject.
type createProjectBody struct {
	// Name is the display name, 1..200 characters after trim. Required.
	Name string `json:"name"`
	// IDPrefix is the prefix of the project's issue ids, ^[a-z][a-z0-9_-]{0,49}$
	// (case does not matter). Omitted: the first free prefix derived from the
	// name. It may not repeat another project's prefix or alias, nor the
	// reserved word "resolve".
	IDPrefix *string `json:"idPrefix,omitempty"`
	// DefaultIDMode is the id-generation mode of the project's issues:
	// counter (default), hash or custom.
	DefaultIDMode *string `json:"defaultIdMode,omitempty"`
	// Description is free-form markdown.
	Description *string `json:"description,omitempty"`
	// Space is the short id of the space of the project (getMe lists them).
	// Omitted: the one space the caller may create projects in.
	Space *string `json:"space,omitempty"`
	// Visibility: space (every member of the space sees the project) or
	// members (only its members and the space owner). Omitted: the
	// installation's default; always members in a personal space.
	Visibility *string `json:"visibility,omitempty"`
	// Aliases of the project; each unique across projects, and none may
	// repeat another project's prefix or the reserved word "resolve".
	Aliases []string `json:"aliases,omitempty"`
	// GitRepos are the repositories of the project.
	GitRepos []gitRepoInput `json:"gitRepos,omitempty"`
	// Kind of the write: directed or autonomous. A project records no
	// origin; the value is checked like on every write.
	Kind *string `json:"kind,omitempty"`
}

// createProject creates a project.
//
// @Summary Create a project
// @Description Creates a project in a space; the key's owner becomes its owner. The same operation as the MCP tool skein_project_create:
// @Description 1. The name and the optional prefix, id mode, description, aliases and repositories are validated; an omitted prefix is the first free one derived from the name.
// @Description 2. The space is the one named, or the only space the owner may create projects in (several — space_required with their list). A key limited to spaces creates only inside them; a key limited to projects cannot create one (insufficient_scope, requiredScope).
// @Description 3. In one transaction, under the space's lock: the owner must still take part in the space, the space's project quota must allow one more, the prefix must be free (no project, deleted project, alias or reserved word holds it), the name free in the space, every alias free; then the project, the owner's membership, the members of a space-visible project and the aliases and repositories are written, with the project.created event. Any refusal writes nothing.
// @Description
// @Description Side effects: project.created event; every member of the space who sees the project gains it in live updates. The Location header names the new project.
// @Tags Projects
// @Accept json
// @Produce json
// @Security SkeinApiKey[manage]
// @Param body body createProjectBody true "The project"
// @Success 201 {object} projects.ProjectDTO "The project created"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage or is limited to projects (code insufficient_scope); an external user (code external_cannot_create_project); the space is seen only as the instance admin (code not_space_member)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "Validation of a value (code validation_error, prefix_invalid); prefix taken (code prefix_taken, with suggestions); name taken in the space (code project_name_taken); alias taken (code id_taken); space quota reached (code project_quota); space unknown (code space_not_found); several spaces possible (code space_required); concurrent change of the space (code concurrent_change)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects [post]
// @ID createProject
func (a *projectsAPI) createProject(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body createProjectBody
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	ctx := r.Context()
	callerID := callerOf(r)
	role, _ := auth.UserRoleFrom(ctx)

	name, err := projects.ValidateProjectName(body.Name)
	if err != nil {
		writeProjectsValidation(w, err)
		return
	}
	idPrefix := ""
	if body.IDPrefix != nil {
		idPrefix = strings.TrimSpace(*body.IDPrefix)
	}
	if idPrefix == "" {
		sugg, serr := a.projects.SuggestPrefixes(ctx, name)
		if serr != nil {
			a.writeCreateError(w, r, serr, name)
			return
		}
		if len(sugg) == 0 {
			a.writeCreateError(w, r, projects.ErrPrefixTaken, name)
			return
		}
		idPrefix = sugg[0]
	} else if idPrefix, err = a.projects.CheckPrefixRules(idPrefix); err != nil {
		writeProjectsValidation(w, err)
		return
	}
	mode := "counter"
	if body.DefaultIDMode != nil && strings.TrimSpace(*body.DefaultIDMode) != "" {
		mode = strings.TrimSpace(*body.DefaultIDMode)
	}
	if _, err := projects.ValidateIDMode(mode); err != nil {
		writeProjectsValidation(w, err)
		return
	}
	description := ""
	if body.Description != nil {
		description = *body.Description
	}
	if err := projects.ValidateProse("description", description); err != nil {
		writeProjectsValidation(w, err)
		return
	}
	aliases := make([]string, 0, len(body.Aliases))
	for _, raw := range body.Aliases {
		canon, verr := projects.ValidateAlias(raw)
		if verr != nil {
			writeProjectsValidation(w, verr)
			return
		}
		aliases = append(aliases, canon)
	}
	repos, err := repoArgs("gitRepos", body.GitRepos)
	if err != nil {
		writeCode(w, errcode.ValidationError, OriginDomain, err.Error(), nil)
		return
	}
	visibility := ""
	if body.Visibility != nil {
		visibility = strings.TrimSpace(*body.Visibility)
	}
	if visibility != "" && visibility != spaces.VisibilitySpace && visibility != spaces.VisibilityMembers {
		writeCode(w, errcode.ValidationError, OriginDomain, "visibility must be space or members", map[string]any{"field": "visibility"})
		return
	}
	// A key limited to spaces creates projects only in them (INV-2); the
	// key right already refused a key limited to projects.
	var within []uuid.UUID
	if g, ok := auth.KeyGrantFrom(ctx); ok && g.Scope.Mode == apikeys.ScopeSpaces {
		within = append([]uuid.UUID{}, g.Scope.Spaces...)
	}
	spaceArg := ""
	if body.Space != nil {
		spaceArg = strings.TrimSpace(*body.Space)
	}
	space, err := spaces.ChooseForProjectWithin(ctx, a.pool, callerID, spaceArg, within)
	if err != nil {
		if writeSpacesRefusal(w, err) {
			return
		}
		a.writeCreateError(w, r, err, name)
		return
	}
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerID)
	dto, err := a.projects.CreateProject(ctx, projects.CreateProjectArgs{
		Name:             name,
		IDPrefix:         idPrefix,
		Description:      description,
		DefaultIDMode:    mode,
		CreatorID:        callerID,
		CreatorRole:      role,
		CreatorName:      actor.Username,
		Actor:            actor,
		Aliases:          aliases,
		AliasAddedByKind: "agent",
		GitRepos:         repos,
		SpaceID:          space.ID,
		Visibility:       spaces.ProjectVisibility(space.Kind, visibility, a.defaultVisibility),
	})
	if errors.Is(err, projects.ErrNotSpaceMember) {
		writeCode(w, errcode.NotSpaceMember, OriginDomain,
			"add yourself as a member of the space to create projects there", map[string]any{"spaceId": space.ShortID})
		return
	}
	if err != nil {
		a.writeCreateError(w, r, err, name)
		return
	}
	w.Header().Set("Location", PathPrefix+"projects/"+dto.ID.String())
	WriteJSON(w, r, http.StatusCreated, dto)
}

// writeCreateError answers a refusal of creating a project with the code of
// the MCP tool (mapProjectCreateError).
func (a *projectsAPI) writeCreateError(w http.ResponseWriter, r *http.Request, err error, name string) {
	if ve, ok := projects.IsValidationError(err); ok {
		writeCode(w, errcode.ValidationError, OriginDomain, ve.Message, map[string]any{"validation": ve.Code})
		return
	}
	var quota *projects.ErrProjectQuota
	if errors.As(err, &quota) {
		writeCode(w, errcode.ProjectQuota, OriginDomain,
			fmt.Sprintf("the space has reached its project limit: %d of %d", quota.ProjectCount, quota.ProjectQuota),
			map[string]any{"spaceId": quota.SpaceID, "projectCount": quota.ProjectCount, "projectQuota": quota.ProjectQuota})
		return
	}
	var conflict *projects.ErrConflict
	switch {
	case errors.Is(err, projects.ErrPrefixTaken):
		alts, aerr := a.projects.PrefixAlternatives(r.Context(), name)
		if aerr != nil || alts == nil {
			alts = []string{}
		}
		writeCode(w, errcode.PrefixTaken, OriginDomain, "this prefix is taken", map[string]any{"suggestions": alts})
	case errors.Is(err, projects.ErrExternalCannotCreateProject):
		writeCode(w, errcode.ExternalCannotCreateProject, OriginDomain, "external users cannot create projects", nil)
	case errors.Is(err, spaces.ErrSpaceNotFound):
		writeCode(w, errcode.SpaceNotFound, OriginBody, "space not found", nil)
	case errors.Is(err, projects.ErrNotSpaceMember):
		writeCode(w, errcode.NotSpaceMember, OriginDomain, "add yourself as a member of the space to create projects there", nil)
	case errors.Is(err, spaces.ErrConcurrentChange):
		writeCode(w, errcode.ConcurrentChange, OriginDomain, "the space was changed concurrently; nothing was changed, retry the request", nil)
	case errors.As(err, &conflict):
		writeProjectConflict(w, conflict, name)
	default:
		WriteInternalError(w, r, "creating a project", err)
	}
}

// writeProjectConflict answers a unique violation of a project write: the
// name in the space, an alias, another constraint.
func writeProjectConflict(w http.ResponseWriter, conflict *projects.ErrConflict, name string) {
	switch {
	case strings.Contains(conflict.Constraint, "name_lower"):
		var ext map[string]any
		if name != "" {
			ext = map[string]any{"name": name}
		}
		writeCode(w, errcode.ProjectNameTaken, OriginDomain, "a project with this name already exists in the space", ext)
	case strings.Contains(conflict.Constraint, "project_aliases"):
		writeCode(w, errcode.IDTaken, OriginDomain, "one of the aliases is already taken", map[string]any{"constraint": conflict.Constraint})
	default:
		writeCode(w, errcode.IDTaken, OriginDomain, "unique constraint violation", map[string]any{"constraint": conflict.Constraint})
	}
}

// getProject reads one project.
//
// @Summary Get a project
// @Description Returns the project named by the path — its UUID, prefix (case does not matter) or alias, matched in that order among the projects the key's owner may access within the key's scope — with its settings, space, visibility, aliases, repositories and issue counters (the caller's own counters included). A project out of reach answers exactly as a missing one. A name is not accepted here: resolve it with resolveProject first. Read-only.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[read]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param fields query string false "Comma-separated fields to return (plus id)"
// @Success 200 {object} projects.ProjectDTO "The project"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project} [get]
// @ID getProject
func (a *projectsAPI) getProject(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok || !a.requireVisible(w, r, projectID) {
		return
	}
	dto, err := a.projects.GetProjectWithStats(r.Context(), projectID, callerOf(r))
	if errors.Is(err, projects.ErrNotFound) {
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", nil)
		return
	}
	if err != nil {
		WriteInternalError(w, r, "reading a project", err)
		return
	}
	WriteJSON(w, r, http.StatusOK, dto)
}

// updateProjectBody is the body of updateProject. An absent member is left
// as it is.
type updateProjectBody struct {
	// Name replaces the name, 1..200 characters after trim.
	Name *string `json:"name,omitempty"`
	// Description replaces the description; an empty string clears it.
	Description *string `json:"description,omitempty"`
	// GitlabGroup replaces the GitLab group binding; an empty string clears it.
	GitlabGroup *string `json:"gitlabGroup,omitempty"`
	// Visibility switches who sees the project: space or members.
	Visibility *string `json:"visibility,omitempty"`
	// Space is not supported: a project never moves to another space. Any
	// value but null is refused with space_change_forbidden.
	Space *string `json:"space,omitempty"`
	// Archived: true archives the project, false restores it; a no-op when
	// it is already so.
	Archived *bool `json:"archived,omitempty"`
	// AliasesAdd are aliases to add; each must be free and may not repeat
	// another project's prefix or the reserved word "resolve".
	AliasesAdd []string `json:"aliasesAdd,omitempty"`
	// AliasesRemove are aliases of this project to remove.
	AliasesRemove []string `json:"aliasesRemove,omitempty"`
	// GitReposAdd are repositories to add; the server generates their ids.
	GitReposAdd []gitRepoInput `json:"gitReposAdd,omitempty"`
	// GitReposRemove are the UUIDs of repositories to remove.
	GitReposRemove []string `json:"gitReposRemove,omitempty"`
	// Kind of the write: directed or autonomous. A project records no
	// origin; the value is checked like on every write.
	Kind *string `json:"kind,omitempty"`
}

// updateProject changes a project in one atomic command.
//
// @Summary Update a project
// @Description Changes the project named by the path in one command — the same operation as the MCP tool skein_project_update. Allowed to the project's owner, the owner of its space and the instance admin.
// @Description 1. Every part of the body is validated first; at least one must be present. A space value is refused (a project never moves).
// @Description 2. All changes land in one SERIALIZABLE transaction — name, description, GitLab group, visibility, archive flag, aliases added and removed, repositories added and removed. Any refusal (an alias taken, an alias not on the project, a repository still used by a component) writes nothing.
// @Description
// @Description A project has no version: the request takes no If-Match and the last write wins, as in MCP and the UI. Side effects: the project.* events of what changed; a visibility change gives the project to, or takes it from, the members of the space.
// @Tags Projects
// @Accept json
// @Produce json
// @Security SkeinApiKey[manage]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param body body updateProjectBody true "The changes"
// @Success 200 {object} projects.ProjectDTO "The project after the change"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage (code insufficient_scope); the owner is not the project's owner, the space owner or an admin (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "Nothing to change or an invalid value (code validation_error); an alias taken (code id_taken); the name taken in the space (code project_name_taken); a space given (code space_change_forbidden); concurrent change (code concurrent_change)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project} [patch]
// @ID updateProject
func (a *projectsAPI) updateProject(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok {
		return
	}
	var body updateProjectBody
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	if !a.requireOwner(w, r, projectID, "update a project") {
		return
	}
	patch, ok := projectPatch(w, body)
	if !ok {
		return
	}
	ctx := r.Context()
	callerID := callerOf(r)
	role, _ := auth.UserRoleFrom(ctx)
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerID)
	result, err := a.projects.PatchProject(ctx, projectID, patch, callerID, role, actor)
	if err != nil {
		writeUpdateError(w, r, err, projectID)
		return
	}
	WriteJSON(w, r, http.StatusOK, result.Project)
}

// projectPatch validates the body of updateProject into the store's patch,
// in the order and with the refusals of the MCP tool.
func projectPatch(w http.ResponseWriter, body updateProjectBody) (projects.PatchProjectArgs, bool) {
	var patch projects.PatchProjectArgs
	if body.Space != nil {
		writeCode(w, errcode.SpaceChangeForbidden, OriginDomain, "a project cannot be moved to another space", nil)
		return patch, false
	}
	if body.Visibility != nil {
		v := strings.TrimSpace(*body.Visibility)
		if v != spaces.VisibilitySpace && v != spaces.VisibilityMembers {
			writeCode(w, errcode.ValidationError, OriginDomain, "visibility must be space or members", map[string]any{"field": "visibility"})
			return patch, false
		}
		patch.Visibility = &v
	}
	if body.Name != nil {
		canonical, err := projects.ValidateProjectName(*body.Name)
		if err != nil {
			writeProjectsValidation(w, err)
			return patch, false
		}
		patch.Name = &canonical
	}
	if body.Description != nil {
		if err := projects.ValidateProse("description", *body.Description); err != nil {
			writeProjectsValidation(w, err)
			return patch, false
		}
		patch.Description = body.Description
	}
	if body.GitlabGroup != nil {
		v := strings.TrimSpace(*body.GitlabGroup)
		patch.GitlabGroup = &v
	}
	patch.Archived = body.Archived
	for _, raw := range body.AliasesAdd {
		canon, err := projects.ValidateAlias(raw)
		if err != nil {
			writeProjectsValidation(w, err)
			return patch, false
		}
		patch.AliasesAdd = append(patch.AliasesAdd, canon)
	}
	for _, raw := range body.AliasesRemove {
		canon, err := projects.ValidateAlias(raw)
		if err != nil {
			writeProjectsValidation(w, err)
			return patch, false
		}
		patch.AliasesRemove = append(patch.AliasesRemove, canon)
	}
	repos, err := repoArgs("gitReposAdd", body.GitReposAdd)
	if err != nil {
		writeCode(w, errcode.ValidationError, OriginDomain, err.Error(), nil)
		return patch, false
	}
	if len(repos) > 0 {
		patch.GitReposAdd = repos
	}
	for i, raw := range body.GitReposRemove {
		id, perr := uuid.Parse(strings.TrimSpace(raw))
		if perr != nil {
			writeCode(w, errcode.ValidationError, OriginDomain, "gitReposRemove entry is not a valid UUID",
				map[string]any{"index": i, "value": raw})
			return patch, false
		}
		patch.GitReposRemove = append(patch.GitReposRemove, id)
	}
	if patch.Name == nil && patch.Description == nil && patch.GitlabGroup == nil && patch.Archived == nil &&
		len(patch.AliasesAdd) == 0 && len(patch.AliasesRemove) == 0 &&
		len(patch.GitReposAdd) == 0 && len(patch.GitReposRemove) == 0 && patch.Visibility == nil {
		writeCode(w, errcode.ValidationError, OriginDomain,
			"at least one of name, description, gitlabGroup, archived, visibility, aliasesAdd, aliasesRemove, gitReposAdd, gitReposRemove must be present", nil)
		return patch, false
	}
	return patch, true
}

// writeUpdateError answers a refusal of PatchProject with the code of the
// MCP tool (mapProjectUpdateError).
func writeUpdateError(w http.ResponseWriter, r *http.Request, err error, projectID uuid.UUID) {
	if ve, ok := projects.IsValidationError(err); ok {
		writeCode(w, errcode.ValidationError, OriginDomain, ve.Message, map[string]any{"validation": ve.Code})
		return
	}
	var aliasTaken *projects.ErrAliasTaken
	var repoInUse *projects.ErrRepoInUse
	var conflict *projects.ErrConflict
	if alias, ok := projects.IsAliasMissing(err); ok {
		writeCode(w, errcode.ValidationError, OriginDomain, "alias not present on this project", map[string]any{"alias": alias})
		return
	}
	if repoID, ok := projects.IsRepoMissing(err); ok {
		writeCode(w, errcode.ValidationError, OriginDomain, "repo not found in project", map[string]any{"repoId": repoID.String()})
		return
	}
	switch {
	case errors.Is(err, projects.ErrNotFound):
		writeCode(w, errcode.ProjectNotFound, OriginPath, "project does not exist or is not visible", map[string]any{"projectId": projectID.String()})
	case errors.Is(err, spaces.ErrConcurrentChange):
		writeCode(w, errcode.ConcurrentChange, OriginDomain, "the project was changed concurrently; nothing was changed, retry the request", nil)
	case errors.As(err, &aliasTaken):
		writeCode(w, errcode.IDTaken, OriginDomain, "one of the aliases is already taken", map[string]any{"alias": aliasTaken.Alias})
	case errors.As(err, &repoInUse):
		writeCode(w, errcode.ValidationError, OriginDomain, "repo is still referenced by component(s); detach paths first",
			map[string]any{"components": repoInUse.Components})
	case errors.As(err, &conflict):
		writeProjectConflict(w, conflict, "")
	default:
		WriteInternalError(w, r, "updating a project", err)
	}
}

// listProjectMembers lists the members of a project.
//
// @Summary List the members of a project
// @Description Returns every member of the project named by the path with their role (owner or member) and how they take part (added manually, through the space or both). The list is not paged: nextCursor is always null and hasMore false. The same set as the MCP tool skein_project_members, action list. Read-only.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[read]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param fields query string false "Comma-separated fields of each item to return"
// @Success 200 {object} List[projects.MemberDTO] "The members"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/members [get]
// @ID listProjectMembers
func (a *projectsAPI) listProjectMembers(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok || !a.requireVisible(w, r, projectID) {
		return
	}
	members, err := a.projects.ListMembers(r.Context(), projectID)
	if err != nil {
		WriteInternalError(w, r, "listing members", err)
		return
	}
	if members == nil {
		members = []projects.MemberDTO{}
	}
	WriteJSON(w, r, http.StatusOK, List[projects.MemberDTO]{Items: members})
}

// pathUser resolves the {user} of the path: a user UUID or a username, as
// the MCP tool takes userId or username. An unknown username is 404
// user_not_found.
func (a *projectsAPI) pathUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw, err := url.PathUnescape(chi.URLParam(r, "user"))
	raw = strings.TrimSpace(raw)
	if err != nil || raw == "" {
		writeCode(w, errcode.UserNotFound, OriginPath, "user not found", nil)
		return uuid.Nil, false
	}
	if id, perr := uuid.Parse(raw); perr == nil {
		return id, true
	}
	id, err := a.users.ResolveByUsername(r.Context(), raw)
	if errors.Is(err, users.ErrUserNotFound) {
		writeCode(w, errcode.UserNotFound, OriginPath, "user not found", map[string]any{"username": raw})
		return uuid.Nil, false
	}
	if err != nil {
		WriteInternalError(w, r, "resolving a username", err)
		return uuid.Nil, false
	}
	return id, true
}

// putMemberBody is the body of putProjectMember.
type putMemberBody struct {
	// Role of the member: owner or member. Required.
	Role string `json:"role"`
	// Kind of the write: directed or autonomous. A membership records no
	// origin; the value is checked like on every write.
	Kind *string `json:"kind,omitempty"`
}

// putProjectMember adds a member or changes a member's role.
//
// @Summary Add a project member or change the role
// @Description Makes the user named by the path (UUID or username) a member of the project with the given role, or changes the role of an existing member — the same operation as the MCP tool skein_project_members, action add. Allowed to the project's owner, the owner of its space and the instance admin. An external user can only be a member, never an owner.
// @Description
// @Description Side effects: the project.member_added or project.member_role_changed event; the user gains the project in live updates.
// @Tags Projects
// @Accept json
// @Produce json
// @Security SkeinApiKey[manage]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param user path string true "The user's UUID or username"
// @Param body body putMemberBody true "The role"
// @Success 200 {object} projects.MemberDTO "The membership"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage (code insufficient_scope); the owner may not manage members (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found); no such username (code user_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "role is not owner or member, or an external user as owner (code validation_error); the user id does not exist (code user_not_found); concurrent change (code concurrent_change)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/members/{user} [put]
// @ID putProjectMember
func (a *projectsAPI) putProjectMember(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok {
		return
	}
	var body putMemberBody
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	if !a.requireOwner(w, r, projectID, "add members") {
		return
	}
	targetID, ok := a.pathUser(w, r)
	if !ok {
		return
	}
	if body.Role != "owner" && body.Role != "member" {
		writeCode(w, errcode.ValidationError, OriginDomain, "role must be 'owner' or 'member'", map[string]any{"field": "role"})
		return
	}
	ctx := r.Context()
	callerID := callerOf(r)
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerID)
	dto, err := a.projects.UpsertMember(ctx, projectID, targetID, body.Role, callerID, actor)
	switch {
	case err == nil:
		WriteJSON(w, r, http.StatusOK, dto)
	case errors.Is(err, projects.ErrUserNotFound):
		writeCode(w, errcode.UserNotFound, OriginDomain, "target user does not exist", map[string]any{"userId": targetID.String()})
	case errors.Is(err, projects.ErrExternalCannotBeOwner):
		writeCode(w, errcode.ValidationError, OriginDomain, "external users cannot be project owners", map[string]any{"field": "role"})
	case errors.Is(err, spaces.ErrConcurrentChange):
		writeCode(w, errcode.ConcurrentChange, OriginDomain, "a concurrent membership change won; retry", nil)
	default:
		WriteInternalError(w, r, "adding a member", err)
	}
}

// deleteProjectMember removes a member of a project.
//
// @Summary Remove a project member
// @Description Removes the user named by the path (UUID or username) from the project — the same operation as the MCP tool skein_project_members, action remove. The key's owner may always remove himself (leave the project); removing anyone else is allowed to the project's owner, the owner of its space and the instance admin. A member who sees the project through its space cannot be removed here: switch the project's visibility to members or remove them from the space.
// @Description
// @Description Side effects: the project.member_removed event; the user loses the project in live updates. Answers 204 without a body.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[manage]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param user path string true "The user's UUID or username"
// @Success 204 "Removed"
// @Failure 400 {object} ProblemDocument "Unknown parameter, a body that is not a JSON object (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage (code insufficient_scope); the owner may not remove others (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found); no such username (code user_not_found)"
// @Failure 422 {object} ProblemDocument "The user is not a member (code member_not_found); the member takes part through the space (code member_via_space); concurrent change (code concurrent_change)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/members/{user} [delete]
// @ID deleteProjectMember
func (a *projectsAPI) deleteProjectMember(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind *string `json:"kind,omitempty"`
	}
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	targetID, ok := a.pathUser(w, r)
	if !ok {
		return
	}
	callerID := callerOf(r)
	if targetID == callerID {
		if !a.requireVisible(w, r, projectID) {
			return
		}
	} else if !a.requireOwner(w, r, projectID, "remove members") {
		return
	}
	ctx := r.Context()
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerID)
	err := a.projects.RemoveMember(ctx, projectID, targetID, actor)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, projects.ErrMemberViaSpace):
		writeCode(w, errcode.MemberViaSpace, OriginDomain,
			"the member has access to the project through the space; switch the project's visibility to members or remove them from the space",
			map[string]any{"userId": targetID.String()})
	case errors.Is(err, spaces.ErrConcurrentChange):
		writeCode(w, errcode.ConcurrentChange, OriginDomain, "a concurrent membership change won; retry", nil)
	case errors.Is(err, projects.ErrNotFound):
		writeCode(w, errcode.MemberNotFound, OriginDomain, "user is not a member of this project", map[string]any{"userId": targetID.String()})
	default:
		WriteInternalError(w, r, "removing a member", err)
	}
}

// listComponents lists the components of a project.
//
// @Summary List the components of a project
// @Description Returns the components of the project named by the path with their issue counters — the same set as the MCP tool skein_component, action list. The list is not paged: nextCursor is always null and hasMore false. Read-only.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[read]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param archived query string false "Which components: false (active, the default), true (archived only) or all" Enums(false, true, all)
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[projects.ComponentDTO] "The components"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, archived outside false|true|all (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/components [get]
// @ID listComponents
func (a *projectsAPI) listComponents(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "archived") {
		return
	}
	archived := "false"
	if vals, ok := r.URL.Query()["archived"]; ok {
		if len(vals) != 1 || (vals[0] != "false" && vals[0] != "true" && vals[0] != "all") {
			WriteParseError(w, errors.New("archived must be one of false, true, all"))
			return
		}
		archived = vals[0]
	}
	projectID, ok := a.pathProject(w, r)
	if !ok || !a.requireVisible(w, r, projectID) {
		return
	}
	username, _ := auth.UsernameFrom(r.Context())
	items, err := a.projects.ListComponents(r.Context(), projectID, archived, callerOf(r), username)
	if err != nil {
		WriteInternalError(w, r, "listing components", err)
		return
	}
	if items == nil {
		items = []projects.ComponentDTO{}
	}
	WriteJSON(w, r, http.StatusOK, List[projects.ComponentDTO]{Items: items})
}

// componentBody is the body of createComponent and updateComponent.
type componentBody struct {
	// Name of the component, unique in the project. Required on create.
	Name *string `json:"name,omitempty"`
	// Description is free-form markdown.
	Description *string `json:"description,omitempty"`
	// Kind of the write: directed or autonomous. A component records no
	// origin; the value is checked like on every write.
	Kind *string `json:"kind,omitempty"`
}

// createComponent creates a component of a project.
//
// @Summary Create a component
// @Description Creates a component of the project named by the path — the same operation as the MCP tool skein_component, action create. Allowed to the project's owner, the owner of its space and the instance admin. The name is unique in the project.
// @Description
// @Description Side effects: the component.created event. The Location header names the new component.
// @Tags Projects
// @Accept json
// @Produce json
// @Security SkeinApiKey[manage]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param body body componentBody true "The component"
// @Success 201 {object} projects.ComponentDTO "The component created"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage (code insufficient_scope); the owner may not manage the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "Name missing or invalid (code validation_error); name taken in the project (code id_taken)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/components [post]
// @ID createComponent
func (a *projectsAPI) createComponent(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok {
		return
	}
	var body componentBody
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	if !a.requireOwner(w, r, projectID, "manage components") {
		return
	}
	if body.Name == nil {
		writeCode(w, errcode.ValidationError, OriginDomain, "name is required", nil)
		return
	}
	name := strings.TrimSpace(*body.Name)
	if name == "" {
		writeCode(w, errcode.ValidationError, OriginDomain, "name must be non-empty after trim", nil)
		return
	}
	if err := projects.ValidateProse("name", name); err != nil {
		writeProjectsValidation(w, err)
		return
	}
	description := ""
	if body.Description != nil {
		description = *body.Description
	}
	if err := projects.ValidateProse("description", description); err != nil {
		writeProjectsValidation(w, err)
		return
	}
	ctx := r.Context()
	callerID := callerOf(r)
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerID)
	dto, err := a.projects.CreateComponent(ctx, projectID, callerID,
		projects.CreateComponentArgs{Name: name, Description: description}, actor)
	if err != nil {
		writeComponentError(w, r, err, uuid.Nil, "creating a component")
		return
	}
	w.Header().Set("Location", PathPrefix+"projects/"+projectID.String()+"/components/"+dto.ID.String())
	WriteJSON(w, r, http.StatusCreated, dto)
}

// pathComponent parses the {component} of the path. A malformed id is the
// refusal of the MCP tool for it.
func pathComponent(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "component")
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		writeCode(w, errcode.ValidationError, OriginDomain, "id must be a UUID", map[string]any{"id": raw})
		return uuid.Nil, false
	}
	return id, true
}

// writeComponentError answers a refusal of a component write with the code
// of the MCP tool: a component that is not in the project is
// validation_error, as MCP answers it (it has no component_not_found).
func writeComponentError(w http.ResponseWriter, r *http.Request, err error, componentID uuid.UUID, what string) {
	if ve, ok := projects.IsValidationError(err); ok {
		writeCode(w, errcode.ValidationError, OriginDomain, ve.Message, map[string]any{"validation": ve.Code})
		return
	}
	if errors.Is(err, projects.ErrNotFound) {
		writeCode(w, errcode.ValidationError, OriginDomain, "component not found in this project",
			map[string]any{"componentId": componentID.String()})
		return
	}
	var conflict *projects.ErrConflict
	if errors.As(err, &conflict) {
		if strings.Contains(conflict.Constraint, "components_project_id_name") {
			writeCode(w, errcode.IDTaken, OriginDomain, "a component with this name already exists in the project",
				map[string]any{"constraint": conflict.Constraint})
			return
		}
		writeCode(w, errcode.IDTaken, OriginDomain, "unique constraint violation", map[string]any{"constraint": conflict.Constraint})
		return
	}
	WriteInternalError(w, r, what, err)
}

// updateComponentBody is the body of updateComponent.
type updateComponentBody struct {
	// Name replaces the name; unique in the project.
	Name *string `json:"name,omitempty"`
	// Description replaces the description; an empty string clears it.
	Description *string `json:"description,omitempty"`
	// Archived false restores an archived component, true archives it (as
	// archiveComponent). Sent alone: it is not combined with name or
	// description.
	Archived *bool `json:"archived,omitempty"`
	// Kind of the write: directed or autonomous. A component records no
	// origin; the value is checked like on every write.
	Kind *string `json:"kind,omitempty"`
}

// updateComponent changes a component, restoring it from the archive.
//
// @Summary Update a component
// @Description Changes the name or the description of a component of the project named by the path — the same operation as the MCP tool skein_component, action update — or, with archived alone, archives (true) or restores (false) it, as the action archive does; a component already in the requested state is answered as it is. Allowed to the project's owner, the owner of its space and the instance admin. A component has no version: no If-Match, the last write wins.
// @Description
// @Description Side effects: the component.updated, component.archived or component.unarchived event of the change.
// @Tags Projects
// @Accept json
// @Produce json
// @Security SkeinApiKey[manage]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param component path string true "The component's UUID"
// @Param body body updateComponentBody true "The changes"
// @Success 200 {object} projects.ComponentDTO "The component after the change"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage (code insufficient_scope); the owner may not manage the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "The component is not in the project, the id is not a UUID, a value is invalid, nothing to change, archived combined with another change (code validation_error); name taken (code id_taken)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/components/{component} [patch]
// @ID updateComponent
func (a *projectsAPI) updateComponent(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok {
		return
	}
	var body updateComponentBody
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	if !a.requireOwner(w, r, projectID, "manage components") {
		return
	}
	componentID, ok := pathComponent(w, r)
	if !ok {
		return
	}
	if body.Archived != nil {
		if body.Name != nil || body.Description != nil {
			writeCode(w, errcode.ValidationError, OriginDomain,
				"archived is sent alone: change the name and the description in another request", nil)
			return
		}
		a.setComponentArchived(w, r, projectID, componentID, *body.Archived)
		return
	}
	var args projects.UpdateComponentArgs
	if body.Name != nil {
		v := strings.TrimSpace(*body.Name)
		if v == "" {
			writeCode(w, errcode.ValidationError, OriginDomain, "name must be non-empty after trim", nil)
			return
		}
		if err := projects.ValidateProse("name", v); err != nil {
			writeProjectsValidation(w, err)
			return
		}
		args.Name = &v
	}
	if body.Description != nil {
		if err := projects.ValidateProse("description", *body.Description); err != nil {
			writeProjectsValidation(w, err)
			return
		}
		args.Description = body.Description
	}
	ctx := r.Context()
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerOf(r))
	dto, err := a.projects.UpdateComponent(ctx, projectID, componentID, args, actor)
	if err != nil {
		writeComponentError(w, r, err, componentID, "updating a component")
		return
	}
	WriteJSON(w, r, http.StatusOK, dto)
}

// archiveComponent archives a component.
//
// @Summary Archive a component
// @Description Archives the component of the project named by the path — the same operation as the MCP tool skein_component, action archive. A component already archived is answered as it is. Restore it with updateComponent and archived false. Allowed to the project's owner, the owner of its space and the instance admin.
// @Description
// @Description Side effects: the component.archived event when the state changed.
// @Tags Projects
// @Produce json
// @Security SkeinApiKey[manage]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param component path string true "The component's UUID"
// @Success 200 {object} projects.ComponentDTO "The component, archived"
// @Failure 400 {object} ProblemDocument "Unknown parameter, a body that is not a JSON object (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks manage (code insufficient_scope); the owner may not manage the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 422 {object} ProblemDocument "The component is not in the project or the id is not a UUID (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/components/{component}/archive [post]
// @ID archiveComponent
func (a *projectsAPI) archiveComponent(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	projectID, ok := a.pathProject(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind *string `json:"kind,omitempty"`
	}
	if !DecodeBody(w, r, &body) || !checkKind(w, body.Kind) {
		return
	}
	if !a.requireOwner(w, r, projectID, "manage components") {
		return
	}
	componentID, ok := pathComponent(w, r)
	if !ok {
		return
	}
	a.setComponentArchived(w, r, projectID, componentID, true)
}

// setComponentArchived archives or restores a component, a no-op when it is
// already so, and answers its state after — the MCP tool's archive action.
func (a *projectsAPI) setComponentArchived(w http.ResponseWriter, r *http.Request, projectID, componentID uuid.UUID, archive bool) {
	ctx := r.Context()
	actor := tools.ProjectsAgentActor(ctx, a.pool, callerOf(r))
	var err error
	if archive {
		if err = a.projects.ArchiveComponent(ctx, projectID, componentID, actor); errors.Is(err, projects.ErrAlreadyArchived) {
			err = nil
		}
	} else {
		if err = a.projects.UnarchiveComponent(ctx, projectID, componentID, actor); errors.Is(err, projects.ErrNotArchived) {
			err = nil
		}
	}
	if err != nil {
		writeComponentError(w, r, err, componentID, "changing the archive state of a component")
		return
	}
	dto, err := a.projects.GetComponent(ctx, projectID, componentID)
	if err != nil {
		writeComponentError(w, r, err, componentID, "reading a component")
		return
	}
	slog.DebugContext(ctx, "public API: component archive state", "componentId", componentID.String(), "archived", archive)
	WriteJSON(w, r, http.StatusOK, dto)
}

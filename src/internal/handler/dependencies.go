package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// DependenciesHandler bundles every /issues/{id}/dependencies* route plus
// the issue-scoped POST /issues/{id}/reparent (sk-43). Constructed once at
// startup; methods are goroutine-safe (both Stores are stateless beyond
// the pool).
type DependenciesHandler struct {
	pool        *pgxpool.Pool
	depsStore   *dependencies.Store
	issuesStore *issues.Store
}

// NewDependenciesHandler returns a handler that shares the supplied
// dependencies.Store with /ready and the issues.Store with the issues
// routes. Both Stores are owned by main(); we reference them instead of
// constructing new ones so projects/issues share the connection pool
// wiring. issuesStore is used by Reparent to re-fetch the moved issue's
// IssueDetail for the response (Reparent itself can't return one — the
// dependencies package cannot import issues).
func NewDependenciesHandler(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) *DependenciesHandler {
	return &DependenciesHandler{pool: pool, depsStore: depsStore, issuesStore: issuesStore}
}

// depActor adapts the projects.Actor produced by caller() into the
// dependencies.Actor shape. We keep the types separate (instead of
// re-exporting one from another package) so neither package has to
// import the other; both packages would have a dep cycle otherwise.
func depActor(c callerCtx) dependencies.Actor {
	return dependencies.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	}
}

// mapDepValidation surfaces typed dependencies.ValidationError as 400, except
// business-rule validation errors (well-formed request the current state
// forbids) which land on 422 per the project guidelines.
//
//   - nested_standing_forbidden (sk-118 F2e) — a parent-child edge / reparent
//     that would put a standing task under a standing task.
//   - blocker_target_standing (sk-118 F2f) — a blocks/waits-for edge pointing
//     at a standing task (which would gate the blocked issue forever).
func mapDepValidation(w http.ResponseWriter, err error) bool {
	if ve, ok := dependencies.IsValidationError(err); ok {
		status := http.StatusBadRequest
		switch ve.Code {
		case "nested_standing_forbidden", "blocker_target_standing":
			status = http.StatusUnprocessableEntity
		}
		writeProblem(w, status, ve.Code, ve.Message, nil)
		return true
	}
	return false
}

// isDepTargetErr reports whether err is one of the two TO-side refusals the
// REST surface deliberately answers identically (see mapDepErr below).
//
// sk-489 (US-2): the same pair now reaches REST through the CLOSE paths too
// — closing or editing an issue with `closeTarget` creates the very same
// `duplicates` / `supersedes` edge, so it can fail the very same two ways.
// Both predicate and writer are shared rather than re-spelled beside
// mapIssueErr, because the whole point of the merge is that ONE code comes
// out of BOTH conditions: two spellings would be two chances to drift, and
// drift here re-opens the existence oracle the merge was made to close.
func isDepTargetErr(err error) bool {
	return errors.Is(err, dependencies.ErrTargetNotFound) ||
		errors.Is(err, dependencies.ErrDependencyTargetNotVisible)
}

// writeTargetNotFound emits the single anti-leak answer for both TO-side
// refusals: 422 `target_not_found`, same status, same code, same message.
func writeTargetNotFound(w http.ResponseWriter) {
	writeProblem(w, http.StatusUnprocessableEntity, "target_not_found",
		"target issue does not exist or is not accessible", nil)
}

// mapDepErr translates store errors. Returns true if a response was written.
//
// Business errors land on 422 (per the project guidelines); the two
// pre-existing "conflict" shapes (dependency already exists, version
// conflict) keep their idiomatic codes. The sk-43 re-parent typed errors
// are mapped here too so the REST reparent handler and the dep
// create/delete handlers share one translation table.
func mapDepErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, dependencies.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "dependency_not_found",
			"dependency not found", nil)
		return true
	}
	if errors.Is(err, dependencies.ErrDependencyExists) {
		writeProblem(w, http.StatusConflict, "dependency_exists",
			"this dependency already exists", nil)
		return true
	}
	// TO-side: "no such issue anywhere" and "issue exists in a project you
	// cannot see" answer IDENTICALLY — same status, same code, same message
	// (sk-194.6.51 follow-up).
	//
	// The two used to be `target_not_found` and
	// `dependency_target_not_visible`. Split like that they form an
	// existence oracle on the OTHER end of the edge: a caller who owns one
	// issue in one visible project could POST guessed toIds and learn which
	// ids are real issues in projects and teams they cannot see. That is an
	// observable response discrepancy — CWE-203, and CWE-204 for the
	// response-content flavour specifically; note that neither OWASP
	// API1:2023 nor ASVS 5.0 states a rule about response uniformity, so
	// CWE is the citation and API1:2023 is only the class of the underlying
	// object-level failure. It is also precisely the enumeration the
	// FROM-side 404 was chosen to deny, so leaving the TO side split would
	// have made the FROM-side choice decorative.
	//
	// `target_not_found` is the surviving code because it is the anti-leak
	// one: it discloses nothing, and it is the truthful answer to "can I
	// point an edge at this id" for BOTH cases. The frontend already
	// renders it ("target issue not found"), so no client contract moves.
	//
	// Scope note: this is the REST mapping only. The MCP tools do NOT come
	// through here — internal/mcp/tools/{dep_add,reparent,create}.go keep
	// their own table, which since sk-674.18 T033 (INV-3) merges the pair
	// too: both answer `issue_not_found`. A target of another space
	// (INV-24) reaches both surfaces as ErrTargetNotFound already.
	if isDepTargetErr(err) {
		writeTargetNotFound(w)
		return true
	}
	if errors.Is(err, dependencies.ErrFromNotFound) {
		writeProblem(w, http.StatusNotFound, "issue_not_found",
			"issue not found", nil)
		return true
	}
	// sk-43 — re-parent classification + parent-child guard.
	if errors.Is(err, dependencies.ErrParentAlreadySet) {
		writeProblem(w, http.StatusUnprocessableEntity, "parent_already_set",
			"issue already has a parent — use reparent to move it", nil)
		return true
	}
	if errors.Is(err, dependencies.ErrReparentSelf) {
		writeProblem(w, http.StatusUnprocessableEntity, "reparent_self",
			"an issue cannot be its own parent", nil)
		return true
	}
	if errors.Is(err, dependencies.ErrReparentCrossProject) {
		writeProblem(w, http.StatusUnprocessableEntity, "reparent_cross_project",
			"new parent must be in the same project as the issue", nil)
		return true
	}
	var linkErr *dependencies.ErrCloseOutcomeLinkRequired
	if errors.As(err, &linkErr) {
		// sk-540.8: business refusal — the edge is the last one behind a
		// close outcome. Nothing was written. A closed issue outside the
		// caller's horizon is not named (sk-423): Details and the message
		// then carry only the edge type.
		writeProblem(w, http.StatusUnprocessableEntity, "close_outcome_link_required",
			linkErr.Error(), linkErr.Details())
		return true
	}
	if errors.Is(err, dependencies.ErrSerializationRetry) {
		// Transient SERIALIZABLE loss — no durable change, safe to retry.
		writeProblem(w, http.StatusUnprocessableEntity, "serialization_retry",
			"write lost a serialization race — safe to retry", nil)
		return true
	}
	if errors.Is(err, dependencies.ErrDependencyTargetNotReady) {
		writeProblem(w, http.StatusUnprocessableEntity, "dependency_target_not_visible_yet",
			"dependency target not visible yet — retry shortly", nil)
		return true
	}
	var verr *dependencies.ErrVersionConflict
	if errors.As(err, &verr) {
		writeProblem(w, http.StatusUnprocessableEntity, "version_conflict",
			"the resource was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
		return true
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(err, &cycle) {
		writeProblem(w, http.StatusUnprocessableEntity, "dep_cycle",
			"adding this dependency would create a cycle",
			map[string]any{"path": cycle.Path})
		return true
	}
	return false
}

// ----------------- Endpoints -----------------

// createDependencyRequest is the wire shape for POST.
type createDependencyRequest struct {
	// ToID — full hierarchical id of the TO-side issue; may live in another
	// project the caller can see.
	ToID string `json:"toId"`
	// Type — one of blocks | waits-for | related | discovered-from |
	// supersedes | duplicates | parent-child; direction is canonical per type.
	Type string `json:"type"`
	// Gate — optional, accepted only when Type is waits-for: all-children |
	// any-children; any other type with a gate is 400 invalid_gate. Stored in
	// the edge's metadata.gate but NOT evaluated by readiness (sk-483): the
	// edge holds the FROM issue out of /ready until the target itself is
	// closed or standing, whatever its children's state.
	Gate *string `json:"gate,omitempty"`
}

// Create handles POST /api/v1/projects/{projectID}/issues/{issueID}/dependencies.
//
// The edge is owned by its FROM side, which is the path issue — so that is
// the side scoped to the path project: the issue in {issueID} must live in
// {projectID}, otherwise 404 issue_not_found and nothing is written
// (sk-194.6.51). The TO side is deliberately NOT scoped to the path
// project: cross-project edges are a supported feature. Its own gate is
// visibility, enforced in the store — a target whose project the caller
// cannot see comes back 422 target_not_found, byte-identical to the answer
// for a target id that does not exist at all, so the TO side is not an
// existence oracle either (see mapDepErr).
//
// Responses:
//   - 201 — the created edge.
//   - 400 — invalid project id / JSON / missing toId.
//   - 401 — unauthenticated.
//   - 404 — project not visible.
//   - 403 — forbidden: the caller may not write issues in the project
//     (US-6 ActionWrite; since sk-741 every level of a visible project has
//     it, so the answer stays for a narrower level of the action table).
//   - 404 — issue not found, or not in this project (issue_not_found).
//   - 409 — dependency_exists.
//   - 422 — target_not_found (target missing OR its project invisible —
//     one indistinguishable answer), dep_cycle, parent_already_set,
//     nested_standing_forbidden, blocker_target_standing,
//     serialization_retry.
//   - 500 — store fault.
func (h *DependenciesHandler) Create(w http.ResponseWriter, r *http.Request) {
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
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	var req createDependencyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.ToID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_to_id", "toId is required", nil)
		return
	}

	dto, err := h.depsStore.Create(ctx, issueID, dependencies.CreateDepInput{
		ToID: req.ToID,
		Type: req.Type,
		Gate: req.Gate,
	}, dependencies.CallerAccess{
		UserID: c.UserID,
		Role:   c.Role,
	}, depActor(c))
	if err != nil {
		if mapDepValidation(w, err) {
			return
		}
		if mapDepErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "create dependency failed",
			"userId", c.UserID.String(), "from", issueID, "to", req.ToID, "type", req.Type,
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "dependency created",
		"userId", c.UserID.String(), "from", dto.FromID, "to", dto.ToID, "type", dto.Type)
	writeJSON(w, http.StatusCreated, dto)
}

// Delete handles DELETE /api/v1/projects/{projectID}/issues/{issueID}/dependencies/{toID}/{type}.
//
// Same FROM-side scoping as Create: {issueID} must live in {projectID} or
// the call is 404 issue_not_found with no edge removed (sk-194.6.51). The
// TO side may be in any project — deleting a legitimate cross-project edge
// goes through its FROM side, which is exactly this route.
//
// Responses:
//   - 204 — deleted.
//   - 400 — invalid project id / missing path params.
//   - 401 — unauthenticated.
//   - 404 — project not visible.
//   - 403 — forbidden: the caller may not write issues in the project
//     (US-6 ActionWrite; since sk-741 every level of a visible project has
//     it, so the answer stays for a narrower level of the action table).
//   - 404 — issue not found / not in this project (issue_not_found), or
//     dependency_not_found.
//   - 422 — close_outcome_link_required (sk-540.8): the edge is the last
//     `duplicates` edge of an issue closed as "duplicate", or the last
//     `supersedes` edge into an issue closed as "superseded"; nothing is
//     removed — change the outcome or reopen that issue first. The body
//     names the closed issue (issueId, closeOutcome) only when the caller
//     can see it; otherwise it carries just the edge type. Also
//     serialization_retry (lost a concurrent-write race, safe to retry).
//   - 500 — store fault.
func (h *DependenciesHandler) Delete(w http.ResponseWriter, r *http.Request) {
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
	issueID := chi.URLParam(r, "issueID")
	toID := chi.URLParam(r, "toID")
	depType := chi.URLParam(r, "type")
	if issueID == "" || toID == "" || depType == "" {
		writeProblem(w, http.StatusBadRequest, "missing_param",
			"issueID, toID and type are required", nil)
		return
	}
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	if err := h.depsStore.Delete(ctx, issueID, toID, depType, depActor(c)); err != nil {
		if mapDepValidation(w, err) {
			return
		}
		if mapDepErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "delete dependency failed",
			"from", issueID, "to", toID, "type", depType, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "dependency deleted",
		"userId", c.UserID.String(), "from", issueID, "to", toID, "type", depType)
	w.WriteHeader(http.StatusNoContent)
}

// reparentRequest is the wire shape for POST .../issues/{issueID}/reparent.
type reparentRequest struct {
	// NewParentID is the full hierarchical id of the new parent. Must be
	// in the same project as the issue and must not be a descendant of it.
	NewParentID string `json:"newParentId"`
	// ExpectedVersion is the version the caller observed on the issue;
	// the move only proceeds when it matches the live version.
	ExpectedVersion int `json:"expectedVersion"`
}

// Reparent handles POST /api/v1/projects/{projectID}/issues/{issueID}/reparent.
//
// Atomically moves the issue under a new parent (sk-43). This is the
// issue-scoped re-parent endpoint — the canonical, audit-friendly way to
// change an existing issue's parent. Algorithm (all inside one
// SERIALIZABLE transaction in the store):
//
//  1. Validate the project is visible to the caller (path projectID), then
//     that the issue in {issueID} actually lives in it (sk-194.6.51) — a
//     foreign issue id is 404 issue_not_found and nothing moves. The NEW
//     PARENT is a separate question and keeps its own rule: the store
//     requires it to be in the same project as the issue
//     (reparent_cross_project), which is stricter than dependency Create
//     because the hierarchy is per-project.
//  2. Decode {newParentId, expectedVersion}; both required.
//  3. dependencies.Store.Reparent loads the issue (lock + optimistic
//     guards), validates the new parent (exists, visible, same project,
//     not a descendant), DELETEs every existing parent-child edge of the
//     issue (self-healing a double-parented child to exactly one edge),
//     INSERTs the single new parent-child edge, bumps the issue version,
//     and emits issue.reparented + per-edge dependency.deleted /
//     dependency.created events.
//
// Side effects:
//   - issue.reparented fans out a notification to the issue's watchers +
//     project watchers (sk-43); the move also surfaces in the activity
//     feed (issue.reparented is in DefaultActivityEventTypes).
//   - issues.version is incremented; updated_at is touched.
//
// Responses:
//   - 200 — the updated IssueDetail (version bumped, parentIdCurrent set).
//   - 400 — invalid project id / JSON / missing newParentId.
//   - 401 — unauthenticated.
//   - 404 — project not visible.
//   - 403 — forbidden: the caller may not write issues in the project
//     (US-6 ActionWrite; since sk-741 every level of a visible project has
//     it, so the answer stays for a narrower level of the action table).
//   - 404 — issue not found, or not in this project (issue_not_found).
//   - 422 — business errors: reparent_self, reparent_cross_project,
//     target_not_found (new parent missing OR its project invisible — one
//     indistinguishable answer), version_conflict (currentVersion
//     echoed), dep_cycle (path echoed), serialization_retry /
//     dependency_target_not_visible_yet (retryable), parent_already_set.
//     Not lock-gated — a lock held by another session does not block.
//   - 500 — store / DB fault, or a post-commit re-fetch failure.
//
// Re-parent is NOT idempotent: re-running with the same newParentId still
// performs a real DELETE+INSERT and bumps the version.
func (h *DependenciesHandler) Reparent(w http.ResponseWriter, r *http.Request) {
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
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	var req reparentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.NewParentID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_new_parent_id",
			"newParentId is required", nil)
		return
	}
	if req.ExpectedVersion < 1 {
		writeProblem(w, http.StatusBadRequest, "missing_version",
			"expectedVersion is required and must be >= 1", nil)
		return
	}

	if _, err := h.depsStore.Reparent(ctx, issueID, req.NewParentID, req.ExpectedVersion,
		dependencies.CallerAccess{UserID: c.UserID, Role: c.Role}, depActor(c)); err != nil {
		if mapDepValidation(w, err) {
			return
		}
		if mapDepErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "reparent failed",
			"userId", c.UserID.String(), "issueId", issueID,
			"newParentId", req.NewParentID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	// Re-fetch the moved issue's IssueDetail for the response — Reparent
	// itself cannot return one (the dependencies package can't import
	// issues). The version is now bumped and parentIdCurrent reflects the
	// new parent.
	dto, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		slog.ErrorContext(ctx, "reparent: re-fetch after commit failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"reparent committed but re-fetch failed", nil)
		return
	}
	slog.InfoContext(ctx, "issue reparented",
		"userId", c.UserID.String(), "issueId", issueID, "newParentId", req.NewParentID,
		"version", dto.Version)
	writeJSON(w, http.StatusOK, dto)
}

// List handles GET /api/v1/projects/{projectID}/issues/{issueID}/dependencies.
//
// Returns {outgoing, incoming} arrays. The OTHER side of each edge is
// embedded as IssueRef when the caller can see its project; otherwise
// the row is marked redacted=true (link still listed so the user sees
// the topology, just no payload from the foreign project) and its other
// end is a synthetic "hidden-N" token instead of the real id (sk-674.18
// T009): numbered in response order, one token per hidden issue. The
// token names nothing — an operation addressed by it answers as for a
// missing issue.
//
// @Summary List an issue's dependency edges
// @Description Business meaning: the "Dependencies" section of the issue card — every edge of the issue in both directions, with the issue on the other end. Algorithm: check the project is visible and the issue lives in it -> one query reads the outgoing and incoming edges with the other end's summary and, per edge, whether the caller sees the other end's project (the access relation) -> edges to a hidden issue are redacted -> sort.
// @Description
// @Description Redaction (sk-674.18 T009): an edge whose other end the caller cannot see stays listed with redacted=true, no `issue` key, and the other end's id replaced by "hidden-1", "hidden-2", … in response order (the same hidden issue linked twice keeps one token). The real id of an inaccessible issue is never on the wire; DELETE …/dependencies/hidden-N answers as for a missing issue.
// @Description
// @Description Order (sk-426): within each array by edge type, then by the other end's id under the id-order rule (`x.2` before `x.10`). Side effects: none.
// @Tags Dependencies
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param issueID path string true "Hierarchical issue id"
// @Success 200 {object} dependencies.DependencyList "Outgoing and incoming edges"
// @Failure 400 {object} map[string]any "invalid project id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 404 {object} map[string]any "project not visible, issue not found or not in this project"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/{issueID}/dependencies [get]
//
// Order (sk-426): within each array, by edge type, then by the other side's
// id under the id-order rule of sk-421.2 US-6 — digit segments compare by
// numeric value, so `x.2` precedes `x.10` (string order before sk-426).
//
// Path scoping: {issueID} must live in {projectID}, else 404
// issue_not_found (sk-194.6.51). Without it the per-edge redaction above
// was protecting only the FAR side — the near side's entire edge list was
// readable for any issue id, through any project the caller could see.
//
// Responses: 200 {outgoing, incoming} · 400 invalid project id ·
// 401 unauthenticated · 404 project not visible · 403 forbidden — the caller
// may not write issues in the project (US-6 ActionWrite) · 404 issue not found /
// not in this project · 500 store fault.
func (h *DependenciesHandler) List(w http.ResponseWriter, r *http.Request) {
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
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization: EnsureProjectVisible cleared the project in
	// the path, not the issue in the path. Without this, a foreign issue's
	// whole dependency topology is readable through any project the caller
	// can see. Rationale and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	out, err := h.depsStore.List(ctx, issueID, authz.FromContext(ctx))
	if err != nil {
		slog.ErrorContext(ctx, "list dependencies failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

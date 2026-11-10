package publicapi

// dependencies.go — the dependency writes of the public API (spec
// sk-878.3.1, "API surface", T026): thin adapters over the operations of
// internal/issueops that the MCP tools skein_dep_add and skein_dep_remove
// also call (INV-4). The issue of the path is the FROM end of the edge.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/issueops"
)

// The operationIds of the dependency writes.
const (
	OperationAddDependency    = "addDependency"
	OperationRemoveDependency = "removeDependency"
)

// addDependencyBody is the body of addDependency.
type addDependencyBody struct {
	// ToID is the hierarchical id of the issue the issue of the path
	// depends on. Required.
	ToID *string `json:"toId,omitempty"`
	// Type: blocks, waits-for, related, discovered-from, supersedes,
	// duplicates or parent-child. Required.
	Type *string `json:"type,omitempty"`
	// Gate qualifies a waits-for edge.
	Gate *string `json:"gate,omitempty"`
	// Kind of the write: directed or autonomous; omitted — the origin is
	// api.
	Kind *string `json:"kind,omitempty"`
}

// DependencyRemoved is the answer of removeDependency.
type DependencyRemoved struct {
	// Removed is always true.
	Removed bool `json:"removed"`
	// FromID, ToID and Type name the edge that was removed.
	FromID string `json:"fromId"`
	ToID   string `json:"toId"`
	Type   string `json:"type"`
}

// addDependency adds an edge from the issue of the path.
//
// @Summary Add a dependency
// @Description Adds one edge from the issue of the path to toId — the same operation as the MCP tool skein_dep_add: the owner of the key must be able to write in the project of the issue of the path; the edge is not guarded by the lock. A waits-for edge may carry a gate. The graph stays acyclic (blocks, waits-for, parent-child), a standing task is neither a nested standing child nor a blocker target, and an existing edge is refused.
// @Description
// @Description The issue of the path that does not exist or is out of reach is 404 issue_not_found; toId that does not exist or is out of reach is the same code with 422 (a reference of the body). kind is optional and checked; a write by key is an agent's write. Side effects: dependency.created; notifications of the watchers.
// @Tags Dependencies
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The FROM issue's hierarchical id"
// @Param body body addDependencyBody true "The edge"
// @Success 201 {object} issueops.DepDTO "The edge added"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue of the path within reach (code issue_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "toId does not exist or is out of reach (code issue_not_found, as MCP); an invalid type or gate (code validation_error); a cycle (dependency_cycle); the edge exists (dependency_already_exists); a second parent (parent_already_set); a nested standing task (nested_standing_forbidden) or a standing blocker target (blocker_target_standing); the target is not visible yet (dependency_target_not_visible_yet, retryable); a serialization race (serialization_retry, retryable)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/dependencies [post]
// @ID addDependency
func (a *issueWrites) addDependency(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body addDependencyBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["fromId"] = id
	dep, f := issueops.DepAdd(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusCreated, dep)
}

// removeDependency removes an edge from the issue of the path.
//
// @Summary Remove a dependency
// @Description Removes the edge of the given type from the issue of the path to toIssue — the same operation as the MCP tool skein_dep_remove: the owner of the key must be able to write in the project of the issue of the path; the edge is not guarded by the lock. No such edge is 404 dependency_not_found: the edge is the object of the path. An edge the close outcome of a closed issue needs is not removed (close_outcome_link_required).
// @Description
// @Description Side effects: dependency.deleted; notifications of the watchers.
// @Tags Dependencies
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The FROM issue's hierarchical id"
// @Param toIssue path string true "The TO issue's hierarchical id"
// @Param type path string true "The edge type"
// @Param body body kindBody false "Options"
// @Success 200 {object} DependencyRemoved "The edge removed"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue of the path within reach (code issue_not_found); no such edge (code dependency_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "An invalid type or TO id (code validation_error); the close outcome needs the edge (close_outcome_link_required); a serialization race (serialization_retry, retryable)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/dependencies/{toIssue}/{type} [delete]
// @ID removeDependency
func (a *issueWrites) removeDependency(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body kindBody
	if _, ok := decodeArgs(w, r, &body); !ok || !kindOf(w, body.Kind) {
		return
	}
	args := issueops.Args{"fromId": id, "toId": chi.URLParam(r, "toIssue"), "type": chi.URLParam(r, "type")}
	res, f := issueops.DepRemove(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, DependencyRemoved{Removed: true, FromID: res.FromID, ToID: res.ToID, Type: res.Type})
}

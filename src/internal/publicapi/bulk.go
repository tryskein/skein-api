package publicapi

// bulk.go — the batches of the public API (spec sk-878.3.1, "Batches"):
// bulkUpdateIssues, bulkCloseIssues and bulkAddDependencies, adapters over
// the batch operations of internal/issueops that skein_update_many,
// skein_close_many and skein_dep_add_many also call. Each row is atomic on
// its own and the batch answers 207 with a result or a refusal code per
// row; a refusal of the whole request (the key right, the format, the
// limit of 1..100 items) is answered with its own status and runs no row.
// Batches take neither If-Match (the expected version is in each row) nor
// Idempotency-Key.

import (
	"errors"
	"net/http"

	"skein-api/internal/issueops"
)

// The operationIds of the batches.
const (
	OperationBulkUpdateIssues    = "bulkUpdateIssues"
	OperationBulkCloseIssues     = "bulkCloseIssues"
	OperationBulkAddDependencies = "bulkAddDependencies"
)

// bulkUpdateItem is one row of bulkUpdateIssues: the issue, its expected
// version and the changes of updateIssue (without reviewRequired and
// closeTarget, as skein_update_many).
type bulkUpdateItem struct {
	// ID of the issue. Required.
	ID string `json:"id"`
	// ExpectedVersion of the issue. Required.
	ExpectedVersion int `json:"expectedVersion"`
	// Title, 1..500 characters.
	Title *string `json:"title,omitempty"`
	// Description; null or "" clears it.
	Description *string `json:"description,omitempty"`
	// Type of the issue.
	Type *string `json:"type,omitempty"`
	// Priority 0..4.
	Priority *int `json:"priority,omitempty"`
	// Design; null or "" clears it.
	Design *string `json:"design,omitempty"`
	// AcceptanceCriteria; null or "" clears it.
	AcceptanceCriteria *string `json:"acceptanceCriteria,omitempty"`
	// Notes; null or "" clears it.
	Notes *string `json:"notes,omitempty"`
	// SourceContext; null or "" clears it.
	SourceContext *string `json:"sourceContext,omitempty"`
	// Assignee; "" clears it.
	Assignee *string `json:"assignee,omitempty"`
	// DueAt, RFC 3339.
	DueAt *string `json:"dueAt,omitempty"`
	// DeferUntil, RFC 3339.
	DeferUntil *string `json:"deferUntil,omitempty"`
	// ExternalRef; null or "" clears it.
	ExternalRef *string `json:"externalRef,omitempty"`
	// EstimatedMinutes, >= 0.
	EstimatedMinutes *int `json:"estimatedMinutes,omitempty"`
	// Status: open, in_progress, deferred, closed or standing.
	Status *string `json:"status,omitempty"`
	// Reason of a status change.
	Reason *string `json:"reason,omitempty"`
	// CloseOutcome with status closed.
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// ComponentIDs replace the components.
	ComponentIDs []string `json:"componentIds,omitempty"`
	// LabelsAdd are labels to add.
	LabelsAdd []string `json:"labelsAdd,omitempty"`
	// LabelsRemove are labels to remove.
	LabelsRemove []string `json:"labelsRemove,omitempty"`
}

// bulkUpdateBody is the body of bulkUpdateIssues.
type bulkUpdateBody struct {
	// Items are 1..100 rows.
	Items []bulkUpdateItem `json:"items"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// bulkCloseItem is one row of bulkCloseIssues.
type bulkCloseItem struct {
	// ID of the issue. Required.
	ID string `json:"id"`
	// ExpectedVersion of the issue. Required.
	ExpectedVersion int `json:"expectedVersion"`
	// Reason of the close, non-empty. Required.
	Reason string `json:"reason"`
	// CloseOutcome: done, duplicate, superseded or wont_do.
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseTarget of a duplicate or superseded close.
	CloseTarget *string `json:"closeTarget,omitempty"`
	// Commits of the close.
	Commits []commitInput `json:"commits,omitempty"`
}

// bulkCloseBody is the body of bulkCloseIssues.
type bulkCloseBody struct {
	// Items are 1..100 rows.
	Items []bulkCloseItem `json:"items"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// bulkDependencyItem is one row of bulkAddDependencies.
type bulkDependencyItem struct {
	// FromID is the issue the edge goes from. Required.
	FromID string `json:"fromId"`
	// ToID is the issue the edge goes to. Required.
	ToID string `json:"toId"`
	// Type of the edge. Required.
	Type string `json:"type"`
	// Gate qualifies a waits-for edge.
	Gate *string `json:"gate,omitempty"`
}

// bulkDependenciesBody is the body of bulkAddDependencies.
type bulkDependenciesBody struct {
	// Items are 1..100 rows.
	Items []bulkDependencyItem `json:"items"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// bulk decodes a batch body, checks its kind and its 1..100 items at the
// parsing layer, and returns the arguments of the batch operation.
func bulk(w http.ResponseWriter, r *http.Request, typed any, kind **string, items func() int) (issueops.Args, bool) {
	if !CheckQuery(w, r) {
		return nil, false
	}
	args, ok := decodeArgs(w, r, typed)
	if !ok || !kindOf(w, *kind) {
		return nil, false
	}
	delete(args, "kind")
	if n := items(); n < 1 || n > issueops.BatchMaxItems {
		WriteParseError(w, errors.New("items must contain 1..100 rows"))
		return nil, false
	}
	return args, true
}

// bulkUpdateIssues changes several issues, each row on its own.
//
// @Summary Update several issues
// @Description Applies each row — an issue, its expected version and the changes of updateIssue — in a transaction of its own, the same batch as the MCP tool skein_update_many: a row's refusal (version_conflict with the current version, locked_by_other, issue_not_found, …) does not stop the others; the answer is 207 with each row's result (id, ok, version) or error (code, message, details) and a summary. A refusal of the whole request — the key right, the format, 0 or more than 100 rows — is answered with its own status and no row runs. reviewRequired and closeTarget are not accepted in a row. The batch stops at its server deadline: the rows not run answer batch_deadline_exceeded.
// @Description
// @Description force=true (the key needs manage) bypasses another holder's claim, the version and the pin in every row; a row of a project where the owner lacks force_unlock is refused force_unlock_forbidden, the others run. A repeat of a batch with force=true applies its changes again: read the issues before repeating. No Idempotency-Key.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Param body body bulkUpdateBody true "The rows"
// @Success 207 {object} issueops.BatchResponse "The result of each row"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, 0 or more than 100 rows, force not true|false (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/bulk-update [post]
// @ID bulkUpdateIssues
func (a *issueWrites) bulkUpdateIssues(w http.ResponseWriter, r *http.Request) {
	var body bulkUpdateBody
	args, ok := bulk(w, r, &body, &body.Kind, func() int { return len(body.Items) })
	if !ok {
		return
	}
	forceArg(r, args)
	res, f := issueops.UpdateMany(r.Context(), a.ops, callerOf(r), args, issueops.DefaultBatchLoop)
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusMultiStatus, res)
}

// bulkCloseIssues closes several issues, each row on its own.
//
// @Summary Close several issues
// @Description Closes each row — an issue, its expected version, the reason and optionally the outcome, the target and the commits — in a transaction of its own, the same batch as the MCP tool skein_close_many: a row's refusal does not stop the others; the answer is 207 with each row's result (id, ok, version, closeOutcome, closeLink) or error and a summary. A refusal of the whole request — the key right, the format, 0 or more than 100 rows — is answered with its own status and no row runs.
// @Description
// @Description force=true (the key needs manage) bypasses another holder's claim, the version and the pin in every row; a row of a project where the owner lacks force_unlock is refused force_unlock_forbidden. A repeat of a batch with force=true applies again: read the issues before repeating. No Idempotency-Key.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Param body body bulkCloseBody true "The rows"
// @Success 207 {object} issueops.CloseManyResponse "The result of each row"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, 0 or more than 100 rows, force not true|false (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/bulk-close [post]
// @ID bulkCloseIssues
func (a *issueWrites) bulkCloseIssues(w http.ResponseWriter, r *http.Request) {
	var body bulkCloseBody
	args, ok := bulk(w, r, &body, &body.Kind, func() int { return len(body.Items) })
	if !ok {
		return
	}
	forceArg(r, args)
	res, f := issueops.CloseMany(r.Context(), a.ops, callerOf(r), args, issueops.DefaultBatchLoop)
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusMultiStatus, res)
}

// bulkAddDependencies adds several edges, each row on its own.
//
// @Summary Add several dependencies
// @Description Adds each edge — from, to, type and optionally a gate — on its own, the same batch as the MCP tool skein_dep_add_many: a row's refusal (an end missing or outside the key's scope — issue_not_found as MCP, dependency_already_exists, dependency_cycle, …) does not stop the others; the answer is 207 with each row's result (the edge) or error and a summary. Edges have no version. A refusal of the whole request — the key right, the format, 0 or more than 100 rows — is answered with its own status and no row runs. A repeat is safe: an existing edge answers dependency_already_exists. No Idempotency-Key.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param body body bulkDependenciesBody true "The rows"
// @Success 207 {object} issueops.DepAddManyResponse "The result of each row"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, 0 or more than 100 rows (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/dependencies/bulk-add [post]
// @ID bulkAddDependencies
func (a *issueWrites) bulkAddDependencies(w http.ResponseWriter, r *http.Request) {
	var body bulkDependenciesBody
	args, ok := bulk(w, r, &body, &body.Kind, func() int { return len(body.Items) })
	if !ok {
		return
	}
	res, f := issueops.DepAddMany(r.Context(), a.ops, callerOf(r), args, issueops.DefaultBatchLoop)
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusMultiStatus, res)
}

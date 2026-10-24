package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// BulkHandler implements POST /projects/{pid}/issues/bulk — close/reopen/
// pin/unpin/archive across many issues in one request. Per-id transactions
// (loop) so a single bad row doesn't fail the whole batch; the response
// is an HTTP 207 with both successes and failures, mirroring how the
// real-world UI presents bulk results.
type BulkHandler struct {
	pool        *pgxpool.Pool
	issuesStore *issues.Store
}

// NewBulkHandler builds the bulk handler. Reuses the issues.Store so we
// don't need to re-derive ID-mode / project metadata for each id (we
// don't actually need that for bulk — every action below works on an
// existing row by id).
func NewBulkHandler(pool *pgxpool.Pool, issuesStore *issues.Store) *BulkHandler {
	return &BulkHandler{pool: pool, issuesStore: issuesStore}
}

// Allowed bulk actions. We deliberately do NOT include "delete" — the
// vision rejects hard delete in normal flow (vision §10).
const (
	BulkActionClose   = "close"
	BulkActionReopen  = "reopen"
	BulkActionPin     = "pin"
	BulkActionUnpin   = "unpin"
	BulkActionArchive = "archive"
)

// bulkMaxIDs caps the per-request batch. Anything bigger should be a
// background job, not a synchronous round-trip from the BFF.
const bulkMaxIDs = 100

type bulkRequest struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"`
	Reason *string  `json:"reason,omitempty"`
	// Force bypasses the claim, version and pinned guards for every row;
	// it needs force_unlock on the project (sk-674.18 T034, K-28).
	Force bool `json:"force,omitempty"`

	// CloseOutcome - ONE close outcome for the whole batch (sk-489, US-11): one
	// of four values, accepted only for the close and archive actions.
	// Not per item: a batch is one intent applied to a list, and
	// "close these three as duplicates of that one" is expressible in it, while "each with its own
	// outcome" is no longer a batch.
	//
	// nil means "the caller said nothing about the outcome": for unclosed items
	// `close` gives the transition default (done), for already closed ones the outcome
	// is kept (INV-10). `archive` without an outcome is refused as a
	// whole when the batch holds an unclosed non-standing issue (sk-540.4,
	// checkArchiveOutcome); with one, the outcome and target apply to the
	// unclosed rows only (UpdateStatusInput.Archive).
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseTarget - ONE target of the "duplicate" / "superseded" link for the whole batch
	// (US-2): the issue that all items duplicate or are superseded by.
	// Allowed only together with an explicit CloseOutcome duplicate / superseded.
	// The target may live in ANOTHER visible project - the visibility check goes
	// by the caller's horizon, not by the batch's project.
	CloseTarget string `json:"closeTarget,omitempty"`
}

type bulkFailureItem struct {
	ID      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type bulkResponse struct {
	Succeeded []string          `json:"succeeded"`
	Failed    []bulkFailureItem `json:"failed"`
}

// Bulk handles POST /api/v1/projects/{projectID}/issues/bulk.
//
// Body: { ids: [...], action: 'close' | 'reopen' | 'pin' | 'unpin' | 'archive',
//
//	reason?: string, force?: bool }
//
// Status codes:
//   - 207 Multi-Status — mix of succeeded and failed (the typical case)
//   - 200 OK             — every id succeeded
//   - 422 Unprocessable  — every id failed
//   - 400 Bad Request    — invalid input (bad json, ids empty, ids > limit,
//     unsupported action)
//
// Object-level authorization: the route-level RequireUser +
// EnsureProjectAction(ActionWrite) only authorize the PROJECT in the path. The ids come
// from the request body, so each one is separately gated in runOne by
// issues.Store.IssueBelongsToProject BEFORE anything is read or written for
// it — an id belonging to another project fails exactly as an id that does
// not exist (`failed[].code == "not_found"`), so a batch cannot be used to
// enumerate issues elsewhere in the installation (sk-194.6.50).
//
// Per-id ops use the existing single-issue methods, so optimistic
// concurrency, pin-protection on close, and event/fan-out emission all
// behave exactly as on the regular endpoints.
//
// @Summary Apply one action to many issues
// @Description Applies a single action — close, reopen, pin, unpin, archive — to a list of issue ids in one request. Business meaning: the multi-select on a board, where a person sweeps a batch of tasks in one gesture and expects to be told, per id, what actually happened rather than being handed one all-or-nothing verdict.
// @Description
// @Description Algorithm: authenticate -> validate the path project id -> verify project visibility -> decode the body -> refuse an empty id list or one over 100 -> refuse an unknown action (delete is rejected by name: the product has no hard delete) -> validate the batch-wide close outcome and target, refusing the WHOLE request before anything is written -> for archive without an outcome, read the statuses of the batch rows in this project and refuse the WHOLE request if any is unclosed and not standing -> then, per id in order: gate the id against the path project, read its current version, and run the single-issue writer. A cancelled client connection stops the loop where it is.
// @Description
// @Description Two layers of refusal, and the difference is the contract: a bad REQUEST (unknown action, an outcome on a non-closing action, an unknown outcome value, a target that does not exist or is invisible, an archive without an outcome over an unclosed row) refuses everything and applies nothing; a bad ROW (not in this project, stale version, pinned without force, a row that IS the close target, duplicate/superseded without a target on a row the exceptions do not cover) fails only that row and the rest still run.
// @Description
// @Description Close outcome (sk-489): `closeOutcome` and `closeTarget` are ONE outcome and ONE target for the whole batch, accepted only on close and archive. `close` without an outcome gives unclosed rows `done`; an already closed row keeps the outcome it had, and an explicit outcome re-closes it with the new value. duplicate and superseded without a target (sk-540.2) are judged PER ROW, because the exceptions depend on the row: an unclosed row fails with close_target_required, an already closed row passes when it keeps that outcome or already has the matching edge (`duplicates` from it, `supersedes` to it) and fails with close_target_required otherwise.
// @Description
// @Description Archive (sk-540.4, a deliberate contract change): archiving no longer closes an unclosed issue as `wont_do` behind the caller's back. `archive` WITHOUT `closeOutcome` on a batch holding at least one issue of this project that is neither closed nor standing is refused as a WHOLE — 422 close_outcome_required, `ids` lists those issues, nothing is written. A batch of only closed and/or standing issues archives as before: closed rows keep their outcome, standing rows get `done`. WITH `closeOutcome` the outcome (and `closeTarget`) apply to the unclosed rows only; already closed rows keep their outcome and standing rows get `done`, and no edge is created for either — this replaces the earlier «an explicit outcome re-closes the already-closed ones». So an archive of only closed/standing rows with duplicate/superseded and no target passes. A row reopened after that check fails alone with close_outcome_required instead of being archived without an outcome.
// @Description
// @Description Side effects: per applied row, everything the single-issue path emits — issue.status_changed, issue.closed and lock release on a close, the close-target dependency edge when one was named, issue.outcome_changed when a re-close changed the outcome, watcher fan-out and notifications.
// @Tags Issues
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param body body bulkRequest true "Ids, the action, and — on close/archive — one outcome and one target for the batch"
// @Success 200 {object} bulkResponse "Every id succeeded"
// @Success 207 {object} bulkResponse "Partial success — succeeded[] and failed[] both non-empty; each failure carries a stable code"
// @Failure 400 {object} map[string]any "invalid_id; invalid_json; ids_required; bulk_too_large; unsupported_action; close_outcome_not_a_close — an outcome or target on reopen/pin/unpin; invalid_close_outcome; close_target_requires_outcome; close_target_not_allowed"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "not_found — the project is absent or invisible"
// @Failure 422 {object} map[string]any "force_unlock_forbidden — force=true without the right to release another holder's claim (project owner, space owner, instance admin), NOTHING was applied; every id failed (the body is the same bulkResponse, with failed[] full); close_outcome_required — archive without closeOutcome while the batch holds an unclosed non-standing issue (`ids` lists them), NOTHING was applied; target_not_found — closeTarget does not exist OR is in a project this caller cannot see, answered identically on purpose, and NOTHING was applied"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/bulk [post]
func (h *BulkHandler) Bulk(w http.ResponseWriter, r *http.Request) {
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
	level, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	var req bulkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	// sk-674.18 T034 (K-28): every id of the batch is of the path project,
	// so its force_unlock answers for the whole request; nothing written.
	if req.Force && !requireForceUnlock(w, level) {
		return
	}

	if len(req.IDs) == 0 {
		writeProblem(w, http.StatusBadRequest, "ids_required",
			"ids must be a non-empty array", nil)
		return
	}
	if len(req.IDs) > bulkMaxIDs {
		writeProblem(w, http.StatusBadRequest, "bulk_too_large",
			"at most 100 ids per bulk request", nil)
		return
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case BulkActionClose, BulkActionReopen, BulkActionPin, BulkActionUnpin, BulkActionArchive:
		// ok
	case "delete":
		writeProblem(w, http.StatusBadRequest, "unsupported_action",
			"delete is not allowed in bulk", nil)
		return
	default:
		writeProblem(w, http.StatusBadRequest, "unsupported_action",
			"unknown action: "+req.Action, nil)
		return
	}

	if !h.checkBulkCloseOutcome(ctx, w, c, action, pid, req) {
		return
	}
	if !h.checkArchiveOutcome(ctx, w, pid, action, req) {
		return
	}

	resp := bulkResponse{
		Succeeded: make([]string, 0, len(req.IDs)),
		Failed:    make([]bulkFailureItem, 0),
	}

	// Sequentially execute per id. The cost (one round-trip per item) is
	// acceptable at the 100-id ceiling, and serial execution avoids
	// surprising lock contention on the same row in pin/unpin races.
	for _, rawID := range req.IDs {
		// Bail out early if the client cancelled (browser closed,
		// nav-away). Without this we keep hitting pgx with a dead ctx
		// for the rest of the batch.
		if err := ctx.Err(); err != nil {
			break
		}
		id := strings.TrimSpace(rawID)
		if id == "" {
			resp.Failed = append(resp.Failed, bulkFailureItem{
				ID:   rawID,
				Code: "invalid_id",
			})
			continue
		}
		if err := h.runOne(ctx, c, pid, id, action, req); err != nil {
			resp.Failed = append(resp.Failed, mapBulkError(id, err))
			continue
		}
		resp.Succeeded = append(resp.Succeeded, id)
	}

	switch {
	case len(resp.Failed) == 0:
		writeJSON(w, http.StatusOK, resp)
	case len(resp.Succeeded) == 0:
		writeJSON(w, http.StatusUnprocessableEntity, resp)
	default:
		writeJSON(w, http.StatusMultiStatus, resp)
	}

	slog.InfoContext(ctx, "bulk action processed",
		"userId", c.UserID.String(),
		"projectId", pid.String(),
		"action", action,
		"requested", len(req.IDs),
		"succeeded", len(resp.Succeeded),
		"failed", len(resp.Failed))
}

// checkBulkCloseOutcome validates the batch-wide outcome and target BEFORE
// a single row is touched (sk-489, US-11). Returns false when a response has
// already been written and Bulk must return.
//
// # Why before the loop, and not inside runOne
//
// These refusals are refusals of the REQUEST, not of a row. "Close these
// forty as duplicates of an id that does not exist" cannot be half-right:
// there is no subset of the forty for which it means something. Running the
// check inside runOne would instead fail every row separately, which reads
// as forty independent accidents and — worse for `archive` — would already
// have closed nothing while reporting forty different failures. So the
// whole batch is refused, nothing is applied, and the status code says which
// kind of wrongness it was: 400 for a request that cannot be right in any
// state, 422 for one the current state forbids.
//
// ⚠ This is the OPPOSITE contract to the MCP batch (skein_close_many),
// where the same target refusal is a refusal of the POSITION and the other
// rows still close. That is deliberate, not an inconsistency: the MCP batch
// carries a per-item outcome and target, so a bad item is genuinely one
// item's problem, whereas here the outcome and target are properties of the
// single request.
//
// # What is NOT checked here
//
// The self-target case — a position whose id equals closeTarget — is a
// property of the ROW, not of the request: the other rows are perfectly
// closable against that target. It is therefore left to the writer, where
// issues.DecideCloseOutcome raises close_target_self and mapBulkError turns
// it into that one row's failure. That is why the IssueID passed below is
// empty: this call asks the shared decider only the questions that are
// answerable about the request alone.
func (h *BulkHandler) checkBulkCloseOutcome(
	ctx context.Context,
	w http.ResponseWriter,
	c callerCtx,
	action string,
	pid uuid.UUID,
	req bulkRequest,
) bool {
	target := strings.TrimSpace(req.CloseTarget)
	hasOutcomeOrTarget := req.CloseOutcome != nil || target != ""

	switch action {
	case BulkActionClose, BulkActionArchive:
		// These close; an outcome and a target are meaningful.
	default:
		// reopen / pin / unpin never close anything, so an outcome or a
		// target here is a request that cannot be right under any state of
		// any issue — a malformed combination of fields, hence 400. Same
		// code the single-issue transition uses for the same mistake
		// (issues.Store.UpdateStatus with a non-closed status), so a client
		// branches on one code for one error.
		if hasOutcomeOrTarget {
			writeProblem(w, http.StatusBadRequest, "close_outcome_not_a_close",
				"closeOutcome and closeTarget are only accepted on actions that close "+
					"(close, archive)", nil)
			return false
		}
		return true
	}

	// One decider for every surface and both writers (US-2): the value must
	// be one of the four, a target needs an EXPLICIT duplicate/superseded
	// outcome, and nothing else may carry a target. The decision itself is
	// discarded on purpose — here it would be the decision for no particular
	// issue, and each row re-derives its own inside the writer, where the
	// row's previous outcome and closed-ness are known.
	//
	// sk-540.2: RowStateUnknown — "duplicate/superseded needs a target" has
	// two exceptions that depend on the ROW (already closed with the same
	// outcome; already closed with the matching edge), so it is not a
	// property of the request. The writer (issues.Store.UpdateStatus)
	// enforces it per row, and a refused row is that row's failure
	// (close_target_required) while the rest of the batch proceeds.
	if _, err := issues.DecideCloseOutcome(issues.CloseOutcomeRequest{
		Requested:       req.CloseOutcome,
		Target:          target,
		RowStateUnknown: true,
	}); err != nil {
		if mapIssueValidation(w, err) {
			return false
		}
		slog.ErrorContext(ctx, "bulk close outcome check failed", "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return false
	}

	if target == "" {
		return true
	}

	// Existence, space (INV-24: the batch's project binds the space — every
	// row is of that project) and visibility of the target, once for the
	// batch, on the pool.
	//
	// ⚠ NOT IssueBelongsToProject: that predicate is scoped to the batch's
	// project and would refuse a target in ANOTHER project the caller can
	// see — which US-2 explicitly allows (a duplicate may well live in a
	// different project). The scoped predicate stays where it belongs, on
	// each row's own id in runOne, because THAT is the id the path project
	// authorises.
	if _, err := issues.EnsureCloseTargetVisible(ctx, h.pool, target, pid,
		issues.DependenciesCaller{UserID: c.UserID, Role: c.Role}); err != nil {
		if isDepTargetErr(err) {
			writeTargetNotFound(w)
			return false
		}
		slog.ErrorContext(ctx, "bulk close target check failed",
			"closeTarget", target, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return false
	}
	return true
}

// checkArchiveOutcome refuses an `archive` without closeOutcome as a WHOLE
// request (422 close_outcome_required, nothing written) when the batch holds
// at least one issue that is neither closed nor standing (sk-540.4). Returns
// false when a response has already been written.
//
// Archiving used to close such an issue silently as wont_do, turning «get it
// out of sight» into «we decided not to do it» and shrinking its epic's
// progress denominator without anyone saying so. The batch carries ONE
// outcome, so a missing one is a property of the request — hence the refusal
// before the loop rather than per row. A batch of only closed and/or
// standing issues still archives without an outcome, as before.
//
// Only rows of the path project are read (issues.Store.UnclosedWorkIDsInProject):
// a foreign or unknown id neither triggers nor escapes the refusal and still
// fails later in runOne as not_found, so the refusal reveals nothing about
// issues outside the authorised project. The listed `ids` are all ids the
// caller sent and may see.
//
// A row reopened between this read and its write is caught by the writer:
// UpdateStatus with Archive refuses an unclosed row without an outcome inside
// its transaction, so the race costs that row, never a silent wont_do.
func (h *BulkHandler) checkArchiveOutcome(
	ctx context.Context,
	w http.ResponseWriter,
	pid uuid.UUID,
	action string,
	req bulkRequest,
) bool {
	if action != BulkActionArchive || req.CloseOutcome != nil {
		return true
	}
	ids := make([]string, 0, len(req.IDs))
	for _, raw := range req.IDs {
		if id := strings.TrimSpace(raw); id != "" {
			ids = append(ids, id)
		}
	}
	unclosed, err := h.issuesStore.UnclosedWorkIDsInProject(ctx, pid, ids)
	if err != nil {
		slog.ErrorContext(ctx, "bulk archive outcome check failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return false
	}
	if len(unclosed) == 0 {
		return true
	}
	ve, ok := issues.IsValidationError(issues.ArchiveOutcomeRequiredError())
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return false
	}
	writeProblem(w, http.StatusUnprocessableEntity, ve.Code, ve.Message,
		map[string]any{"ids": unclosed})
	return false
}

// runOne dispatches one bulk action against one id. It gates the id against
// the path project FIRST (see below), then reads the current version, so
// optimistic concurrency is handled here and the caller doesn't have to
// provide If-Match for every id.
//
// `archive` is implemented as "close with reason='archived'" because the
// issues schema currently uses status='closed' as the archive sink (no
// separate archived_at column exists yet — see db-schema.md §0
// "archived_at handling" and the existing UpdateStatus tests).
func (h *BulkHandler) runOne(
	ctx context.Context,
	c callerCtx,
	pid uuid.UUID,
	id string,
	action string,
	req bulkRequest,
) error {
	reason, force := req.Reason, req.Force
	// Object-level authorization FIRST, on a scoped predicate
	// (sk-194.6.50). Bulk takes its ids from the request BODY, so the only
	// thing tying an id to the caller's authorized scope is this check —
	// there is no path segment the router could have validated. Running it
	// before GetIssue also means a foreign id never hydrates a full
	// IssueDetail: "id belongs to another project" and "no such id" leave
	// through one query plan and one error, so a batch of guessed ids
	// cannot be used to sort real issues from imaginary ones (CWE-203/204;
	// the query-the-authorized-dataset shape is OWASP's IDOR Prevention
	// Cheat Sheet, "Mitigation").
	if err := h.issuesStore.IssueBelongsToProject(ctx, id, pid); err != nil {
		return err
	}
	dto, err := h.issuesStore.GetIssue(ctx, id)
	if err != nil {
		return err
	}

	switch action {
	case BulkActionClose, BulkActionArchive:
		statusReason := reason
		if action == BulkActionArchive {
			r := "archived"
			if reason != nil {
				r = *reason
			}
			statusReason = &r
		}
		_, err := h.issuesStore.UpdateStatus(ctx, id, issues.UpdateStatusInput{
			ExpectedVersion: dto.Version,
			Status:          issues.StatusClosed,
			Reason:          statusReason,
			Force:           force,
			CloseOutcome:    req.CloseOutcome,
			CloseTarget:     req.CloseTarget,
			// sk-540.4: the flag says this is an archive; the writer decides
			// per row what follows — an unclosed row needs the outcome (the
			// in-transaction guard against a reopen after the preflight), a
			// closed or standing row ignores the outcome and target.
			Archive:            action == BulkActionArchive,
			DependenciesCaller: closeTargetCaller(c),
		}, issueActor(c))
		return err

	case BulkActionReopen:
		_, err := h.issuesStore.UpdateStatus(ctx, id, issues.UpdateStatusInput{
			ExpectedVersion: dto.Version,
			Status:          issues.StatusOpen,
			Reason:          reason,
			Force:           force,
		}, issueActor(c))
		return err

	case BulkActionPin:
		_, err := h.issuesStore.Pin(ctx, id, dto.Version, issueActor(c))
		return err

	case BulkActionUnpin:
		_, err := h.issuesStore.Unpin(ctx, id, dto.Version, issueActor(c))
		return err
	}
	// Unreachable: caller validates action against the closed set above.
	// Panic to fail loud during development if a new action is added
	// without a case here.
	panic("unhandled bulk action: " + action)
}

// mapBulkError converts an internal store error into a stable
// machine-readable code for the bulk response. Unknown errors collapse
// to "internal_error" with a generic message — the underlying error
// (which may include schema details from pgx) is logged server-side
// rather than echoed back to the client.
func mapBulkError(id string, err error) bulkFailureItem {
	switch {
	case errors.Is(err, issues.ErrNotFound):
		return bulkFailureItem{ID: id, Code: "not_found", Message: "issue not found"}
	case errors.Is(err, issues.ErrPinnedNoForce):
		return bulkFailureItem{ID: id, Code: "pinned_no_force",
			Message: "pinned issue requires force=true to close"}
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return bulkFailureItem{ID: id, Code: "version_conflict",
			Message: "issue was modified concurrently"}
	}
	if ve, ok := issues.IsValidationError(err); ok {
		return bulkFailureItem{ID: id, Code: ve.Code, Message: ve.Message}
	}
	slog.Error("bulk action: unmapped error", "issueId", id, "error", err.Error())
	return bulkFailureItem{ID: id, Code: "internal_error", Message: "operation failed"}
}

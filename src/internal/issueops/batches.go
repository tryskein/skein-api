package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"strings"
	"time"

	"github.com/google/uuid"
)

// batches.go — the batches of issue writes (MCP skein_update_many,
// skein_close_many, skein_dep_add_many; the public API's bulkUpdateIssues,
// bulkCloseIssues, bulkAddDependencies): each row is atomic on its own, the
// batch answers a result or a refusal code per row.

// BatchLoop bounds the loop of the rows of a batch: the context it returns
// ends at the batch deadline.
type BatchLoop func(parent context.Context) (context.Context, context.CancelFunc)

// BatchResultRow is the per-row entry in any batch tool's `results`
// array. ok=true success carries a Version (post-state); ok=false
// carries a structured Error matching the single-tool error envelope.
type BatchResultRow struct {
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Version int            `json:"version,omitempty"`
	Error   *BatchErrorDTO `json:"error,omitempty"`
}

// BatchErrorDTO mirrors the body skmcp.ToolError emits for the
// single-issue tools, minus the top-level wrapping.  Code is the
// stable error string ("version_conflict" etc.); Message is the
// human-readable detail; the rest of the structured fields land in
// Details for the agent to inspect.
type BatchErrorDTO struct {
	Code    string         `json:"code"`
	Message string         `json:"message,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// BatchSummary is the per-call rollup. Lets the agent log "X/Y
// succeeded" without re-iterating results[].
type BatchSummary struct {
	Requested int `json:"requested"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// BatchResponse is the full MCP envelope for every batch tool. Agents
// always receive HTTP 200 — partial-success is encoded in the body via
// per-row OK/Error pairs.
type BatchResponse struct {
	Results []BatchResultRow `json:"results"`
	Summary BatchSummary     `json:"summary"`
}

// CloseManyResultRow is skein_close_many's OWN per-row shape: the
// reference row's four keys plus the close outcome and the close-target
// link state (sk-489, US-9).
//
// # Why a new type instead of two fields on BatchResultRow
//
// US-5 names the group close/update response THE reference form and
// forbids repairing it. Widening
// BatchResultRow would change the wire of skein_update_many and
// skein_dep_add_many too — tools that close nothing and for which
// `closeOutcome` would be a key about somebody else's operation.
//
// So this is a DELIBERATE contract extension of ONE batch tool, made by
// giving that tool its own pair, and the reference pair stays untouched
// and still guarded. US-9 requires the outcome in the confirmations of
// the three closing surfaces; the batch close is one of them, and a row
// that reported `ok:true` without saying what the issue ended AS would
// force a follow-up read per row — the opposite of what a batch is for.
//
// The first four keys are BatchResultRow's, name for name and type for
// type, so a caller that parses a batch row parses this. They are spelled
// out rather than embedded for the reason ackRow states: embedding would
// tie the two shapes together and make any future change to the reference
// row change this one silently.
type CloseManyResultRow struct {
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Version int            `json:"version,omitempty"`
	Error   *BatchErrorDTO `json:"error,omitempty"`

	// CloseOutcome is the row's post-state outcome, same Go type and same
	// wire name as on the single acknowledgement (ack.go#ackRow).
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseLink is PER ROW, not per call: one item may create the edge and
	// the next find it already there, in the same request. A per-call flag
	// would have to pick one of those answers and be wrong about the other.
	CloseLink issues.CloseLinkState `json:"closeLink,omitempty"`
}

// CloseManyResponse is skein_close_many's envelope. Identical in shape to
// BatchResponse — same two keys, same summary type — with the rows being
// this tool's own. The envelope is NOT reused so that the guard pinning
// BatchResponse keeps guarding only the tools it still describes.
type CloseManyResponse struct {
	Results []CloseManyResultRow `json:"results"`
	Summary BatchSummary         `json:"summary"`
}

// CloseManyItem is the parsed shape of one input items[] entry.
type CloseManyItem struct {
	ID              string
	ExpectedVersion int
	Reason          string
	Commits         []issues.CloseCommitInput
	// CloseOutcome / CloseTarget — sk-489, per item. They are parsed AND
	// checked in ParseCloseManyItem, not here and not in close.go: this
	// batch never calls the single tool's handler, so a check that lives
	// only there does not exist for these rows at all.
	CloseOutcome *string
	CloseTarget  string
}

// ParsedCloseItem is one partially-parsed input row. parseErr is set
// when validation fails — the dispatcher then emits a results[i] with
// the captured error and skips the store call for that row.
type ParsedCloseItem struct {
	rawID    string
	item     CloseManyItem
	parseErr *BatchErrorDTO
}

func ParseCloseManyItem(raw any) ParsedCloseItem {
	out := ParsedCloseItem{}
	obj, ok := raw.(map[string]any)
	if !ok {
		out.parseErr = &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i] must be an object",
		}
		return out
	}

	idAny, _ := obj["id"]
	rawID, _ := idAny.(string)
	out.rawID = rawID
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		out.parseErr = &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i].id must be a full hierarchical id like \"demo-42\"",
			Details: map[string]any{"id": rawID},
		}
		return out
	}

	expectedVersion, vErr := ReadPositiveInt(obj, "expectedVersion")
	if vErr != nil {
		out.parseErr = vErr
		return out
	}

	rawReason, _ := obj["reason"].(string)
	reason := strings.TrimSpace(rawReason)
	if reason == "" {
		out.parseErr = &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i].reason is required and must be non-empty after trim",
			Details: map[string]any{"id": id},
		}
		return out
	}

	// sk-489 — the SECOND input builder. The same helper as in the
	// single skein_close: the parsing rules and all target checks
	// that need no database must sound here word for word the same, otherwise
	// a refusal not intercepted here would reach ClassifyCloseError and become
	// internal_error at the batch position.
	outcome, outcomeErr := ParseCloseOutcomeArgs(obj, id, "items[i].")
	if outcomeErr != nil {
		details := outcomeErr.Fields
		if details == nil {
			details = map[string]any{}
		}
		if _, ok := details["id"]; !ok {
			details["id"] = id
		}
		out.parseErr = &BatchErrorDTO{
			Code:    outcomeErr.Code,
			Message: outcomeErr.Message,
			Details: details,
		}
		return out
	}

	commits, commitErr := ParseCommitInputs(obj["commits"], "items[i].commits")
	if commitErr != nil {
		// Annotate with the issue id so the caller can pinpoint
		// the offending row in a partial-success batch.
		details := commitErr.Fields
		if details == nil {
			details = map[string]any{}
		}
		if _, ok := details["id"]; !ok {
			details["id"] = id
		}
		out.parseErr = &BatchErrorDTO{
			Code:    commitErr.Code,
			Message: commitErr.Message,
			Details: details,
		}
		return out
	}

	out.item = CloseManyItem{
		ID:              id,
		ExpectedVersion: expectedVersion,
		Reason:          reason,
		Commits:         commits,
		CloseOutcome:    outcome.Outcome,
		CloseTarget:     outcome.Target,
	}
	return out
}

// ReadPositiveInt extracts a positive-integer field from a map[string]any
// produced by JSON unmarshal. Numbers may arrive as float64 (mcp-go
// passes them through encoding/json) or json.Number. Both are accepted.
func ReadPositiveInt(obj map[string]any, key string) (int, *BatchErrorDTO) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return 0, &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i]." + key + " is required",
		}
	}
	var n int
	switch v := raw.(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	default:
		return 0, &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i]." + key + " must be a positive integer",
		}
	}
	if n < 1 {
		return 0, &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i]." + key + " must be >= 1",
		}
	}
	return n, nil
}

// CloseMany runs the batch of skein_close_many for the caller userID; loop bounds the
// loop of rows (the batch deadline).
func CloseMany(ctx context.Context, d Deps, userID uuid.UUID, a Args, loop BatchLoop) (CloseManyResponse, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// per issue on its project (forceUnlockGate), not by the role.

	rawItems, ok := a.GetArguments()["items"]
	if !ok || rawItems == nil {
		return CloseManyResponse{}, Fail(ErrCodeValidationError,
			"items is required and must be a non-empty array", nil)
	}
	rawSlice, ok := rawItems.([]any)
	if !ok {
		return CloseManyResponse{}, Fail(ErrCodeValidationError,
			"items must be an array of objects", nil)
	}
	if len(rawSlice) == 0 {
		return CloseManyResponse{}, Fail(ErrCodeValidationError,
			"items must contain at least 1 entry", nil)
	}
	if len(rawSlice) > BatchMaxItems {
		return CloseManyResponse{}, Fail(ErrCodeValidationError,
			"items exceeds the per-call cap",
			map[string]any{"max": BatchMaxItems, "got": len(rawSlice)})
	}

	// Parse + per-row validation. We do NOT fail the entire batch on
	// a single bad row — invalid rows produce a results[i].ok=false
	// entry with code=validation_error and the rest of the batch
	// runs. This matches the partial-success contract documented in
	// the tool description.
	items := make([]ParsedCloseItem, len(rawSlice))
	for i, raw := range rawSlice {
		items[i] = ParseCloseManyItem(raw)
	}

	sessionID := auth.SessionIDFrom(ctx)
	actor := IssuesAgentActor(ctx, d.Pool, userID)

	// See batchLoopDeadline: the loop stops on our clock so the caller
	// learns which rows closed instead of losing the response.
	loopCtx, cancelLoop := loop(ctx)
	defer cancelLoop()

	results := make([]CloseManyResultRow, len(items))
	var succeeded, failed int

	for i, parsed := range items {
		// Bail out cleanly if the client cancelled — fill remaining
		// rows as cancelled and stop (same client-visible result as
		// per-row continue, without extra dead-ctx iterations).
		if code, message, stop := BatchStopReason(ctx, loopCtx); stop {
			for j := i; j < len(items); j++ {
				results[j] = CloseManyResultRow{
					ID: items[j].rawID,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    code,
						Message: message,
					},
				}
				failed++
			}
			slog.WarnContext(ctx, "mcp batch stopped early",
				"op", "close_many", "code", code,
				"appliedRows", succeeded, "unrunRows", len(items)-i,
				"userId", userID.String())
			break
		}
		if parsed.parseErr != nil {
			results[i] = CloseManyResultRow{
				ID:    parsed.rawID,
				OK:    false,
				Error: parsed.parseErr,
			}
			failed++
			continue
		}

		// Visibility check — same shape as skein_close. Per-row, so
		// a missing project on one id doesn't poison the rest.
		issue, err := d.Issues.GetIssue(loopCtx, parsed.item.ID)
		if err != nil {
			// projectID is unknown at this point (the load that would
			// tell us just failed) — uuid.Nil is safe here because a
			// GetIssue failure can never be projects.ErrRepoNotFound /
			// ErrRepoAmbiguous, the only codes that consult it.
			c := ClassifyCloseRowError(ctx, loopCtx, d.Pool, uuid.Nil, err, userID.String(), parsed.item.ID, parsed.item.CloseTarget)
			results[i] = CloseManyResultRow{
				ID:    parsed.item.ID,
				OK:    false,
				Error: &BatchErrorDTO{Code: c.Code, Message: c.Message, Details: c.Details},
			}
			failed++
			continue
		}
		// sk-674.18 T066: closing is ActionWrite of US-6.
		rowLevel, vErr := projects.EnsureProjectAction(loopCtx, d.Pool, issue.ProjectID, userID, authz.ActionWrite)
		if vErr == nil && force && projects.RequireAction(rowLevel, authz.ActionForceUnlock) != nil {
			// sk-674.18 T034 (K-28): force on this row's project.
			results[i] = CloseManyResultRow{
				ID: parsed.item.ID,
				OK: false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeForceUnlockForbidden,
					Message: MsgForceUnlockForbidden,
					Details: map[string]any{"id": parsed.item.ID},
				},
			}
			failed++
			continue
		}
		if vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				results[i] = CloseManyResultRow{
					ID: parsed.item.ID,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeIssueNotFound,
						Message: "issue does not exist or is not visible",
						Details: map[string]any{"id": parsed.item.ID},
					},
				}
				failed++
				continue
			}
			if errors.Is(vErr, projects.ErrProjectActionForbidden) {
				results[i] = CloseManyResultRow{
					ID: parsed.item.ID,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeForbidden,
						Message: MsgActionForbidden,
						Details: map[string]any{"id": parsed.item.ID},
					},
				}
				failed++
				continue
			}
			slog.ErrorContext(ctx, "issue operation error",
				"op", "close_many",
				"reason", "visibility check failed",
				"userId", userID.String(),
				"issueId", parsed.item.ID,
				"error", vErr.Error())
			results[i] = CloseManyResultRow{
				ID: parsed.item.ID,
				OK: false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeInternalError,
					Message: "visibility check failed",
				},
			}
			failed++
			continue
		}

		// sk-607: CloseCommitted, not CloseWithResult — the row is
		// {id, ok, version, closeOutcome, closeLink}, all of which the
		// committing transaction read. The full card per row was loaded
		// only to be dropped, and it was that post-commit read the
		// batch's own deadline cut in production, reporting committed
		// closes as internal_error.
		state, link, err := d.Issues.CloseCommitted(loopCtx, issues.CloseInput{
			ID:              parsed.item.ID,
			ExpectedVersion: parsed.item.ExpectedVersion,
			Reason:          parsed.item.Reason,
			Force:           force,
			SessionID:       sessionID,
			Actor:           actor,
			Commits:         parsed.item.Commits,
			CloseOutcome:    parsed.item.CloseOutcome,
			CloseTarget:     parsed.item.CloseTarget,
			// sk-540.1: an explicit outcome is mandatory on a transition;
			// a row without one fails alone, the rest still close.
			RequireOutcomeOnTransition: true,
			// The system role — see close.go. Required with a non-empty
			// target, and always passed: "pass only when
			// needed" is one refactoring away from a store failure.
			DependenciesCaller: &issues.DependenciesCaller{UserID: userID, Role: role},
		})
		if err != nil {
			c := ClassifyCloseRowError(ctx, loopCtx, d.Pool, issue.ProjectID, err, userID.String(), parsed.item.ID, parsed.item.CloseTarget)
			results[i] = CloseManyResultRow{
				ID:    parsed.item.ID,
				OK:    false,
				Error: &BatchErrorDTO{Code: c.Code, Message: c.Message, Details: c.Details},
			}
			failed++
			continue
		}

		results[i] = CloseManyResultRow{
			ID:           parsed.item.ID,
			OK:           true,
			Version:      state.Version,
			CloseOutcome: state.CloseOutcome,
			CloseLink:    link,
		}
		succeeded++
	}

	slog.InfoContext(ctx, "issue operation ok",
		"op", "close_many",
		"userId", userID.String(),
		"requested", len(items),
		"succeeded", succeeded,
		"failed", failed,
		"force", force,
		"latencyMs", time.Since(start).Milliseconds())

	return CloseManyResponse{
		Results: results,
		Summary: BatchSummary{
			Requested: len(items),
			Succeeded: succeeded,
			Failed:    failed,
		},
	}, nil
}

// UpdateRowContextStop renders a skein_update_many row whose work was cut by
// a context — the batch loop's own deadline (batchLoopDeadline) or the
// caller going away — while the row was in progress, or nil when err is not
// that (readback.go#ContextStop decides which code). Before sk-647.2 such a
// row answered internal_error, the code of a row that broke while being
// written, although nothing broke: the server stopped on its own clock.
//
// The message is NOT ContextStop's «did not report a commit, resend with the
// same expectedVersion»: the update chain (update_run.go#runUpdateForOne)
// commits step by step, so a cut in the middle can leave the steps before it
// applied. The row therefore says so and asks for a re-read first; the
// guide's rule for these codes — compare the live state, resend only what is
// missing — covers the rest.
func UpdateRowContextStop(parent, loop context.Context, err error, issueID string) *BatchErrorDTO {
	code, _, ok := ContextStop(parent, loop, err)
	if !ok {
		return nil
	}
	message := "the request ended while this row was in progress; the row may have committed as a " +
		"whole — re-read the issue before resending it"
	if code == ErrCodeBatchDeadlineExceeded {
		message = "the batch reached its server-side deadline while this row was in progress; the row " +
			"may have committed as a whole — re-read the issue before resending it"
	}
	return &BatchErrorDTO{
		Code:    code,
		Message: message,
		Details: map[string]any{"id": issueID},
	}
}

// ClassifyUpdateError mirrors mapUpdateError but returns a
// BatchErrorDTO directly (no CallToolResult round-trip) so the batch
// tool keeps the per-row error envelope identical to the single tool's
// output without re-marshalling.
func ClassifyUpdateError(ctx context.Context, err error, tool, userID, issueID string) *BatchErrorDTO {
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		return &BatchErrorDTO{
			Code:    ErrCodeAssigneeNotAllowed,
			Message: "the assignee chosen by id may not work in this project; nothing was changed",
			Details: map[string]any{"id": issueID},
		}
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		// sk-118 (F2c): promote the standing transition guard codes to the
		// top-level batch row `code` (parity with mapUpdateError).
		if code, ok := StandingTransitionCode(vErr.Code); ok {
			return &BatchErrorDTO{
				Code:    code,
				Message: vErr.Message,
				Details: map[string]any{"id": issueID},
			}
		}
		return &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: vErr.Message,
			Details: map[string]any{"code": vErr.Code},
		}
	}
	var labelErr *ErrLabelNotPresent
	if errors.As(err, &labelErr) {
		return &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "label is not currently set on this issue",
			Details: map[string]any{"label": labelErr.Label},
		}
	}
	var verr *issues.ErrVersionConflict
	if errors.Is(err, issues.ErrSerializationRetry) {
		return &BatchErrorDTO{
			Code:    ErrCodeSerializationRetry,
			Message: "the write transaction lost a serialization race; nothing was written — resend the row unchanged",
			Details: map[string]any{"id": issueID},
		}
	}
	if errors.As(err, &verr) {
		return &BatchErrorDTO{
			Code:    ErrCodeVersionConflict,
			Message: "the issue was modified by another writer",
			Details: map[string]any{"currentVersion": verr.CurrentVersion},
		}
	}
	var lockErr *issues.ErrLockedByOther
	if errors.As(err, &lockErr) {
		return &BatchErrorDTO{
			Code:    ErrCodeLockedByOther,
			Message: "issue is locked by another session",
			Details: map[string]any{"id": issueID, "lockedBy": lockErr.LockedBy},
		}
	}
	if errors.Is(err, issues.ErrNotFound) {
		return &BatchErrorDTO{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"id": issueID},
		}
	}
	if errors.Is(err, issues.ErrPinnedNoForce) {
		return &BatchErrorDTO{
			Code:    ErrCodePinnedNoForce,
			Message: "pinned issues require the dedicated unpin tool to be closed",
		}
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		return &BatchErrorDTO{
			Code:    ErrCodeComponentsWrongProject,
			Message: "some components do not belong to this project",
			Details: map[string]any{"componentIds": cerr.ComponentIDs},
		}
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", tool,
		"reason", "store call failed",
		"userId", userID, "issueId", issueID, "error", err.Error())
	return &BatchErrorDTO{
		Code:    ErrCodeInternalError,
		Message: "update failed",
	}
}

// UpdateMany runs the batch of skein_update_many for the caller userID; loop bounds the
// loop of rows (the batch deadline).
func UpdateMany(ctx context.Context, d Deps, userID uuid.UUID, a Args, loop BatchLoop) (BatchResponse, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// per issue on its project (forceUnlockGate), not by the role.

	rawItems, ok := a.GetArguments()["items"]
	if !ok || rawItems == nil {
		return BatchResponse{}, Fail(ErrCodeValidationError,
			"items is required and must be a non-empty array", nil)
	}
	rawSlice, ok := rawItems.([]any)
	if !ok {
		return BatchResponse{}, Fail(ErrCodeValidationError,
			"items must be an array of objects", nil)
	}
	if len(rawSlice) == 0 {
		return BatchResponse{}, Fail(ErrCodeValidationError,
			"items must contain at least 1 entry", nil)
	}
	if len(rawSlice) > BatchMaxItems {
		return BatchResponse{}, Fail(ErrCodeValidationError,
			"items exceeds the per-call cap",
			map[string]any{"max": BatchMaxItems, "got": len(rawSlice)})
	}

	sessionID := auth.SessionIDFrom(ctx)
	actor := IssuesAgentActor(ctx, d.Pool, userID)

	// The row work runs under our own deadline so the response is
	// written while the caller is still there to read it — see
	// batchLoopDeadline. Logging keeps the parent context.
	loopCtx, cancelLoop := loop(ctx)
	defer cancelLoop()

	results := make([]BatchResultRow, len(rawSlice))
	var succeeded, failed int

	// Visibility is a property of the PROJECT, and a batch is
	// overwhelmingly one project's worth of issues, so the same answer
	// was being fetched once per row. Cached for the call only — a
	// membership change mid-batch is not a case worth a round-trip per
	// row, and every row of this batch was authorised in the same
	// instant either way.
	//
	// ONLY definitive answers are cached: visible, and
	// ErrProjectNotVisible. A transient failure — a d.Pool checkout that
	// blipped, a scan that lost its connection — is not an answer about
	// the project, and caching it would replay one row's bad luck as
	// internal_error onto every later row of that project without ever
	// asking again.
	visible := make(map[uuid.UUID]error)
	// levels holds the caller's level per visible project, so each row
	// asks the action table of US-6 for its own actions without another
	// query (sk-674.18 T066).
	levels := make(map[uuid.UUID]projects.AccessLevel)

	for i, raw := range rawSlice {
		// Stopping early: mark this and all remaining rows with the
		// reason, then stop — same caller-visible result without extra
		// iterations. The rows above are already committed and stay so.
		if code, message, stop := BatchStopReason(ctx, loopCtx); stop {
			for j := i; j < len(rawSlice); j++ {
				// Carry the id whenever the row parses far enough to
				// have one: "retry the unrun rows" is easier to act on
				// than "zip these by input order", even though the
				// order is guaranteed.
				unrunID := ""
				if obj, okObj := rawSlice[j].(map[string]any); okObj {
					unrunID, _ = obj["id"].(string)
				}
				results[j] = BatchResultRow{
					ID: unrunID,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    code,
						Message: message,
					},
				}
				failed++
			}
			slog.WarnContext(ctx, "mcp batch stopped early",
				"op", "update_many", "code", code,
				"appliedRows", succeeded, "unrunRows", len(rawSlice)-i,
				"userId", userID.String())
			break
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			results[i] = BatchResultRow{
				OK: false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeValidationError,
					Message: "items[i] must be an object",
				},
			}
			failed++
			continue
		}

		rawID, _ := obj["id"].(string)
		id := strings.TrimSpace(rawID)
		if !LooksLikeHierarchicalID(id) {
			results[i] = BatchResultRow{
				ID: rawID,
				OK: false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeValidationError,
					Message: "items[i].id must be a full hierarchical id like \"demo-42\"",
					Details: map[string]any{"id": rawID},
				},
			}
			failed++
			continue
		}

		expectedVersion, vErr := ReadPositiveInt(obj, "expectedVersion")
		if vErr != nil {
			results[i] = BatchResultRow{ID: id, OK: false, Error: vErr}
			failed++
			continue
		}

		// Build the run-input from this item's args. Reuses the
		// same parser the single-tool handler does; per-row
		// validation_error is captured as an in-place row failure
		// rather than aborting the batch.
		// The system-role carrier is ALWAYS passed, even though this surface
		// rejects the target: card T009
		// requires "both builders fill it", and the reason is in the
		// doc block of update_run.go#updateOneInput.
		in, parseRes := BuildUpdateOneInput(obj, d.Pool, ctx, id, expectedVersion,
			force, sessionID, actor,
			&issues.DependenciesCaller{UserID: userID, Role: role})
		if parseRes != nil {
			results[i] = BatchResultRow{
				ID:    id,
				OK:    false,
				Error: batchErrorFromFailure(parseRes),
			}
			failed++
			continue
		}

		// Visibility check — read the owning project, ensure it is
		// visible. Per-row, AFTER the row shape is parsed — and since sk-516 this
		// is indeed the same order as in the single
		// update.go#handleUpdate: before that change the single one went to the
		// database FIRST, and the claim that the orders match was wrong. The
		// proof of this half is
		// update_close_outcome_test.go#
		// TestUpdateManyMCP_FormalRefusalPrecedesTheStore.
		// Only the project id is needed here, and reading it costs one
		// query where loading the whole issue costs six.
		projectID, err := d.Issues.GetIssueProjectID(loopCtx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				results[i] = BatchResultRow{
					ID: id,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeIssueNotFound,
						Message: "issue does not exist or is not visible",
						Details: map[string]any{"id": id},
					},
				}
			} else if stopErr := UpdateRowContextStop(ctx, loopCtx, err, id); stopErr != nil {
				results[i] = BatchResultRow{ID: id, OK: false, Error: stopErr}
			} else {
				slog.ErrorContext(ctx, "issue operation error",
					"op", "update_many",
					"reason", "load issue failed",
					"userId", userID.String(), "issueId", id, "error", err.Error())
				results[i] = BatchResultRow{
					ID: id,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeInternalError,
						Message: "failed to load issue",
					},
				}
			}
			failed++
			continue
		}
		visErr, checked := visible[projectID]
		if !checked {
			var level projects.AccessLevel
			level, visErr = projects.EnsureProjectVisible(loopCtx, d.Pool, projectID, userID)
			if visErr == nil || errors.Is(visErr, projects.ErrProjectNotVisible) {
				visible[projectID] = visErr
				levels[projectID] = level
			}
		}
		if visErr == nil {
			// Editing is ActionWrite of US-6; a row changing the
			// assignee is ActionAssign too.
			rowActions := []authz.Action{authz.ActionWrite}
			if _, ok := obj["assignee"]; ok {
				rowActions = append(rowActions, authz.ActionAssign)
			}
			if force && projects.RequireAction(levels[projectID], rowActions...) == nil &&
				projects.RequireAction(levels[projectID], authz.ActionForceUnlock) != nil {
				// sk-674.18 T034 (K-28): force on this row's project.
				results[i] = BatchResultRow{
					ID: id,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeForceUnlockForbidden,
						Message: MsgForceUnlockForbidden,
						Details: map[string]any{"id": id},
					},
				}
				failed++
				continue
			}
			if projects.RequireAction(levels[projectID], rowActions...) != nil {
				results[i] = BatchResultRow{
					ID: id,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeForbidden,
						Message: MsgActionForbidden,
						Details: map[string]any{"id": id},
					},
				}
				failed++
				continue
			}
		}
		if visErr != nil {
			if errors.Is(visErr, projects.ErrProjectNotVisible) {
				results[i] = BatchResultRow{
					ID: id,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeIssueNotFound,
						Message: "issue does not exist or is not visible",
						Details: map[string]any{"id": id},
					},
				}
			} else if stopErr := UpdateRowContextStop(ctx, loopCtx, visErr, id); stopErr != nil {
				results[i] = BatchResultRow{ID: id, OK: false, Error: stopErr}
			} else {
				slog.ErrorContext(ctx, "issue operation error",
					"op", "update_many",
					"reason", "visibility check failed",
					"userId", userID.String(), "issueId", id, "error", visErr.Error())
				results[i] = BatchResultRow{
					ID: id,
					OK: false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeInternalError,
						Message: "visibility check failed",
					},
				}
			}
			failed++
			continue
		}

		version, err := RunUpdateChainVersion(loopCtx, d.Issues, in)
		if err != nil {
			dto := UpdateRowContextStop(ctx, loopCtx, err, id)
			if dto == nil {
				dto = ClassifyUpdateError(ctx, err, "skein_update_many", userID.String(), id)
			}
			results[i] = BatchResultRow{ID: id, OK: false, Error: dto}
			failed++
			continue
		}
		results[i] = BatchResultRow{
			ID:      id,
			OK:      true,
			Version: version,
		}
		succeeded++
	}

	slog.InfoContext(ctx, "issue operation ok",
		"op", "update_many",
		"userId", userID.String(),
		"requested", len(rawSlice),
		"succeeded", succeeded,
		"failed", failed,
		"force", force,
		"latencyMs", time.Since(start).Milliseconds())

	return BatchResponse{
		Results: results,
		Summary: BatchSummary{
			Requested: len(rawSlice),
			Succeeded: succeeded,
			Failed:    failed,
		},
	}, nil
}

// DepAddManyResultRow extends BatchResultRow with the resolved
// dependency DTO on success — there is no version on dependency
// rows so the bare BatchResultRow's Version field is unused.
type DepAddManyResultRow struct {
	FromID string         `json:"fromId"`
	ToID   string         `json:"toId"`
	Type   string         `json:"type,omitempty"`
	OK     bool           `json:"ok"`
	Dep    *DepDTO        `json:"dependency,omitempty"`
	Error  *BatchErrorDTO `json:"error,omitempty"`
}

// DepAddManyResponse mirrors BatchResponse but with the row shape
// above — keeps the on-the-wire shape obvious without forcing the
// agent to map (id, ?) onto (fromId, toId) when reading.
type DepAddManyResponse struct {
	Results []DepAddManyResultRow `json:"results"`
	Summary BatchSummary          `json:"summary"`
}

// DepErrorDTOFromValidation re-uses the dependencies.ValidationError
// → BatchErrorDTO mapping the single tool surfaces.
func DepErrorDTOFromValidation(err error) *BatchErrorDTO {
	if ve, ok := dependencies.IsValidationError(err); ok {
		return &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: ve.Message,
			Details: map[string]any{"code": ve.Code},
		}
	}
	return &BatchErrorDTO{
		Code:    ErrCodeValidationError,
		Message: err.Error(),
	}
}

// DepRowContextStop renders a skein_dep_add_many row whose work was cut by
// a context — the batch loop's own deadline (batchLoopDeadline) or the
// caller going away — while the row was in progress, or nil when err is not
// that (readback.go#ContextStop decides). The row then answers the code the
// caller already resends on, batch_deadline_exceeded / request_cancelled,
// and not internal_error, which is what production returned when the
// deadline fired inside Store.Create (sk-647.2; the close_many twin is
// close_errors.go#ClassifyCloseRowError, sk-607).
//
// The message is the edge's own, not ContextStop's: a dependency has no
// expectedVersion, and none is needed to resend safely — the row is one
// transaction, and an edge that did commit before the cut answers the
// resend with dependency_already_exists.
func DepRowContextStop(parent, loop context.Context, err error, fromID, toID, depType string) *BatchErrorDTO {
	code, _, ok := ContextStop(parent, loop, err)
	if !ok {
		return nil
	}
	message := "the request ended while this edge was in progress; it did not report a commit — " +
		"resend it if still wanted: an edge that did land answers dependency_already_exists"
	if code == ErrCodeBatchDeadlineExceeded {
		message = "the batch reached its server-side deadline while this edge was in progress; it did not report a commit — " +
			"resend it: an edge that did land answers dependency_already_exists"
	}
	return &BatchErrorDTO{
		Code:    code,
		Message: message,
		Details: map[string]any{"fromId": fromID, "toId": toID, "type": depType},
	}
}

// ClassifyDepAddError mirrors mapDepAddError but returns a
// BatchErrorDTO directly. Same closed set of error codes; preserves
// every structured detail (path[] on cycle, fromId/toId/type on
// already-exists, etc.) the agent uses to retry. A row cut by a context
// (loopCtx's deadline or the caller leaving) is DepRowContextStop's.
func ClassifyDepAddError(ctx, loopCtx context.Context, err error, userID, fromID, toID, depType string) *BatchErrorDTO {
	if stopErr := DepRowContextStop(ctx, loopCtx, err, fromID, toID, depType); stopErr != nil {
		return stopErr
	}
	if vErr, ok := dependencies.IsValidationError(err); ok {
		// sk-118 (F2e): the nested-standing ban (parent-child edge where both
		// ends are standing) is a *ValidationError but carries an actionable,
		// branch-on-able code. Promote it to the row-level code so this batch
		// surface matches the single-row skein_dep_add / skein_reparent
		// contract (which promote nested_standing_forbidden to the envelope
		// `error` rather than burying it under validation_error). Preserve the
		// fromId/toId/type details so the agent can pinpoint the failing row.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return &BatchErrorDTO{
				Code:    ErrCodeNestedStandingForbidden,
				Message: vErr.Message,
				Details: map[string]any{
					"code":   vErr.Code,
					"fromId": fromID,
					"toId":   toID,
					"type":   depType,
				},
			}
		}
		// sk-118 (F2f): blocker-target standing ban — same row-level promotion so
		// the batch surface matches the single-row skein_dep_add contract.
		if vErr.Code == ErrCodeBlockerTargetStanding {
			return &BatchErrorDTO{
				Code:    ErrCodeBlockerTargetStanding,
				Message: vErr.Message,
				Details: map[string]any{
					"code":   vErr.Code,
					"fromId": fromID,
					"toId":   toID,
					"type":   depType,
				},
			}
		}
		return &BatchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: vErr.Message,
			Details: map[string]any{"code": vErr.Code},
		}
	}
	// Every remaining store error renders identically on both dep-add
	// surfaces (same code, message and details) — only the envelope
	// differs. ClassifyDepStoreError owns that shared table; the
	// retryable codes (serialization_retry,
	// dependency_target_not_visible_yet) come out of it unchanged so the
	// agent still re-submits the row.
	if cls := ClassifyDepStoreError(err, fromID, toID, depType); cls != nil {
		return &BatchErrorDTO{
			Code:    cls.Code,
			Message: cls.Message,
			Details: cls.Details,
		}
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_dep_add_many",
		"reason", "store create failed",
		"userId", userID,
		"fromId", fromID, "toId", toID, "type", depType, "error", err.Error())
	return &BatchErrorDTO{
		Code:    ErrCodeInternalError,
		Message: "failed to create dependency",
	}
}

// DepAddMany runs the batch of skein_dep_add_many for the caller userID; loop bounds the
// loop of rows (the batch deadline).
func DepAddMany(ctx context.Context, d Deps, userID uuid.UUID, a Args, loop BatchLoop) (DepAddManyResponse, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	rawItems, ok := a.GetArguments()["items"]
	if !ok || rawItems == nil {
		return DepAddManyResponse{}, Fail(ErrCodeValidationError,
			"items is required and must be a non-empty array", nil)
	}
	rawSlice, ok := rawItems.([]any)
	if !ok {
		return DepAddManyResponse{}, Fail(ErrCodeValidationError,
			"items must be an array of objects", nil)
	}
	if len(rawSlice) == 0 {
		return DepAddManyResponse{}, Fail(ErrCodeValidationError,
			"items must contain at least 1 entry", nil)
	}
	if len(rawSlice) > BatchMaxItems {
		return DepAddManyResponse{}, Fail(ErrCodeValidationError,
			"items exceeds the per-call cap",
			map[string]any{"max": BatchMaxItems, "got": len(rawSlice)})
	}

	actor := DependenciesAgentActor(ctx, d.Pool, userID)

	// See batchLoopDeadline: the loop stops on our clock so the caller
	// learns which edges landed instead of losing the response.
	loopCtx, cancelLoop := loop(ctx)
	defer cancelLoop()
	caller := dependencies.CallerAccess{UserID: userID, Role: role}

	results := make([]DepAddManyResultRow, len(rawSlice))
	var succeeded, failed int

	for i, raw := range rawSlice {
		if code, message, stop := BatchStopReason(ctx, loopCtx); stop {
			// Fill every remaining slot and stop, rather than walking
			// the rest of the batch against a dead context one row at a
			// time. Same shape as skein_update_many and
			// skein_close_many: the rows above are committed and stay
			// so, and the WARN line is how an incident like this is
			// found afterwards.
			for j := i; j < len(rawSlice); j++ {
				unrunFrom, unrunTo := "", ""
				if obj, okObj := rawSlice[j].(map[string]any); okObj {
					unrunFrom, _ = obj["fromId"].(string)
					unrunTo, _ = obj["toId"].(string)
				}
				results[j] = DepAddManyResultRow{
					FromID: unrunFrom,
					ToID:   unrunTo,
					OK:     false,
					Error: &BatchErrorDTO{
						Code:    code,
						Message: message,
					},
				}
				failed++
			}
			slog.WarnContext(ctx, "mcp batch stopped early",
				"op", "dep_add_many", "code", code,
				"appliedRows", succeeded, "unrunRows", len(rawSlice)-i,
				"userId", userID.String())
			break
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			results[i] = DepAddManyResultRow{
				OK: false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeValidationError,
					Message: "items[i] must be an object",
				},
			}
			failed++
			continue
		}

		rawFromID, _ := obj["fromId"].(string)
		fromID := strings.TrimSpace(rawFromID)
		if !LooksLikeHierarchicalID(fromID) {
			results[i] = DepAddManyResultRow{
				FromID: rawFromID,
				OK:     false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeValidationError,
					Message: "items[i].fromId must be a full hierarchical id",
					Details: map[string]any{"fromId": rawFromID},
				},
			}
			failed++
			continue
		}
		rawToID, _ := obj["toId"].(string)
		toID := strings.TrimSpace(rawToID)
		if !LooksLikeHierarchicalID(toID) {
			results[i] = DepAddManyResultRow{
				FromID: fromID,
				ToID:   rawToID,
				OK:     false,
				Error: &BatchErrorDTO{
					Code:    ErrCodeValidationError,
					Message: "items[i].toId must be a full hierarchical id",
					Details: map[string]any{"toId": rawToID},
				},
			}
			failed++
			continue
		}
		rawType, _ := obj["type"].(string)
		depType, vErr := dependencies.ValidateType(rawType)
		if vErr != nil {
			results[i] = DepAddManyResultRow{
				FromID: fromID,
				ToID:   toID,
				OK:     false,
				Error:  DepErrorDTOFromValidation(vErr),
			}
			failed++
			continue
		}

		var gateArg *string
		if g, ok := obj["gate"].(string); ok {
			if trimmed := strings.TrimSpace(g); trimmed != "" {
				gateArg = &trimmed
			}
		}
		if _, vErr := dependencies.ValidateGate(depType, gateArg); vErr != nil {
			results[i] = DepAddManyResultRow{
				FromID: fromID,
				ToID:   toID,
				Type:   depType,
				OK:     false,
				Error:  DepErrorDTOFromValidation(vErr),
			}
			failed++
			continue
		}

		// Pre-check FROM-side visibility — same shape as
		// skein_dep_add. Surfaces issue_not_found rather than the
		// generic "from-issue not found".
		fromIssue, err := d.Issues.GetIssue(loopCtx, fromID)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeIssueNotFound,
						Message: "issue does not exist or is not visible",
						Details: map[string]any{"fromId": fromID},
					},
				}
			} else if stopErr := DepRowContextStop(ctx, loopCtx, err, fromID, toID, depType); stopErr != nil {
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error:  stopErr,
				}
			} else {
				slog.ErrorContext(ctx, "issue operation error",
					"op", "dep_add_many",
					"reason", "load fromId failed",
					"userId", userID.String(), "fromId", fromID, "error", err.Error())
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeInternalError,
						Message: "failed to load fromId",
					},
				}
			}
			failed++
			continue
		}
		// sk-674.18 T066: linking is ActionWrite of US-6 on the FROM side.
		if _, vErr := projects.EnsureProjectAction(loopCtx, d.Pool, fromIssue.ProjectID, userID, authz.ActionWrite); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeIssueNotFound,
						Message: "issue does not exist or is not visible",
						Details: map[string]any{"fromId": fromID},
					},
				}
			} else if errors.Is(vErr, projects.ErrProjectActionForbidden) {
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeForbidden,
						Message: MsgActionForbidden,
						Details: map[string]any{"fromId": fromID},
					},
				}
			} else if stopErr := DepRowContextStop(ctx, loopCtx, vErr, fromID, toID, depType); stopErr != nil {
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error:  stopErr,
				}
			} else {
				slog.ErrorContext(ctx, "issue operation error",
					"op", "dep_add_many",
					"reason", "fromId visibility check failed",
					"userId", userID.String(), "fromId", fromID, "error", vErr.Error())
				results[i] = DepAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error: &BatchErrorDTO{
						Code:    ErrCodeInternalError,
						Message: "visibility check failed",
					},
				}
			}
			failed++
			continue
		}

		dto, err := d.Deps.Create(loopCtx, fromID, dependencies.CreateDepInput{
			ToID: toID,
			Type: depType,
			Gate: gateArg,
		}, caller, actor)
		if err != nil {
			results[i] = DepAddManyResultRow{
				FromID: fromID,
				ToID:   toID,
				Type:   depType,
				OK:     false,
				Error:  ClassifyDepAddError(ctx, loopCtx, err, userID.String(), fromID, toID, depType),
			}
			failed++
			continue
		}

		var gateOut *string
		if g, ok := dto.Metadata["gate"].(string); ok && g != "" {
			gg := g
			gateOut = &gg
		}
		results[i] = DepAddManyResultRow{
			FromID: fromID,
			ToID:   toID,
			Type:   depType,
			OK:     true,
			Dep: &DepDTO{
				From:      dto.FromID,
				To:        dto.ToID,
				Type:      dto.Type,
				Gate:      gateOut,
				Metadata:  dto.Metadata,
				Version:   dto.Version,
				CreatedAt: dto.CreatedAt,
			},
		}
		succeeded++
	}

	slog.InfoContext(ctx, "issue operation ok",
		"op", "dep_add_many",
		"userId", userID.String(),
		"requested", len(rawSlice),
		"succeeded", succeeded,
		"failed", failed,
		"latencyMs", time.Since(start).Milliseconds())

	return DepAddManyResponse{
		Results: results,
		Summary: BatchSummary{
			Requested: len(rawSlice),
			Succeeded: succeeded,
			Failed:    failed,
		},
	}, nil
}

// DepDTO is the wire shape returned by skein_dep_add. We hoist `gate` out
// of metadata so the agent gets the same field shape on the way out as it
// passed in, without having to dig into a generic map.
type DepDTO struct {
	From      string         `json:"from"`
	To        string         `json:"to"`
	Type      string         `json:"type"`
	Gate      *string        `json:"gate"`
	Metadata  map[string]any `json:"metadata"`
	Version   int            `json:"version"`
	CreatedAt time.Time      `json:"createdAt"`
}

// DepStoreErrorClass is the (code, message, details) triple that the
// single-row (skein_dep_add) and batch (skein_dep_add_many) surfaces render
// identically for a given dependencies.Store failure. Only the envelope
// differs — mapDepAddError wraps it in a CallToolResult, ClassifyDepAddError
// in a BatchErrorDTO.
type DepStoreErrorClass struct {
	Code    string
	Message string
	Details map[string]any
}

// ClassifyDepStoreError maps the dependencies.Store errors whose rendering is
// shared by both dep-add surfaces. Returns nil when err is not one of them —
// the caller then applies its own handling (ValidationError promotion, which
// carries per-surface detail shapes, and the internal_error fallback, which
// logs its own tool name).
func ClassifyDepStoreError(err error, fromID, toID, depType string) *DepStoreErrorClass {
	switch {
	case errors.Is(err, dependencies.ErrFromNotFound):
		// Race vs the pre-check (concurrent delete): surface as
		// issue_not_found, which is the right code regardless.
		return &DepStoreErrorClass{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"fromId": fromID},
		}
	case errors.Is(err, dependencies.ErrTargetNotFound),
		// INV-3 (sk-674.18 T033): a target in a project the caller cannot
		// see answers exactly as a nonexistent one.
		errors.Is(err, dependencies.ErrDependencyTargetNotVisible):
		return &DepStoreErrorClass{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"toId": toID},
		}
	case errors.Is(err, dependencies.ErrDependencyExists):
		return &DepStoreErrorClass{
			Code:    ErrCodeDependencyAlreadyExists,
			Message: "this dependency already exists",
			Details: map[string]any{"fromId": fromID, "toId": toID, "type": depType},
		}
	case errors.Is(err, dependencies.ErrParentAlreadySet):
		// Defect 2 / sk-43: a parent-child edge cannot ADD a second parent.
		// Re-parenting must go through skein_reparent (atomic replace).
		return &DepStoreErrorClass{
			Code:    ErrCodeParentAlreadySet,
			Message: "issue already has a parent — use skein_reparent to move it",
			Details: map[string]any{"fromId": fromID},
		}
	case errors.Is(err, dependencies.ErrSerializationRetry):
		// Transient SERIALIZABLE loss — no durable change; safe to retry.
		return &DepStoreErrorClass{
			Code:    ErrCodeSerializationRetry,
			Message: "dependency write lost a serialization race — safe to retry",
			Details: map[string]any{"fromId": fromID, "toId": toID, "type": depType},
		}
	case errors.Is(err, dependencies.ErrDependencyTargetNotReady):
		// Target row not visible in this snapshot (mid-commit elsewhere) —
		// retry once the target has committed.
		return &DepStoreErrorClass{
			Code:    ErrCodeDependencyTargetNotVisibleYet,
			Message: "dependency target not visible yet — retry shortly",
			Details: map[string]any{"toId": toID},
		}
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(err, &cycle) {
		return &DepStoreErrorClass{
			Code:    ErrCodeDependencyCycle,
			Message: "adding this dependency would create a cycle",
			Details: map[string]any{"path": cycle.Path},
		}
	}
	return nil
}

// batchErrorFromFailure is the row error of f.
func batchErrorFromFailure(f *Failure) *BatchErrorDTO {
	if f == nil {
		return nil
	}
	return &BatchErrorDTO{Code: f.Code, Message: f.Message, Details: f.Fields}
}

// BatchLoopDeadline is the server deadline of the loop of a batch: the rows
// not run by then answer batch_deadline_exceeded.
const BatchLoopDeadline = 20 * time.Second

// DefaultBatchLoop is the BatchLoop of BatchLoopDeadline.
func DefaultBatchLoop(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, BatchLoopDeadline)
}

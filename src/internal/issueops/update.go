package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"strings"
	"time"

	"github.com/google/uuid"
)

// update.go — the change of one issue (MCP skein_update, the public API's
// updateIssue): fields, status (and the close it may be), components and
// labels in one transaction (US-14), or the review flag alone.

// UpdateResult is the outcome of Update.
type UpdateResult struct {
	// IssueID is the issue changed.
	IssueID string
	// Final is the issue after the change; nil when the read-back after
	// the commit failed (ReadBack).
	Final *issues.IssueDetail
	// Link is the state of the close link a close or an outcome patch
	// touched ("" otherwise).
	Link issues.CloseLinkState
	// ReadBack is set when the change committed but the issue could not
	// be read back.
	ReadBack *issues.ErrReadBackFailed
	// ReviewFlag — the call changed only the review flag (it moves neither
	// the version nor updated_at).
	ReviewFlag bool
}

// Update changes an issue for the caller userID from the arguments of
// skein_update.
func Update(ctx context.Context, d Deps, userID uuid.UUID, a Args) (UpdateResult, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	// ---- Required: id + expectedVersion ----
	rawID, err := a.RequireString("id")
	if err != nil {
		return UpdateResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return UpdateResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"",
			map[string]any{"id": rawID})
	}

	// expectedVersion is required by spec; mcp-go's Required() does not
	// enforce the int-typed presence on numeric fields (it accepts the
	// schema definition but a missing arg defaults to 0). We still
	// validate explicitly.
	expectedVersion := a.GetInt("expectedVersion", 0)
	if expectedVersion < 1 {
		return UpdateResult{}, Fail(ErrCodeValidationError,
			"expectedVersion is required and must be >= 1", nil)
	}

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// per issue on its project (forceUnlockGate), not by the role.

	// sk-303.14 T007 (contract C1): the review flag does not travel the
	// ordinary update path. A mixed request is refused by FORM, before
	// any write — on MCP the form code is validation_error, the analogue
	// of REST 400. Parsing is reviewFlagArg.
	reviewFlag, reviewErr := ReviewFlagArg(a.GetArguments())
	if reviewErr != nil {
		return UpdateResult{}, reviewErr
	}

	// ---- Parsing and checking the SHAPE — entirely before the store (sk-516) ----
	//
	// The order is aligned with close.go#handleClose. Previously this block stood
	// AFTER the visibility check, and on the very same input — a wrong
	// argument plus a nonexistent (or invisible) issue — the two
	// surfaces answered differently: skein_close about the argument,
	// skein_update about the issue. No requirement chose between
	// them, the divergence arose by itself.
	//
	// The caller paid for it: an agent that got issue_not_found where
	// its own argument is wrong goes looking for the issue instead of
	// fixing the call. The diagnostic value of the code
	// is lost exactly where it is needed.
	//
	// So EVERYTHING that needs no database stands here: componentIds,
	// labels, status, the outcome with its target, `reason` and the field edit. The mutual
	// order inside the block is kept as before — what moved was the place of the block
	// as a whole, not the priorities of the formal refusals among themselves.
	//
	// The statement about the order is update_close_outcome_test.go#
	// TestUpdateMCP_FormalRefusalPrecedesTheStore, the reference —
	// close_outcome_mcp_test.go#TestCloseMCP_FormalRefusalPrecedesTheStore.

	// ---- Pre-validate componentIds (UUID parse) ----
	// This was already done up front; we keep the same pattern for
	// labels below so validation errors never leave partial state.
	var componentIDs []uuid.UUID
	_, hasComponentIDs := a.GetArguments()["componentIds"]
	if hasComponentIDs {
		rawComponentIDs := a.GetStringSlice("componentIds", nil)
		componentIDs = make([]uuid.UUID, 0, len(rawComponentIDs))
		for _, raw := range rawComponentIDs {
			cid, perr := uuid.Parse(strings.TrimSpace(raw))
			if perr != nil {
				return UpdateResult{}, Fail(ErrCodeValidationError,
					"componentIds[i] is not a UUID",
					map[string]any{"componentId": raw})
			}
			componentIDs = append(componentIDs, cid)
		}
	}

	// ---- Pre-validate labelsAdd / labelsRemove ----
	// Validation must happen BEFORE any sub-op runs; otherwise a typo
	// in label N would commit labels 0..N-1 then fail with
	// validation_error, leaving partial state. Aligns with the
	// componentIds pattern above.
	labelsAddRaw := a.GetStringSlice("labelsAdd", nil)
	labelsAdd := make([]string, 0, len(labelsAddRaw))
	for _, l := range labelsAddRaw {
		canon, vErr := issues.ValidateLabel(l)
		if vErr != nil {
			return UpdateResult{}, ValidationFailure(vErr)
		}
		labelsAdd = append(labelsAdd, canon)
	}
	labelsRemoveRaw := a.GetStringSlice("labelsRemove", nil)
	labelsRemove := make([]string, 0, len(labelsRemoveRaw))
	for _, l := range labelsRemoveRaw {
		canon, vErr := issues.ValidateLabel(l)
		if vErr != nil {
			return UpdateResult{}, ValidationFailure(vErr)
		}
		labelsRemove = append(labelsRemove, canon)
	}

	// ---- Pre-validate status (if supplied) ----
	// Same rationale: catch the validation_error up front.
	statusRaw := strings.TrimSpace(a.GetString("status", ""))
	var statusCanon string
	if statusRaw != "" {
		canon, vErr := issues.ValidateStatus(statusRaw)
		if vErr != nil {
			return UpdateResult{}, ValidationFailure(vErr)
		}
		statusCanon = canon
	}

	// sk-489 — the FIRST input builder of this surface. handleUpdate
	// assembles updateOneInput itself and does NOT go through buildUpdateOneInput,
	// so the parsing and checks stand both here and there.
	// The sk-489 requirement on the place holds after the block was moved: the check
	// must stand before the first step of the chain, because step 1 is a field
	// edit and commits on its own, and a refusal after it would mean
	// "priority changed, outcome not". Now it stands even earlier — before
	// the store altogether — and that only strengthens the first requirement.
	outcome, outcomeErr := ParseCloseOutcomeArgs(a.GetArguments(), id, "")
	if outcomeErr != nil {
		return UpdateResult{}, outcomeErr
	}
	if rej := RefuseOutcomeOnNonClosingStatus(outcome, statusCanon, id); rej != nil {
		return UpdateResult{}, rej
	}

	// `reason` and the field edit are the last formal checks. They too
	// stand before the store: a refusal over a bad title is about the title, not
	// about the issue, which may not even exist.
	reasonArg := a.GetString("reason", "")
	if vErr := issues.ValidateProse("reason", reasonArg); vErr != nil {
		return UpdateResult{}, ValidationFailure(vErr)
	}
	patchInput, hasFieldPatch, vErr := BuildPatchInputFromArgs(a, expectedVersion)
	if vErr != nil {
		return UpdateResult{}, vErr
	}

	// ---- Visibility: load issue once, ensure project visible ----
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return UpdateResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, "issue operation error",
			"op", "update",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return UpdateResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	// sk-674.18 T066: editing is ActionWrite of US-6; changing the
	// assignee is ActionAssign too.
	updateActions := []authz.Action{authz.ActionWrite}
	if _, ok := a.GetArguments()["assignee"]; ok {
		updateActions = append(updateActions, authz.ActionAssign)
	}
	if f := IssueActionGate(ctx, d.Pool, "skein_update", "id", id, issue.ProjectID, userID, updateActions...); f != nil {
		return UpdateResult{}, f
	}
	if force {
		if f := ForceUnlockGate(ctx, d.Pool, "skein_update", "id", id, issue.ProjectID, userID); f != nil {
			return UpdateResult{}, f
		}
	}

	// ---- R2.3: lock predicate ----
	// Authoritative lock check lives in the store: every sub-op
	// invokes lockGuard inside its own SERIALIZABLE tx and surfaces
	// ErrLockedByOther{LockedBy: <current>} with the live lock-holder
	// at error time. We do NOT pre-check from the GetIssue snapshot
	// — that read is outside any tx and could return a stale
	// lockedBy that disagrees with what the store rejects against.
	// force=true passed forceUnlockGate above (K-28).
	sessionID := auth.SessionIDFrom(ctx)

	actor := IssuesAgentActor(ctx, d.Pool, userID)

	// The flag takes its own path, outside the general update: without a version bump,
	// without a change stamp, without a fan-out (INV-2). The branch stands
	// AFTER the visibility checks and BEFORE the general edit starts: a mixed
	// request no longer gets here (reviewFlagArg rejected it before
	// the store), and a non-mixed one must not perform an edit that
	// does not exist. The shape parsing of the general edit since sk-516 stands upstream
	// and is harmless for a non-mixed call: without changing arguments it
	// collects nothing and complains about nothing.
	if reviewFlag != nil {
		final, err := d.Issues.SetReviewRequired(ctx, issues.ReviewFlagInput{
			ID:              id,
			ExpectedVersion: expectedVersion,
			Required:        *reviewFlag,
			Actor:           actor,
		})
		if err != nil {
			return UpdateResult{}, MapReviewFlagError(ctx, err, userID, id)
		}
		slog.InfoContext(ctx, "issue operation ok",
			"op", "update",
			"userId", userID.String(), "issueId", id,
			"field", "review_required",
			"version", final.Version,
			"latencyMs", time.Since(start).Milliseconds())
		return UpdateResult{IssueID: id, Final: final, ReviewFlag: true}, nil
	}

	in := UpdateOneInput{
		ID:              id,
		ExpectedVersion: expectedVersion,
		HasFieldPatch:   false,
		StatusCanon:     statusCanon,
		Reason:          reasonArg,
		HasComponentIDs: hasComponentIDs,
		ComponentIDs:    componentIDs,
		LabelsAdd:       labelsAdd,
		LabelsRemove:    labelsRemove,
		CloseOutcome:    outcome.Outcome,
		CloseTarget:     outcome.Target,
		// The system role — the same visibility key as for a manual link.
		// Passed unconditionally: "only when needed" is one
		// refactoring away from a store failure.
		DependenciesCaller: &issues.DependenciesCaller{UserID: userID, Role: role},
		Force:              force,
		SessionID:          sessionID,
		Actor:              actor,
	}
	in.PatchInput = patchInput
	in.HasFieldPatch = hasFieldPatch

	final, link, err := RunUpdateForOne(ctx, d.Issues, in)
	if err != nil {
		if rb, committed := asReadBackFailed(err); committed {
			return UpdateResult{IssueID: id, Link: link, ReadBack: rb}, nil
		}
		return UpdateResult{}, MapUpdateError(ctx, err, userID, id, outcome.Target)
	}
	slog.InfoContext(ctx, "issue operation ok",
		"op", "update",
		"userId", userID.String(), "issueId", id,
		"version", final.Version,
		"latencyMs", time.Since(start).Milliseconds())
	return UpdateResult{IssueID: id, Final: final, Link: link}, nil
}

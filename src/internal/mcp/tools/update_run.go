package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// errLabelNotPresent is the typed error returned by runUpdateForOne
// when a labelsRemove[] entry refers to a label that isn't currently
// set on the issue. The caller (mapUpdateError / classifyUpdateError)
// branches on this to emit a validation_error envelope with the
// {label: <name>} structured detail — matching the R2.2 single-tool
// behaviour exactly so the refactor is bit-for-bit transparent.
type errLabelNotPresent struct {
	Label string
}

func (e *errLabelNotPresent) Error() string {
	return fmt.Sprintf("label is not currently set on this issue: %s", e.Label)
}

// updateOneInput is the validated, ready-to-execute shape one
// skein_update / skein_update_many row passes into runUpdateForOne.
// All fields have already been canonicalised (status enum normalised,
// labels canonicalised, componentIds parsed, etc.) — this struct is
// the contract between the handler-level parser and the chain
// executor.
//
// The shape is the same whether the row came from skein_update (one
// per call) or skein_update_many (N per call); both feed the chain
// executor identical inputs so behaviour stays bit-for-bit consistent.
type updateOneInput struct {
	ID              string
	ExpectedVersion int

	// HasFieldPatch is true when at least one of the patch fields
	// (title / description / type / priority / assignee / due dates
	// / design / acceptance / notes / external ref / source context
	// / estimated minutes) was supplied. When false the caller skips
	// PatchIssue entirely — issuing a no-op patch would still bump
	// version + emit an audit row, surprising the "I only wanted to
	// change status" path.
	HasFieldPatch bool
	PatchInput    issues.PatchIssueInput

	// StatusCanon is the canonicalised status string ("" when not set).
	// "closed" routes through Close; everything else through
	// UpdateStatus.
	StatusCanon string
	Reason      string // free-form reason, applied to status transition + close

	// HasComponentIDs distinguishes "leave alone" from "explicit
	// empty array means clear all" — only fan out when the caller
	// supplied the key.
	HasComponentIDs bool
	ComponentIDs    []uuid.UUID

	LabelsAdd    []string
	LabelsRemove []string

	// CloseOutcome / CloseTarget — the close outcome and the link target (sk-489).
	// They live HERE, not directly in PatchInput / CloseInput, because where
	// they go is decided by the status, and that decision is made in ONE place —
	// runUpdateForOne below. The input builders only parse and
	// validate them; two builders making different routing decisions
	// are exactly the class of bug that makes this pair separate at all.
	CloseOutcome *string
	CloseTarget  string
	// DependenciesCaller — the system role for the target visibility check, not
	// Actor.Role: the key is the same as for a manual link (skein_dep_add).
	// Required by the store when CloseTarget is non-empty.
	//
	// Filled by BOTH builders and UNCONDITIONALLY — the single
	// update.go#handleUpdate sets it itself, the batch one takes it as a ready
	// parameter `depsCaller` (handleUpdateMany already has the role at hand).
	// "Only when needed" does not work here: the batch rejects the target today,
	// and needs no carrier — but that is a decision of the NEIGHBOURING place,
	// not of this one, and the day the target is accepted would be the day the
	// store refuses with `DependenciesCaller is required when CloseTarget is set`
	// through a bare fmt.Errorf, which update_many.go#classifyUpdateError
	// would map to internal_error through the fall-through.
	DependenciesCaller *issues.DependenciesCaller

	Force     bool
	SessionID string
	Actor     issues.Actor
}

// runUpdateForOne executes the full skein_update chain for a single
// row: PatchIssue → UpdateStatus/Close → ReplaceComponents → AddLabel
// (loop) → RemoveLabel (loop). Each store call is its own
// SERIALIZABLE transaction (the underlying methods don't expose a
// shared-tx surface). Mid-chain failure is documented in the tool
// description: earlier sub-operations are NOT rolled back.
//
// Returns the post-state IssueDetail on success, or the typed error
// from whichever sub-step failed first. Callers map that error to
// the MCP envelope via mapUpdateError (single tool) or
// classifyUpdateError (batch).
//
// The second result is the close-target link state (sk-489, US-2):
// "created" / "existed" when this call named a target, empty otherwise.
// It is a RETURN value and not a field of the post-state for the reason
// ack.go#ackFromClosedIssue gives — it describes the call, not the row,
// and two identical calls legitimately answer differently.
//
// # The one place the outcome's route is decided
//
// This surface reaches THREE writers, and which one gets the outcome is
// decided here and nowhere else:
//
//	status=closed        → Store.CloseCommitted      (close step)
//	no status at all     → Store.PatchIssueCommitted (field patch step)
//	status=anything else → neither: the builders refused the call already
//
// The third line is not an omission. An outcome with a transition to a
// non-closed status is refused by BOTH input builders before the chain
// starts, because by the time UpdateStatus could refuse it the field
// patch of step 1 would already have committed — and "nothing
// changed" is what the refusal promises.
//
// The batch path wants the version and nothing else, so it calls
// runUpdateChainVersion instead and skips the final hydration entirely.
func runUpdateForOne(
	ctx context.Context,
	store *issues.Store,
	in updateOneInput,
) (*issues.IssueDetail, issues.CloseLinkState, error) {
	link, last, err := runUpdateChain(ctx, store, in)
	if err != nil {
		return nil, "", err
	}
	// Final post-state load. sk-607: every requested step has committed by
	// now, so the read runs detached from the caller's context, and if even
	// that read fails the chain is acknowledged from the state its last
	// transaction read (*ErrReadBackFailed) — never reported as a failed
	// write. A chain that wrote nothing has nothing to acknowledge, and its
	// read failure stays an error.
	rctx, cancel := issues.ReadBackContext(ctx)
	defer cancel()
	final, err := store.GetIssue(rctx, in.ID)
	if err != nil {
		if last.ID == "" {
			return nil, "", err
		}
		return nil, link, &issues.ErrReadBackFailed{
			Op:    "update",
			State: last,
			Cause: fmt.Errorf("reading back %s: %w", in.ID, err),
		}
	}
	return final, link, nil
}

// runUpdateChainVersion runs the same chain and answers the version its
// last transaction committed — no read after the commit at all (sk-607).
// The batch row it feeds carries {id, ok, version} and no prose; the read
// that used to follow the chain was the one the batch's own deadline could
// cut after every step had committed. Only a chain that wrote nothing
// reads the live version.
//
// The link-to-target state (sk-489) is not needed by the batch: a batch row is exactly
// {id, ok, version}, and the batch row rejects the close target already at
// parse time (buildUpdateOneInput, refusal close_target_not_supported). So
// it is dropped here.
func runUpdateChainVersion(
	ctx context.Context,
	store *issues.Store,
	in updateOneInput,
) (int, error) {
	_, last, err := runUpdateChain(ctx, store, in)
	if err != nil {
		return 0, err
	}
	if last.ID != "" {
		return last.Version, nil
	}
	rctx, cancel := issues.ReadBackContext(ctx)
	defer cancel()
	return store.GetIssueVersion(rctx, in.ID)
}

// afterClosePrecheckHook is a test seam: when non-nil it runs right after
// the sk-540.1 pre-check of runUpdateChain passed and before the first
// write. It lets a test reopen the issue inside the window between the
// pre-check and the close, which is otherwise a real but non-deterministic
// race. Always nil in production; only tests assign it.
var afterClosePrecheckHook func(ctx context.Context, issueID string)

// runUpdateChain performs every mutation of the chain and loads nothing
// back. Both entry points above add the read they actually need. The
// first result is the close-target link state, as documented on
// runUpdateForOne. The second is the post-state the LAST step's own
// transaction read (sk-607) — zero when no step wrote — so a chain whose
// every step committed can be acknowledged without any read after the
// commit. A step that fails returns its error and nothing else: earlier
// steps stay committed (the documented partial-chain semantics), and a
// chain that did not finish is never passed off as one that did.
func runUpdateChain(
	ctx context.Context,
	store *issues.Store,
	in updateOneInput,
) (issues.CloseLinkState, issues.CommittedState, error) {
	lockOpts := issues.LockOpts{Force: in.Force, SessionID: in.SessionID}
	var link issues.CloseLinkState
	var last issues.CommittedState

	// currentVersion threads through the chain — PatchIssue bumps it,
	// UpdateStatus consumes the bumped value as its expectedVersion.
	currentVersion := in.ExpectedVersion

	// sk-540.1 — pre-check BEFORE the first write. A transition into closed
	// without closeOutcome is refused while nothing has been written: the
	// field patch of step 1 commits in its own transaction, so a refusal
	// raised only by the close step would leave the fields written. A
	// re-close of an already closed issue passes and keeps its outcome.
	// CloseCommitted repeats the check in its transaction, which is what
	// catches an issue reopened in between under force=true (no version
	// predicate stops that path).
	//
	// sk-540.2 — the same pre-check refuses duplicate/superseded without
	// closeTarget unless the issue is already closed and keeps its outcome
	// or already carries the matching edge.
	if in.StatusCanon == issues.StatusClosed {
		if err := store.PrecheckClose(ctx, in.ID, in.CloseOutcome, in.CloseTarget); err != nil {
			return "", issues.CommittedState{}, err
		}
		if afterClosePrecheckHook != nil {
			afterClosePrecheckHook(ctx, in.ID)
		}
	}

	// sk-489 — the route, decided once (see the doc comment).
	touchesOutcome := in.CloseOutcome != nil || strings.TrimSpace(in.CloseTarget) != ""
	patchCarriesOutcome := touchesOutcome && in.StatusCanon == ""

	// 1) Field patch.
	//
	// patchCarriesOutcome widens the gate: editing ONLY the outcome of a
	// closed issue supplies no ordinary patch field, so HasFieldPatch is
	// false and the step would be skipped — and the outcome silently never
	// written, with the call answering ok:true.
	if in.HasFieldPatch || patchCarriesOutcome {
		patch := in.PatchInput
		patch.ExpectedVersion = currentVersion
		patch.Force = in.Force
		patch.SessionID = in.SessionID
		if patchCarriesOutcome {
			patch.CloseOutcome = in.CloseOutcome
			patch.CloseTarget = in.CloseTarget
			patch.DependenciesCaller = in.DependenciesCaller
		}
		// PatchIssueCommitted: the chain needs the version, which comes out
		// of the patch's own transaction (sk-607) — no card read here that
		// could fail after the patch committed and stop the chain.
		state, l, err := store.PatchIssueCommitted(ctx, in.ID, patch, in.Actor)
		if err != nil {
			return "", issues.CommittedState{}, err
		}
		if patchCarriesOutcome {
			link = l
		}
		last = state
		currentVersion = state.Version
	}

	// 2) Status transition (closed routes to Close).
	if in.StatusCanon != "" {
		var reasonArg *string
		if r := strings.TrimSpace(in.Reason); r != "" {
			reasonArg = &r
		}
		if in.StatusCanon == issues.StatusClosed {
			reasonStr := ""
			if reasonArg != nil {
				reasonStr = *reasonArg
			}
			// CloseCommitted: the chain needs the version and the link,
			// and both come out of the close's own transaction (sk-607).
			// The card is loaded once, by the entry point, at the end.
			state, l, err := store.CloseCommitted(ctx, issues.CloseInput{
				ID:                 in.ID,
				ExpectedVersion:    currentVersion,
				Reason:             reasonStr,
				Force:              in.Force,
				SessionID:          in.SessionID,
				Actor:              in.Actor,
				CloseOutcome:       in.CloseOutcome,
				CloseTarget:        in.CloseTarget,
				DependenciesCaller: in.DependenciesCaller,
				// sk-540.1: re-checked here against the live status — see
				// the pre-check at the top of runUpdateChain.
				RequireOutcomeOnTransition: true,
			})
			if err != nil {
				return "", issues.CommittedState{}, err
			}
			link = l
			last = state
			currentVersion = state.Version
		} else {
			state, err := store.UpdateStatusCommitted(ctx, in.ID, issues.UpdateStatusInput{
				ExpectedVersion: currentVersion,
				Status:          in.StatusCanon,
				Reason:          reasonArg,
				Force:           in.Force,
				SessionID:       in.SessionID,
			}, in.Actor)
			if err != nil {
				return "", issues.CommittedState{}, err
			}
			last = state
			currentVersion = state.Version
		}
	}

	// 3) Component replace. NoLoad: no card is built — the post-state the
	// transaction read is kept instead, because a component write moves the
	// version the acknowledgement must carry.
	if in.HasComponentIDs {
		state, err := store.ReplaceComponentsNoLoad(ctx, in.ID, in.ComponentIDs, in.Actor, lockOpts)
		if err != nil {
			return "", issues.CommittedState{}, err
		}
		last = state
	}

	// 4) Labels add.
	for _, label := range in.LabelsAdd {
		state, err := store.AddLabelNoLoad(ctx, in.ID, label, in.Actor, lockOpts)
		if err != nil {
			return "", issues.CommittedState{}, err
		}
		last = state
	}

	// 5) Labels remove. Maps "label not on issue" (ErrNotFound) to a
	// validation error so the agent gets a discriminator distinct from
	// "issue not found".
	for _, label := range in.LabelsRemove {
		state, err := store.RemoveLabelNoLoad(ctx, in.ID, label, in.Actor, lockOpts)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				// RemoveLabel returns ErrNotFound for both "issue gone"
				// and "label not present". The chain only reaches here
				// AFTER the field/status/components/labelsAdd steps
				// succeeded, so the issue clearly exists — the cause
				// must be the latter. Re-package as a typed error the
				// caller maps to validation_error with {label: <name>}.
				return "", issues.CommittedState{}, &errLabelNotPresent{Label: label}
			}
			return "", issues.CommittedState{}, err
		}
		last = state
	}

	return link, last, nil
}

// refuseOutcomeOnNonClosingStatus is the check BOTH input builders of this
// surface run before the chain starts (sk-489, US-1).
//
// An outcome (or a target) travelling with a transition to a status other
// than `closed` is impossible in any state, so it is a form refusal. It has
// to happen HERE rather than being left to issues.Store.UpdateStatus, which
// raises the same code: by the time the chain reaches the status step, step
// 1 has already committed the field patch, and this refusal promises that
// nothing was changed.
//
// The third writer this surface can reach — Store.UpdateStatus, for any
// status that is not `closed` — therefore never sees an outcome at all.
// That is asserted rather than inferred (matrix row 34).
func refuseOutcomeOnNonClosingStatus(outcome closeOutcomeArgs, statusCanon, id string) *mcpgo.CallToolResult {
	if !outcome.HasAny() || statusCanon == "" || statusCanon == issues.StatusClosed {
		return nil
	}
	return skmcp.ToolError(ErrCodeValidationError,
		"closeOutcome and closeTarget are only accepted when the new status is "+
			issues.StatusClosed+", or on an already closed issue with no status at all",
		map[string]any{"code": "close_outcome_not_a_close", "id": id})
}

// buildUpdateOneInput parses one skein_update / skein_update_many input
// payload (already a map[string]any) into an updateOneInput. Returns
// either a fully-validated input or a CallToolResult error envelope
// the caller emits as-is. The single-tool path (skein_update) gets
// the CallToolResult; the batch path classifies and re-packages it as
// a per-row error.
//
// `args` is the raw arguments map (req.GetArguments() for skein_update,
// items[i] for skein_update_many).
//
// `depsCaller` is the close-target visibility key, carried in unconditionally
// for the reason its field's doc comment gives.
func buildUpdateOneInput(
	args map[string]any,
	pool *pgxpool.Pool,
	ctx context.Context,
	id string,
	expectedVersion int,
	force bool,
	sessionID string,
	actor issues.Actor,
	depsCaller *issues.DependenciesCaller,
) (updateOneInput, *mcpgo.CallToolResult) {
	out := updateOneInput{
		ID:              id,
		ExpectedVersion: expectedVersion,
		Force:           force,
		SessionID:       sessionID,
		Actor:           actor,
		// Unconditionally — see the field doc block of updateOneInput.
		DependenciesCaller: depsCaller,
	}

	// sk-303.14 T007 (contract C1): the review flag does not travel this
	// assembly and cannot — it lives outside the ordinary patch path. A
	// row carrying it is refused WHOLE and BEFORE any write; the
	// rationale is in rejectReviewFlagInBatch. The check is first on
	// purpose: a row that mixed the flag with ordinary fields must not
	// get to apply those fields before it is found that the flag cannot
	// be applied.
	if rej := rejectReviewFlagInBatch(args); rej != nil {
		return out, rej
	}

	// componentIds — UUID parse, present-or-absent semantics.
	if rawCIDs, ok := args["componentIds"]; ok && rawCIDs != nil {
		out.HasComponentIDs = true
		if slice, isSlice := rawCIDs.([]any); isSlice {
			out.ComponentIDs = make([]uuid.UUID, 0, len(slice))
			for _, raw := range slice {
				s, _ := raw.(string)
				cid, perr := uuid.Parse(strings.TrimSpace(s))
				if perr != nil {
					return out, skmcp.ToolError(ErrCodeValidationError,
						"componentIds[i] is not a UUID",
						map[string]any{"componentId": s})
				}
				out.ComponentIDs = append(out.ComponentIDs, cid)
			}
		}
	}

	// labelsAdd / labelsRemove — canonicalise up front so a typo in
	// label N doesn't commit labels 0..N-1.
	if rawAdd, ok := args["labelsAdd"]; ok && rawAdd != nil {
		if slice, isSlice := rawAdd.([]any); isSlice {
			out.LabelsAdd = make([]string, 0, len(slice))
			for _, raw := range slice {
				s, _ := raw.(string)
				canon, vErr := issues.ValidateLabel(s)
				if vErr != nil {
					return out, validationToolError(vErr)
				}
				out.LabelsAdd = append(out.LabelsAdd, canon)
			}
		}
	}
	if rawRem, ok := args["labelsRemove"]; ok && rawRem != nil {
		if slice, isSlice := rawRem.([]any); isSlice {
			out.LabelsRemove = make([]string, 0, len(slice))
			for _, raw := range slice {
				s, _ := raw.(string)
				canon, vErr := issues.ValidateLabel(s)
				if vErr != nil {
					return out, validationToolError(vErr)
				}
				out.LabelsRemove = append(out.LabelsRemove, canon)
			}
		}
	}

	// status — pre-validate.
	if rawStatus, ok := args["status"]; ok && rawStatus != nil {
		if s, isStr := rawStatus.(string); isStr {
			trimmed := strings.TrimSpace(s)
			if trimmed != "" {
				canon, vErr := issues.ValidateStatus(trimmed)
				if vErr != nil {
					return out, validationToolError(vErr)
				}
				out.StatusCanon = canon
			}
		}
	}

	// reason — captured raw, applied during status transition / close.
	if r, ok := args["reason"]; ok && r != nil {
		if s, isStr := r.(string); isStr {
			out.Reason = s
		}
	}

	// sk-489 — the SECOND input builder of this surface. Only
	// handleUpdateMany calls it; the checks of the single update.go#handleUpdate
	// do not cover the batch row, and the half of the claim proved there
	// stays green in their absence here.
	//
	// A batch update does NOT accept the target at all — a row refusal.
	// The reason is the same as why the skein_update_many response row
	// stayed {id, ok, version}: there is nowhere to put the "created /
	// existed" link flag, and by accepting the target the surface would create
	// an edge it reports nothing about. A boundary of the surface, not an omission:
	// hence it has neither "edge" cells nor target-refusal cells.
	if raw, present := args["closeTarget"]; present && raw != nil {
		if s, isStr := raw.(string); !isStr || strings.TrimSpace(s) != "" {
			return out, skmcp.ToolError(ErrCodeValidationError,
				"closeTarget is not accepted by skein_update_many — the per-row answer "+
					"has nowhere to report whether the link was created or already "+
					"existed. Use skein_update or skein_close for a row that needs a "+
					"close target",
				map[string]any{"code": "close_target_not_supported", "id": id})
		}
	}
	outcome, outcomeErr := parseCloseOutcomeArgs(args, id, "items[i].")
	if outcomeErr != nil {
		return out, skmcp.ToolError(outcomeErr.Code, outcomeErr.Message, outcomeErr.Details)
	}
	if rej := refuseOutcomeOnNonClosingStatus(outcome, out.StatusCanon, id); rej != nil {
		return out, rej
	}
	out.CloseOutcome = outcome.Outcome
	out.CloseTarget = outcome.Target

	// Field patch — delegate to buildPatchInputFromArgs (the legacy
	// buildPatchInput is request-shaped; this variant works on the
	// raw args map so it's reusable for batch).
	patchInput, hasFieldPatch, vErr := buildPatchInputFromArgs(args, expectedVersion)
	if vErr != nil {
		return out, vErr
	}
	out.PatchInput = patchInput
	out.HasFieldPatch = hasFieldPatch

	// Suppress pool/ctx unused warnings — they are reserved for future
	// patch validations that may need to consult the DB (e.g. assignee
	// resolution). Keep the signature stable so the batch caller does
	// not have to re-thread these later.
	_ = pool
	_ = ctx
	return out, nil
}

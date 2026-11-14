package issues

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// updateIssueMaxAttempts is the retry budget of an UpdateIssue that runs
// more than one step: the largest budget of the steps it may contain
// (patch, status and close are 10).
const updateIssueMaxAttempts = 10

// errLabelAbsent is what removeLabelInTx answers for a label that is not on
// the issue. It wraps ErrNotFound, so the single-step writers keep their
// contract (RemoveLabel → 404); UpdateIssue, which has already read the
// issue in the same transaction, tells the two apart and answers
// *LabelNotPresentError.
var errLabelAbsent = fmt.Errorf("label is not set on the issue: %w", ErrNotFound)

// LabelNotPresentError reports a LabelsRemove entry that is not set on the
// issue. The whole UpdateIssue is rolled back.
type LabelNotPresentError struct {
	Label string
}

func (e *LabelNotPresentError) Error() string {
	return fmt.Sprintf("label is not currently set on this issue: %s", e.Label)
}

// UpdateIssueInput is one change of one issue (T015, US-14). Every step that
// is set runs, in this order, inside ONE serializable transaction:
//
//	field patch → status transition or close (commit refs included) →
//	component replace → labels added → labels removed
//
// A refusal or failure at any step rolls the whole change back: nothing is
// written, the version does not move, no event, outbox row or signal is
// left behind. Events and signals of a successful change go out only after
// the commit (signals.BeginTxFunc).
//
// ExpectedVersion, Force, SessionID and Actor are the change's own; they
// override the same fields inside Patch, Status and Close. The caller's
// version is checked for every change (INV-7, US-6): by the first of
// patch / status / close that is set, or — for a change made only of
// components and labels — by its first step, on the row that step reads,
// after the lock check. The steps after the check run on the
// version the transaction itself moved. Force skips the check, as on every
// writer. The single-step component and label writers of /ui/v1
// (ReplaceComponentsNoLoad, AddLabelNoLoad, RemoveLabelNoLoad) keep their
// own contract and check no version.
type UpdateIssueInput struct {
	ID              string
	ExpectedVersion int
	Force           bool
	SessionID       string
	Actor           Actor

	Patch *PatchIssueInput
	// Status is a transition through UpdateStatus semantics; Close is a
	// close through Close semantics (outcome, target, commit refs). At most
	// one of the two.
	Status *UpdateStatusInput
	Close  *CloseInput

	// ReplaceComponents says Components is the new component set (an empty
	// set clears it); false leaves the components alone.
	ReplaceComponents bool
	Components        []uuid.UUID

	LabelsAdd    []string
	LabelsRemove []string

	// BeforeCommit, when set, runs inside the transaction after every step,
	// with the post-state the transaction is about to commit. An error rolls
	// the whole change back. It is the attachment point for a record that
	// must be atomic with the change itself (the Idempotency result of the
	// public API). It runs again on every retried attempt.
	BeforeCommit func(ctx context.Context, tx pgx.Tx, state CommittedState) error
}

// UpdateIssueResult is the post-state the committing transaction read and
// the close-target link state of the close or of an outcome edit.
type UpdateIssueResult struct {
	State CommittedState
	Link  CloseLinkState
}

// updateIssueOp names a change for the retry log, the serialization metric
// and the post-commit seam (AfterCommitHook).
type updateIssueOp struct {
	name     string
	attempts int
	commit   string
}

// UpdateIssue applies one change of one issue atomically (T015, US-14).
// Every entry that changes an issue — skein_update and its batch,
// skein_close and its batch, the REST patch, transition and bulk — reaches
// this transaction: PatchIssueCommitted, UpdateStatusCommitted and
// CloseCommitted are single-step calls of it.
//
// A transaction abort with the caller's version unchanged answers
// ErrSerializationRetry, as every single-step writer did.
func (s *Store) UpdateIssue(ctx context.Context, in UpdateIssueInput) (UpdateIssueResult, error) {
	return s.runUpdateIssue(ctx, in, opForChange(in))
}

// opForChange keeps a single-step change under the name, retry budget and
// post-commit label of its writer; a change of several steps is "update".
func opForChange(in UpdateIssueInput) updateIssueOp {
	steps := 0
	var op updateIssueOp
	if in.Patch != nil {
		steps++
		op = updateIssueOp{name: "patchIssue", attempts: patchIssueMaxAttempts, commit: "patch"}
	}
	if in.Status != nil {
		steps++
		op = updateIssueOp{name: "updateStatus", attempts: updateStatusMaxAttempts, commit: "status"}
	}
	if in.Close != nil {
		steps++
		op = updateIssueOp{name: "close", attempts: closeMaxAttempts, commit: "close"}
	}
	if in.ReplaceComponents {
		steps++
	}
	steps += len(in.LabelsAdd) + len(in.LabelsRemove)
	if steps != 1 || (in.Patch == nil && in.Status == nil && in.Close == nil) {
		return updateIssueOp{name: "updateIssue", attempts: updateIssueMaxAttempts, commit: "update"}
	}
	return op
}

func (s *Store) runUpdateIssue(ctx context.Context, in UpdateIssueInput, op updateIssueOp) (UpdateIssueResult, error) {
	if in.Status != nil && in.Close != nil {
		return UpdateIssueResult{}, fmt.Errorf("update issue %s: Status and Close are mutually exclusive", in.ID)
	}
	versioned := in.Patch != nil || in.Status != nil || in.Close != nil
	if !versioned && !in.ReplaceComponents && len(in.LabelsAdd) == 0 && len(in.LabelsRemove) == 0 {
		return UpdateIssueResult{}, fmt.Errorf("update issue %s: no step to apply", in.ID)
	}
	// A change of components and labels only: its version check rides on
	// the first step (C5-01; INV-7, US-6).
	checkFirst := !versioned && !in.Force && in.ExpectedVersion >= 1

	// Every check that needs no transaction runs before it opens, for every
	// step: a bad label at the end of the change refuses the change before
	// the field patch at its start is attempted.
	var patch PatchIssueInput
	if in.Patch != nil {
		patch = *in.Patch
		patch.ExpectedVersion, patch.Force, patch.SessionID = in.ExpectedVersion, in.Force, in.SessionID
		if err := validatePatchStep(patch); err != nil {
			return UpdateIssueResult{}, err
		}
	}
	var status UpdateStatusInput
	var newStatus string
	if in.Status != nil {
		status = *in.Status
		status.ExpectedVersion, status.Force, status.SessionID = in.ExpectedVersion, in.Force, in.SessionID
		var err error
		if newStatus, err = validateStatusStep(status); err != nil {
			return UpdateIssueResult{}, err
		}
	}
	var closing CloseInput
	if in.Close != nil {
		closing = *in.Close
		closing.ID, closing.ExpectedVersion, closing.Force, closing.SessionID, closing.Actor =
			in.ID, in.ExpectedVersion, in.Force, in.SessionID, in.Actor
		if err := validateCloseStep(closing); err != nil {
			return UpdateIssueResult{}, err
		}
	}
	for _, label := range append(append([]string(nil), in.LabelsAdd...), in.LabelsRemove...) {
		if _, err := ValidateLabel(label); err != nil {
			return UpdateIssueResult{}, err
		}
	}
	lockOpts := LockOpts{Force: in.Force, SessionID: in.SessionID}

	var res UpdateIssueResult
	err := s.withSerializableRetry(ctx, op.name, op.attempts, func(tx pgx.Tx) error {
		// A replayed attempt starts from scratch (sk-622.21, R17).
		res = UpdateIssueResult{}
		chained := false
		// A change of components and labels only: its first step checks
		// the caller's version on the row it reads anyway, after the lock
		// check; the steps after it run on the version it saw.
		pending := 0
		if checkFirst {
			pending = in.ExpectedVersion
		}
		expectOnce := func() int {
			v := pending
			pending = 0
			return v
		}
		if in.Patch != nil {
			var link CloseLinkState
			if err := s.patchIssueInTx(ctx, tx, in.ID, patch, in.Actor, chained, &link); err != nil {
				return err
			}
			res.Link = link
			chained = true
		}
		if in.Status != nil {
			if err := s.updateStatusInTx(ctx, tx, in.ID, newStatus, status, in.Actor, chained); err != nil {
				return err
			}
			chained = true
		}
		if in.Close != nil {
			var link CloseLinkState
			if err := s.closeInTx(ctx, tx, closing, chained, &link); err != nil {
				return err
			}
			res.Link = link
		}
		if in.ReplaceComponents {
			if err := s.replaceComponentsInTx(ctx, tx, in.ID, in.Components, in.Actor, lockOpts, expectOnce()); err != nil {
				return err
			}
		}
		for _, label := range in.LabelsAdd {
			if err := s.addLabelInTx(ctx, tx, in.ID, label, in.Actor, lockOpts, expectOnce()); err != nil {
				return err
			}
		}
		for _, label := range in.LabelsRemove {
			if err := s.removeLabelInTx(ctx, tx, in.ID, label, in.Actor, lockOpts, expectOnce()); err != nil {
				if errors.Is(err, errLabelAbsent) {
					return &LabelNotPresentError{Label: label}
				}
				return err
			}
		}
		// sk-607: the post-state, read last so it reflects every write of
		// this transaction.
		st, err := committedStatesInTx(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		res.State = st[0]
		if in.BeforeCommit != nil {
			if err := in.BeforeCommit(ctx, tx, res.State); err != nil {
				return err
			}
		}
		return runBeforeCommit(ctx, tx, res.State)
	})
	if err != nil {
		// A declared version lets a serialization abort be told from a real
		// conflict: unchanged live version → serialization_retry.
		var expected *int
		if in.ExpectedVersion >= 1 {
			expected = &in.ExpectedVersion
		}
		return UpdateIssueResult{}, s.degradeSerializationAbort(ctx, err, op.name, in.ID, expected)
	}
	afterCommit(ctx, op.commit, in.ID)
	return res, nil
}

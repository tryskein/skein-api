package issues

// review_ops.go — sk-303.14, task T007, contract C1.
//
// Two operations of the author's private bookkeeping: set the review mark
// and toggle the "review required" flag. Both live OUTSIDE the shared patch
// path (PatchIssue) — that is a decision with consequences, not a split
// across files.
//
// # Why not through the shared path
//
// A shared patch bumps the version and stamps updated_at. INV-2 requires
// the opposite: neither the mark nor a flag change may move any issue
// attribute others can observe, otherwise an assignee with the card open
// would see that "something happened" without seeing the fields themselves
// — and privacy would leak through a side channel while the direct read
// stays sealed.
//
// Running the flag through the shared path and then rolling the version
// back is not an option: the rollback writes a patch journal row, fans
// out, and makes the effect depend on which fields the request named.
// So a request that mixes the flag with an ordinary field is REJECTED by
// the form (400 on both transports), not half-executed. The refusal is
// checked by one class and cannot be implemented partially.
//
// # What the operations do and do not do
//
//	do:    UPDATE exactly their own columns; write a journal row of a
//	       private event type (review.marked / review.flag_changed) in
//	       the private ROW SHAPE — see WritePrivateReviewEvent: the
//	       entity is "user" (the author), project is empty, the issue
//	       lives in the payload. The shape IS the hiding mechanism:
//	       journal readers select on entity_type/project_id and never
//	       see the private row, so they do not need type filters —
//	       and there is nowhere to forget them.
//	don't: version + 1; updated_at = now(); fan-out
//	       (notifications.FanOut). Both types are listed in
//	       notifications.auditOnlyEventTypes — the silence is declared
//	       there, not forgotten, so the classifier does not raise an
//	       unmapped signal.
//
// # The permission predicate is authorship, and only authorship
//
// INV-4: only the issue author may change the flag or set the mark.
// Neither the assignee, nor a previous assignee, nor an admin — a role
// widens visibility, not authorship. That is why there is no IsAdmin
// branch here, unlike Accept: its absence is a requirement, and a test
// checks it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
	"skein-api/internal/signals"
)

// ErrReviewNotAuthor — the caller is not the issue author. One sentinel
// for both operations: INV-4 requires the refusal to an admin to be the
// same as to any non-author, and distinct codes for distinct non-authors
// would themselves be an oracle ("you are special, but still no").
var ErrReviewNotAuthor = errors.New("only the issue author may change the review flag or mark it reviewed")

// ErrReviewIssueNotClosed — a mark on an unclosed issue. There is nothing
// to mark: there is no result yet (US-1).
var ErrReviewIssueNotClosed = errors.New("cannot mark an unclosed issue reviewed — there is no result yet")

// ErrReviewFlagCleared — a mark on an issue whose flag is cleared.
//
// A refusal, not a silent success: a cleared flag is a declared "I am not
// interested", and a mark against it is meaningless. A silent no-effect
// would hide from the client that the action did not apply (US-1).
var ErrReviewFlagCleared = errors.New("cannot mark reviewed while the review flag is cleared")

// ReviewFlagInput is the input of the flag-toggle operation.
type ReviewFlagInput struct {
	ID string
	// ExpectedVersion is a read condition, not a write promise. The flag
	// does NOT move the version (INV-2), but the caller must still present
	// the version they read: without it there is no way to tell "I am
	// toggling what I see" from "I am toggling what someone rewrote under
	// me" (the card may have been reopened and closed, and SC-023 resets
	// the mark on that).
	ExpectedVersion int
	Required        bool
	Actor           Actor
}

// ReviewMarkInput is the input of the review-mark operation.
type ReviewMarkInput struct {
	ID string
	// ExpectedVersion is the same read condition as on the flag.
	ExpectedVersion int
	Actor           Actor
}

// SetReviewRequired toggles the "review required" flag.
//
// Returns the card (for the author it carries the three mechanic fields —
// see review_visibility.go). Idempotent on value: setting the same value
// does not write a second event.
//
// INV-12: clearing the flag is NOT a review mark — reviewed_at and
// reviewed_by are not touched in either direction. The issue leaves the
// unreviewed set because the flag was cleared, not because it was
// reviewed, and restoring the flag puts the issue back into the set with
// the previous mark state.
func (s *Store) SetReviewRequired(ctx context.Context, input ReviewFlagInput) (*IssueDetail, error) {
	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		row, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if row.CreatedBy != input.Actor.UserID || input.Actor.UserID == uuid.Nil {
			return ErrReviewNotAuthor
		}
		if input.ExpectedVersion > 0 && row.Version != input.ExpectedVersion {
			return &ErrVersionConflict{CurrentVersion: row.Version}
		}

		var current bool
		if err := tx.QueryRow(ctx,
			`SELECT review_required FROM issues WHERE id = $1`, input.ID).Scan(&current); err != nil {
			return fmt.Errorf("reading review flag: %w", err)
		}
		if current == input.Required {
			// Value is already that. No row, no event — a repeat is
			// not obliged to leave a trace the first application did
			// not leave.
			return nil
		}

		// Exactly one column. Neither version nor updated_at (INV-2).
		if _, err := tx.Exec(ctx, `
UPDATE issues SET review_required = $2 WHERE id = $1`, input.ID, input.Required); err != nil {
			return fmt.Errorf("updating review flag: %w", err)
		}

		// Journal (INV-10). Private type: listed in
		// notifications.auditOnlyEventTypes, so there is no fan-out here
		// — and that absence is declared, not forgotten. Row shape is
		// WritePrivateReviewEvent: entity "user", project empty, issue
		// in the payload; it is the shape, not filters at the readers,
		// that closes observation channels (INV-2).
		if _, err := WritePrivateReviewEvent(ctx, tx, notifications.PrivateReviewFlagChanged,
			row.CreatedBy, input.ID,
			input.Actor, "review_required",
			map[string]any{"reviewRequired": current},
			map[string]any{"reviewRequired": input.Required}); err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): "Review flag / review mark" — the
		// author only; review data is private to them.
		signalReview(tx, row.CreatedBy)
		return nil
	})
	if err != nil {
		if isSerializationFailure(err) {
			cv, _ := s.currentVersion(ctx, input.ID)
			return nil, &ErrVersionConflict{CurrentVersion: cv}
		}
		return nil, err
	}
	return s.GetIssue(ctx, input.ID)
}

// MarkReviewed sets the review mark: time and author.
//
// Refusals (all business refusals, 422 on the transport):
//   - ErrReviewNotAuthor      — the caller is not the author;
//   - ErrReviewIssueNotClosed — the issue is not closed;
//   - ErrReviewFlagCleared    — the flag is cleared.
//
// Check order is load-bearing: authorship goes FIRST. Otherwise a
// non-author asking to mark someone else's unclosed issue would learn its
// status and flag state from the refusal code — the refusal itself would
// become an observation channel, exactly the one INV-2 closes.
//
// Repeat (US-1): the mark is already set — silent success. No second
// event; the author and time of the first mark do not change. Neither the
// first mark nor a repeat moves version or updated_at.
func (s *Store) MarkReviewed(ctx context.Context, input ReviewMarkInput) (*IssueDetail, error) {
	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		row, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if row.CreatedBy != input.Actor.UserID || input.Actor.UserID == uuid.Nil {
			return ErrReviewNotAuthor
		}
		if input.ExpectedVersion > 0 && row.Version != input.ExpectedVersion {
			return &ErrVersionConflict{CurrentVersion: row.Version}
		}
		if row.Status != StatusClosed {
			return ErrReviewIssueNotClosed
		}

		var required bool
		var reviewedAt *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT review_required, reviewed_at FROM issues WHERE id = $1`,
			input.ID).Scan(&required, &reviewedAt); err != nil {
			return fmt.Errorf("reading review state: %w", err)
		}
		if !required {
			return ErrReviewFlagCleared
		}
		if reviewedAt != nil {
			// Already marked — silent success without a second event.
			return nil
		}

		now := time.Now().UTC()
		// The reviewed_at IS NULL condition on the UPDATE is not
		// decoration: two concurrent marks both reach here, and the
		// race loser MUST NOT overwrite the winner's author and time.
		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET reviewed_at = $2, reviewed_by = $3
 WHERE id = $1 AND reviewed_at IS NULL`,
			input.ID, now, input.Actor.UserID)
		if err != nil {
			return fmt.Errorf("marking issue reviewed: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Another writer won the race. The desired state
			// ("marked") is already true — do not write the event,
			// otherwise one mark would produce two journal rows.
			return nil
		}

		// The same private row shape as the flag.
		if _, err := WritePrivateReviewEvent(ctx, tx, notifications.PrivateReviewMarked,
			row.CreatedBy, input.ID,
			input.Actor, "reviewed_at",
			map[string]any{"reviewedAt": nil, "reviewedBy": nil},
			map[string]any{"reviewedAt": now, "reviewedBy": input.Actor.UserID}); err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): "Review flag / review mark" — the
		// author only; review data is private to them.
		signalReview(tx, row.CreatedBy)
		return nil
	})
	if err != nil {
		if isSerializationFailure(err) {
			cv, _ := s.currentVersion(ctx, input.ID)
			return nil, &ErrVersionConflict{CurrentVersion: cv}
		}
		return nil, err
	}
	return s.GetIssue(ctx, input.ID)
}

// ---------------------------------------------------------------------------
// Flag default on create (T009)
// ---------------------------------------------------------------------------

// resolveReviewRequiredOnCreate computes the "review required" flag value
// for a CREATED issue.
//
// Order:
//  1. an explicitly passed value (explicit != nil) — the agent create path;
//  2. the author's personal default from user_notification_prefs;
//  3. factory review.DefaultReviewRequired — when there is no prefs row.
//
// The third step is not a safety net. user_notification_prefs rows are
// created LAZILY, and every upsert that creates them names its columns
// explicitly, so the column DEFAULT is unreachable in practice: a person
// who has never saved notification prefs has no row at all. Without this
// step pgx.ErrNoRows would have to be read as "default not set", and the
// zero value of bool is false, i.e. "not required": the mechanic would
// silently not work for exactly those who have never touched it — everyone.
//
// Read INSIDE the create transaction on purpose: the value lands in the
// same row and the same commit; a separate read before the transaction
// would open a window in which the person changes the default between
// the read and the insert.
//
// sk-565 (P3, P13): the read itself now lives in the per-attempt creator
// projection (creator_projection.go), shared by every issue of one create
// call — one primary-key read per attempt instead of one per issue. This
// function no longer reads; the order above and the factory fallback are
// unchanged.
func resolveReviewRequiredOnCreate(
	ctx context.Context,
	tx pgx.Tx,
	creator *creatorProjection,
	explicit *bool,
) (bool, error) {
	if explicit != nil {
		return *explicit, nil
	}
	return creator.ReviewRequiredDefault(ctx, tx)
}

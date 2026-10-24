package issues

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EventIssueLockForceReleased — sk-674.18 T034 (US-16, K-28). Emitted when
// a caller with force_unlock RELEASES another holder's claim: the explicit
// "release the lock" action (Store.ReleaseLock) and every force path that
// releases the claim — unclaim, close, a transition to closed (bulk and the
// batch tools included). A force edit that leaves the claim in place
// (update, a transition to another status) bypasses it without this event
// (the lead's decision (b), 2026-10-14). It rides beside issue.unclaimed
// (written in the same transaction, so every subscriber that tracks claims
// sees the release as before) and names who released it.
//
// payload schema: old_data {lockedBy, lockedAt, lockedByUserId} — the claim
// that was released; new_data {by: <users.id of the releaser>}.
const EventIssueLockForceReleased = "issue.lock_force_released"

// ErrForceUnlockForbidden — the claim belongs to someone else and the caller
// may not release it (force_unlock of US-6: the project owner, the space
// owner, the instance admin). Surfaces as 422 / MCP force_unlock_forbidden.
var ErrForceUnlockForbidden = errors.New("caller may not release another holder's claim")

// ReleaseLockInput is the request shape for Store.ReleaseLock.
type ReleaseLockInput struct {
	ID string
	// CanForceUnlock is the caller's force_unlock on the issue's project,
	// resolved by the surface (authz.ActionForceUnlock). Without it only
	// the caller's own claim is released.
	CanForceUnlock bool
	Actor          Actor
}

// ReleaseLock is the explicit "release the lock" action (sk-674.18 T034,
// POST …/issues/{id}/unlock): it clears the claim and leaves the status as
// it is — unlike Unclaim, which moves the status too.
//
//   - no claim → idempotent no-op;
//   - the caller's own claim (held by the caller's user, any session — the
//     owner is the authority, sk-424) → released, issue.unclaimed;
//   - another holder's claim → ErrForceUnlockForbidden without
//     CanForceUnlock; with it, released, issue.unclaimed plus
//     issue.lock_force_released{by}.
//
// One SERIALIZABLE transaction, retried like Unclaim; the release is a
// compare-and-swap on the claim that was judged, and a claim replaced in
// between answers ErrNotLockHolder.
func (s *Store) ReleaseLock(ctx context.Context, input ReleaseLockInput) (*IssueDetail, error) {
	err := s.withSerializableRetry(ctx, "release_lock", unclaimMaxAttempts, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if old.LockedBy == nil || *old.LockedBy == "" {
			return nil
		}
		own := lockOwnedBy(old, input.Actor.UserID)
		if !own && !input.CanForceUnlock {
			return ErrForceUnlockForbidden
		}

		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET locked_by  = NULL,
       locked_at  = NULL,
       locked_by_user_id = NULL,
       version    = version + 1,
       updated_at = now()
 WHERE id = $1 AND locked_by = $2 AND locked_at = $3`,
			input.ID, *old.LockedBy, old.LockedAt)
		if err != nil {
			return fmt.Errorf("releasing the claim on %s: %w", input.ID, err)
		}
		if ct.RowsAffected() == 0 {
			// The claim changed between the read and the write: answered
			// like Unclaim's lost race (ErrNotLockHolder) — the caller
			// re-reads and decides on the new claim.
			return errClaimMoved
		}

		reason := "lock released"
		if !own {
			reason = "lock force-released"
		}
		if _, err := WriteIssueEvent(ctx, tx, EventIssueUnclaimed, input.ID, old.ProjectID,
			input.Actor, "lockedBy",
			map[string]any{"lockedBy": *old.LockedBy, "lockedAt": old.LockedAt},
			map[string]any{"lockedBy": nil, "lockedAt": nil},
			reason); err != nil {
			return err
		}
		if !own {
			if err := writeForceReleased(ctx, tx, input.ID, old, input.Actor, reason); err != nil {
				return err
			}
		}
		return signalClaim(ctx, tx, input.ID, signalRoles{})
	})
	if errors.Is(err, errClaimMoved) {
		return nil, ErrNotLockHolder
	}
	if err != nil {
		if isSerializationFailure(err) {
			return nil, ErrNotLockHolder
		}
		return nil, err
	}
	return s.GetIssue(ctx, input.ID)
}

// writeForceReleased writes issue.lock_force_released for the claim old
// held when actor released it with force_unlock: the explicit unlock action
// and every force path that releases another holder's claim (unclaim, close,
// a transition to closed — the lead's decision (b) on T034: a force edit
// that leaves the claim in place writes no such event).
func writeForceReleased(ctx context.Context, tx pgx.Tx, issueID string, old *issueRow, actor Actor, reason string) error {
	_, err := WriteIssueEvent(ctx, tx, EventIssueLockForceReleased, issueID, old.ProjectID,
		actor, "lockedBy",
		map[string]any{"lockedBy": *old.LockedBy, "lockedAt": old.LockedAt, "lockedByUserId": old.LockedByUserID},
		map[string]any{"by": actor.UserID},
		reason)
	return err
}

// forceReleasesAnother reports whether a write with force releases a claim
// that is not the actor's own (another user holds it): the case that
// writes issue.lock_force_released.
func forceReleasesAnother(old *issueRow, actor Actor, force bool) bool {
	return force && old.LockedBy != nil && *old.LockedBy != "" && !lockOwnedBy(old, actor.UserID)
}

// errClaimMoved — the claim judged by ReleaseLock was replaced before its
// write landed.
var errClaimMoved = errors.New("claim moved under release")

// LockHolder returns the user who holds the claim on issue id (the owner of
// the lock, issues.locked_by_user_id), uuid.Nil when the issue is not
// claimed or the owner is not recorded. It answers the per-reader flag
// canForceUnlock (US-16, row 145): the holder of the claim may release it
// as his own, so the force action is not his.
func (s *Store) LockHolder(ctx context.Context, id string) (uuid.UUID, error) {
	var holder *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT locked_by_user_id FROM issues WHERE id = $1 AND locked_by IS NOT NULL`, id).Scan(&holder); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, fmt.Errorf("reading the claim holder: %w", err)
	}
	if holder == nil {
		return uuid.Nil, nil
	}
	return *holder, nil
}

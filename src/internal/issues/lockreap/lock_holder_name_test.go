package lockreap

import (
	"context"
	"testing"
	"time"

	"skein-api/internal/issues"
)

// sk-421.1 T002 (US-15, SC-005), test-plan row 16: the lock holder's name
// on the issue detail disappears when the stale-lock reaper releases the
// lock.
//
// Two blind spots of the neighbouring fixtures are avoided on purpose:
//
//   - seedLockedIssue writes locked_by / locked_at by SQL and no
//     issue.claimed event, so the name is absent before the reaper runs
//     and «absent afterwards» would pass with no mechanism at all. The claim
//     here goes through Store.Claim, which writes the event in the claim's
//     own transaction.
//   - hijack_test.go ages only issues.locked_at. The name is matched on the
//     lock moment (locked_at = the event's occurred_at), so aging one column
//     loses the name BEFORE the reaper runs. Both columns are shifted by the
//     same relative interval in one transaction, and the name is asserted
//     after aging, before Tick.
func TestReaper_LockHolderNameDisappearsWithTheReapedLock(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	store := issues.NewStore(pool)
	id := prefix + "-1"
	seedOpenIssue(t, pool, pid, owner, id)

	const session = "mcp-session-dead-holder"
	if _, err := store.Claim(ctx, issues.ClaimInput{
		ID:        id,
		SessionID: session,
		Actor:     issues.HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The holder falls silent: its session was last seen long ago, so the
	// liveness rule does not extend the lock.
	seedAgent(t, pool, owner, session, 3*time.Hour)

	// Age the lock and its claim event together — relative shift, one tx.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE issues SET locked_at = locked_at - interval '2 hours' WHERE id = $1`, id); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("age locked_at: %v", err)
	}
	ct, err := tx.Exec(ctx, `
UPDATE events SET occurred_at = occurred_at - interval '2 hours'
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.claimed'`, id)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("age claim event: %v", err)
	}
	if ct.RowsAffected() != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("aged %d issue.claimed events, want exactly 1", ct.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit aging: %v", err)
	}

	before, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue after aging: %v", err)
	}
	if before.LockedByUsername == nil || *before.LockedByUsername != ownerName {
		t.Fatalf("after aging, before the reaper: lockedByUsername = %v, want %q — the fixture "+
			"must keep the name until the lock is actually released", before.LockedByUsername, ownerName)
	}

	newReaper(pool).Tick(ctx)

	if l := lockedBy(t, pool, id); l != nil {
		t.Fatalf("reaper did not release the stale lock: locked_by = %q", *l)
	}
	after, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue after the reaper: %v", err)
	}
	if after.LockedByUsername != nil {
		t.Errorf("after the reaper: lockedByUsername = %q, want the key absent", *after.LockedByUsername)
	}
}

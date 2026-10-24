package lockreap

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestReaper_DoesNotClearALockTakenWhileItWaited is the black-box form of
// the verdict→write window, driven through the public Tick() and made
// deterministic by a row lock rather than by luck.
//
// The interleaving:
//
//	tx B   BEGIN; UPDATE issues SET locked_by='fresh' …   (row lock held)
//	reaper Tick() → its UPDATE queues behind tx B
//	tx B   COMMIT
//	reaper the UPDATE wakes up — and now decides what to do
//
// PostgreSQL's READ COMMITTED spells out what happens at that last step:
// "If the first updater commits, the second updater … will attempt to apply
// its operation to the updated version of the row. The search condition of
// the command (the WHERE clause) is re-evaluated to see if the updated
// version of the row still matches." A WHERE that only says `issues.id =
// stale.id` still matches, so the reaper clears a lock that is milliseconds
// old — a lock it never judged. That manufactures the "in_progress with
// nobody holding it" zombie the atomic unclaim exists to prevent, which is
// the sk-243 defect with a smaller window.
//
// Two things make it impossible now: phase one takes the row with FOR
// UPDATE SKIP LOCKED, so a row somebody else is mid-write on is not this
// tick's business at all; and phase two compare-and-swaps (locked_by,
// locked_at, version), so even without the row lock the re-evaluated WHERE
// no longer matches.
func TestReaper_DoesNotClearALockTakenWhileItWaited(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	// No agents row for the holder: stale by age alone, which is the
	// pre-sk-243 path and keeps this test about the window, not liveness.
	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, "sess-abandoned", 2*time.Hour)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `
UPDATE issues
   SET locked_by = 'sess-fresh', locked_at = now(), version = version + 1
 WHERE id = $1`, id); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		newReaper(pool).Tick(ctx)
	}()

	// Give the sweep time to reach the row and queue behind tx B. A reaper
	// that skips locked rows finishes here instead; either way the commit
	// below is what the assertion is about.
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the reclaim: %v", err)
	}
	committed = true

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper tick did not finish")
	}

	got := lockedBy(t, pool, id)
	if got == nil {
		t.Fatal("the reaper cleared a lock that was taken while it waited — " +
			"the row it wrote is not the row it judged")
	}
	if *got != "sess-fresh" {
		t.Fatalf("locked_by = %q, want sess-fresh", *got)
	}
}

// TestReaper_DoesNotClearTheLockOfAnAgentThatWokeUpWhileItWaited is the
// same window from the liveness side, and the reported sk-243 defect in
// miniature: the holder was silent when the sweep looked, and speaking by
// the time the sweep wrote.
//
// The row lock here is a bare touch of `updated_at` — it changes nothing
// about the lock, it only makes the interleaving deterministic by parking
// the reaper's write behind it. What matters is what commits during the
// wait: a heartbeat in `agents`.
//
// A single statement cannot survive this, and not by oversight. READ
// COMMITTED re-evaluates the WHERE against the updated row, but "it does
// not see effects of those commands on OTHER rows in the database" — the
// heartbeat lands in `agents`, so no re-evaluation of any predicate inside
// that one statement can ever see it. Only a new statement can, because
// "Read Committed mode starts each command with a new snapshot". That is
// why the sweep is two statements in one transaction rather than one
// cleverer statement.
func TestReaper_DoesNotClearTheLockOfAnAgentThatWokeUpWhileItWaited(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	const session = "mcp-session-wakes-mid-sweep"
	seedAgent(t, pool, owner, session, 2*time.Hour)

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, session, 2*time.Hour)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	// Touch the row — the lock is untouched; this only parks the reaper.
	if _, err := tx.Exec(ctx,
		`UPDATE issues SET updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("touch: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		newReaper(pool).Tick(ctx)
	}()
	time.Sleep(300 * time.Millisecond)

	// The agent wakes up and calls a tool.
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = now() WHERE user_id = $1 AND session_id = $2`,
		owner, session); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the touch: %v", err)
	}
	committed = true

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper tick did not finish")
	}

	if got := lockedBy(t, pool, id); got == nil {
		t.Fatal("the reaper released the lock of an agent that spoke before the " +
			"write landed — the verdict outlived the facts it was based on (sk-243)")
	} else if *got != session {
		t.Fatalf("locked_by = %q, want %q", *got, session)
	}
}

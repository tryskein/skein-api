package lockreap

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// sk-243 security follow-up (F1) — the window between the verdict and the
// write.
//
// sk-243 stopped the reaper from releasing the lock of an agent that was
// demonstrably alive. It did so with one statement whose CTE decided
// staleness and whose UPDATE then wrote, and under READ COMMITTED those two
// halves are not the same instant: the statement's snapshot freezes the
// liveness verdict, the row lock is taken later, and the gap between them is
// real wall-clock time (an index scan, up to BatchSize rows, a wait on a
// row lock). An agent that woke up inside that gap, or a lock that was
// released and re-taken inside it, was written over anyway — the reported
// defect again, in a narrower window.
//
// These two tests drive the window directly. They call the two phases by
// hand with the interleaving in between, which is the only way to make a
// race deterministic: the point is not that it is likely, the point is that
// the code must not be able to lose.

// TestReaper_HolderWakesUpInsideTheWindow: the sweep judged the holder
// silent — correctly, at that moment — and the holder spoke before the
// write landed. The lock must survive.
//
// The heartbeat is committed from a SEPARATE connection, so it is exactly
// what a real /mcp request does: a committed write to `agents` that never
// touches the issue row, and therefore can neither block the reaper's
// UPDATE nor be seen by a statement whose snapshot predates it. Only a
// re-read in a NEW statement can see it.
func TestReaper_HolderWakesUpInsideTheWindow(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	const session = "mcp-session-wakes-up"
	seedAgent(t, pool, owner, session, 2*time.Hour)

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, session, 2*time.Hour)

	r := newReaper(pool)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cands, err := r.selectCandidates(ctx, tx)
	if err != nil {
		t.Fatalf("selectCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].IssueID != id {
		t.Fatalf("candidates = %+v, want exactly the stale lock %s", cands, id)
	}

	// The window. The agent wakes up and calls a tool; the registry row is
	// bumped and committed while the sweep is between its two phases.
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = now() WHERE user_id = $1 AND session_id = $2`,
		owner, session); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	released, err := r.releaseCandidates(ctx, tx, cands)
	if err != nil {
		t.Fatalf("releaseCandidates: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released %d lock(s) whose holder spoke before the write landed: %+v",
			len(released), released)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := lockedBy(t, pool, id); got == nil {
		t.Fatal("the lock of a live agent was released (sk-243, in the verdict→write window)")
	} else if *got != session {
		t.Fatalf("locked_by = %q, want %q", *got, session)
	}
}

// TestReaper_LockIsReclaimedInsideTheWindow: the lock the sweep judged is
// not the lock it would write over. Between the two phases the holder
// dropped the task and somebody claimed it afresh, so the row now carries a
// seconds-old lock. Clearing it would manufacture the same "in_progress
// with nobody holding it" zombie the atomic unclaim exists to prevent.
//
// Phase one's FOR UPDATE makes this unreachable in a live sweep, which is
// exactly why the test drives phase two on its own: the release statement
// has to be safe by its own WHERE clause, so that dropping the row lock
// some day fails a test instead of silently reopening the race. Under READ
// COMMITTED the UPDATE's WHERE is re-evaluated against the updated row
// version, so the compare-and-swap is what makes that guarantee real.
func TestReaper_LockIsReclaimedInsideTheWindow(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, "mcp-session-abandoned", 2*time.Hour)

	// The verdict: what phase one saw.
	var judged candidate
	judged.IssueID = id
	if err := pool.QueryRow(ctx,
		`SELECT project_id, locked_by, locked_at, version FROM issues WHERE id = $1`, id,
	).Scan(&judged.ProjectID, &judged.LockedBy, &judged.LockedAt, &judged.Version); err != nil {
		t.Fatalf("read the judged lock: %v", err)
	}

	// The window: unclaim + fresh claim, exactly as Store.Unclaim and
	// Store.Claim write them (both bump version).
	if _, err := pool.Exec(ctx, `
UPDATE issues
   SET locked_by = 'mcp-session-fresh', locked_at = now(), version = version + 1
 WHERE id = $1`, id); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	r := newReaper(pool)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	released, err := r.releaseCandidates(ctx, tx, []candidate{judged})
	if err != nil {
		t.Fatalf("releaseCandidates: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released %d lock(s) that had been re-claimed since the verdict: %+v",
			len(released), released)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := lockedBy(t, pool, id); got == nil {
		t.Fatal("a seconds-old lock was cleared because the sweep judged its predecessor")
	} else if *got != "mcp-session-fresh" {
		t.Fatalf("locked_by = %q, want the fresh claim", *got)
	}
}

// The sweep must still do its job: same two phases, nothing happening in
// the window, holder silent — the lock goes. Without this the two tests
// above would pass just as well against a reaper that never releases
// anything.
func TestReaper_PhasedSweepStillReleasesASilentHolder(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	const session = "mcp-session-really-gone"
	seedAgent(t, pool, owner, session, 3*time.Hour)

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, session, 2*time.Hour)

	r := newReaper(pool)
	released, candidates, err := r.reapOnce(ctx)
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}
	if candidates != 1 || len(released) != 1 || released[0].IssueID != id {
		t.Fatalf("candidates = %d, released = %+v, want exactly %s", candidates, released, id)
	}
	if got := lockedBy(t, pool, id); got != nil {
		t.Fatalf("locked_by = %q, want NULL", *got)
	}
}

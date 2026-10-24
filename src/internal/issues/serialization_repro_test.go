package issues

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// waitForBlocked polls pg_locks/pg_stat_activity, scoped to the backend
// tagged with appName (via newTaggedPool), until it observes at least one
// ungranted lock held by that backend — i.e. the target operation is
// genuinely parked behind a row lock, not merely "about to run".
//
// This is the sk-202 repro technique (store_claim_serialization_test.go)
// factored out for reuse: any store method that does read-then-conditional-
// UPDATE on a single row can be forced into a REAL, deterministic SQLSTATE
// 40001 by (1) opening a concurrent transaction that touches the row
// without changing what the target's predicate checks, (2) waiting for the
// target to block on that row's lock (this function), (3) committing the
// concurrent writer so the target's own UPDATE takes the abort. Scoping to
// ONE tagged backend (not "is anything blocked database-wide") matters
// because the suite shares one database with every other package.
func waitForBlocked(t *testing.T, pool *pgxpool.Pool, appName string, timeout time.Duration) bool {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM pg_locks l
  JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE NOT l.granted
   AND a.application_name = $1`, appName).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// waitForPIDBlocked polls pg_locks for the given single backend PID, until
// it holds an ungranted lock — i.e. that connection is genuinely PARKED in
// Postgres's row-lock wait queue, not merely "Exec has been called".
// forceSerializationExhaustion uses this (scoped by pg_backend_pid(), not
// application_name — these are plain writer transactions on the suite's
// already-open pool, not worth a separate tagged connection) to confirm a
// stand-by writer actually queued up behind its predecessor before that
// predecessor is committed.
func waitForPIDBlocked(t *testing.T, pool *pgxpool.Pool, pid int32, timeout time.Duration) bool {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var granted bool
		err := pool.QueryRow(ctx, `
SELECT granted
  FROM pg_locks
 WHERE pid = $1
 ORDER BY granted ASC
 LIMIT 1`, pid).Scan(&granted)
		if err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if !granted {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// forceSerializationExhaustion deterministically drives a
// withSerializableRetry-wrapped target through losing EVERY one of its
// `attempts` tries against a genuine class-40 abort — not just the first
// one, which is all waitForBlocked's single-shot callers (setPinned,
// UpdateComment, DeleteComment happy-path tests) need. Call it instead of
// a single waitForBlocked+commit when the assertion under test is about
// EXHAUSTION (the budget running out), not about "one retry absorbs one
// competitor".
//
// Mechanics — one FRESH writer opened just-in-time per round, never a
// writer that has already lost an earlier round:
//
//  1. writer[0] opens a plain (READ COMMITTED — see below) transaction and
//     runs updateSQL against the SAME row the target contends on, taking
//     its row lock synchronously. This wins the initial race against the
//     target's own first attempt for the same reason the single-shot
//     waitForBlocked callers do: the target's path to its own UPDATE goes
//     through a SELECT and business logic first, so this one-shot UPDATE
//     is reliably faster.
//  2. For each of the remaining `attempts-1` rounds: open writer[k+1] WHILE
//     writer[k] still holds the row (so writer[k+1] has ample time — the
//     whole waitForBlocked(target) confirmation below — to park itself in
//     Postgres's wait queue before writer[k] is released), THEN commit
//     writer[k]. Postgres hands the row straight to writer[k+1] — the only
//     other party waiting on writer[k]'s xid at that instant — and
//     delivers the target's current-attempt abort in the same instant.
//     Confirm writer[k+1] actually took over (its Exec, blocked since it
//     was opened, returns once granted) and that the target re-blocked on
//     it before moving to the next round.
//
// Why "one fresh writer per round", not "queue every writer up front
// before releasing any of them" (the first version of this helper): with
// N writers pre-queued behind writer[0] simultaneously, releasing writer[0]
// makes ALL of writer[1..N-1] AND the target's current attempt race for
// the freed row at once. Empirically (verified with an instrumented probe
// against this exact schema/isolation combination — see the sk-303.14
// investigation notes for the reproduction) whichever of writer[1..N-1]
// is racing for the FIRST slot reliably beats the target, and so does the
// SECOND slot's fresh racer — but a writer that has ALREADY LOST one or
// more of those earlier races and had to internally re-park itself onto
// the next holder is measurably slower to re-register than a writer
// opened fresh for that specific slot, and the target's retry wins that
// particular race instead: the LAST writer in an all-pre-queued chain is
// always the one that has lost the most prior races, so the target's
// FINAL attempt — the one exhaustion depends on — was exactly the one
// most likely to slip through. A writer opened just-in-time for a given
// round has never lost anything: its first-ever wait registration targets
// precisely the writer it needs to beat, on equal footing with the
// target's own fresh attempt, and it wins that footing race reliably
// (confirmed over repeated runs, including with the settle time before
// the release deliberately widened, which ruled out "the writer just
// hasn't caught up yet" as an alternative explanation).
//
// This still closes the ORIGINAL gap a purely sequential version left:
// "sequential" here means opening writer[k+1] only AFTER committing
// writer[k], which leaves the row briefly free while a brand-new
// BEGIN+UPDATE round-trip reaches the server — enough of a window, under
// load, for the target's own retry to win it outright. Opening writer[k+1]
// BEFORE writer[k] is committed removes that window: by the time writer[k]
// is released, writer[k+1] is already parked behind it, confirmed via
// waitForPIDBlocked.
//
// ⚠ Writers MUST run at READ COMMITTED (Postgres's default — plain
// pool.Begin(ctx), no IsoLevel override, matches this suite's pool
// config), NOT SERIALIZABLE. Under READ COMMITTED, a writer blocked on
// the previous writer's row lock simply re-reads the row once unblocked
// and proceeds — the handoff is silent. Under SERIALIZABLE, that same
// blocked writer would ALSO take a first-committer-wins abort the instant
// it unblocks (same rule the target itself is subject to), and the relay
// would collapse into every writer failing instead of relaying the lock.
//
// updateSQL must be a statement that takes an exclusive row lock the
// target's own statement also takes on the SAME row/predicate (e.g. an
// UPDATE on the identical WHERE clause) — the same requirement
// waitForBlocked's original callers already satisfy.
//
// start launches the target itself (the withSerializableRetry-wrapped
// call under test) and MUST be invoked by this function — not by the
// caller beforehand — and only AFTER writer[0] already holds the row.
// Every one of this suite's single-collision tests (setPinned,
// UpdateComment/DeleteComment's "_Retries" siblings) opens its one
// concurrent writer BEFORE launching the target goroutine, guaranteeing
// the writer wins the initial race for a still-free row outright. The
// four "_Exhausted" tests this helper serves used to launch the target
// FIRST and call this helper second, leaving writer[0]'s acquisition to
// race the target's own first attempt for that same free row — usually
// won by writer[0] (its path is a single UPDATE; the target's goes
// through a SELECT and business logic first), but not reliably enough:
// observed losing that race under repeated back-to-back runs of the full
// suite, which reports as "target never blocked behind writer 0" instead
// of a false exhaustion result, but is exactly the same class of gap
// forceSerializationExhaustion exists to close. Taking over the launch
// removes the gap the same way pre-registering each writer does: nothing
// about the target's timing is left for the caller to accidentally get
// wrong.
func forceSerializationExhaustion(t *testing.T, pool *pgxpool.Pool, appName string, attempts int, start func(), updateSQL string, args ...any) {
	t.Helper()
	ctx := context.Background()

	writers := make([]pgx.Tx, attempts)
	t.Cleanup(func() {
		// Best-effort: any writer this function itself did not already
		// commit gets rolled back so a Fatalf mid-run doesn't leave a
		// transaction (and the goroutine blocked inside it) dangling past
		// the test. Rollback on an already-committed/rolled-back tx is a
		// safe no-op (ErrTxClosed, ignored).
		for _, w := range writers {
			if w != nil {
				_ = w.Rollback(ctx)
			}
		}
	})

	// writer[0]: acquire the row lock synchronously (see step 1 above) —
	// BEFORE the target exists at all, so there is nothing for it to race.
	tx0, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer 0/%d: %v", attempts, err)
	}
	writers[0] = tx0
	if _, err := tx0.Exec(ctx, updateSQL, args...); err != nil {
		t.Fatalf("writer 0/%d UPDATE: %v", attempts, err)
	}
	start()
	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("target never blocked behind writer 0/%d — could not orchestrate exhaustion", attempts)
	}

	// One fresh writer per round — see step 2 above.
	for round := 0; round < attempts-1; round++ {
		txi, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin writer %d/%d: %v", round+1, attempts, err)
		}
		var wpid int32
		if err := txi.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&wpid); err != nil {
			t.Fatalf("writer %d/%d backend pid: %v", round+1, attempts, err)
		}
		writers[round+1] = txi

		execDone := make(chan error, 1)
		go func(tx pgx.Tx) {
			_, execErr := tx.Exec(ctx, updateSQL, args...)
			execDone <- execErr
		}(txi)

		if !waitForPIDBlocked(t, pool, wpid, 10*time.Second) {
			t.Fatalf("writer %d/%d never queued behind writer %d/%d — could not orchestrate exhaustion", round+1, attempts, round, attempts)
		}

		if err := writers[round].Commit(ctx); err != nil {
			t.Fatalf("commit writer %d/%d: %v", round, attempts, err)
		}

		// Confirm writer[round+1] actually took over the row (its Exec,
		// blocked since it was opened, returns once granted) before
		// checking that the target re-blocked on it — a precise failure
		// here ("writer never dequeued") beats a vague "target never
		// re-blocked" that could equally mean "nobody holds the row".
		select {
		case execErr := <-execDone:
			if execErr != nil {
				t.Fatalf("writer %d/%d UPDATE (after dequeue): %v", round+1, attempts, execErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("writer %d/%d never dequeued after writer %d/%d committed", round+1, attempts, round, attempts)
		}

		if !waitForBlocked(t, pool, appName, 10*time.Second) {
			t.Fatalf("target never re-blocked behind writer %d/%d", round+1, attempts)
		}
	}

	// Release the final writer — this delivers the target's `attempts`-th
	// (final) abort. Nothing needs to queue behind it: withSerializableRetry
	// gives up after this attempt instead of retrying again.
	if err := writers[attempts-1].Commit(ctx); err != nil {
		t.Fatalf("commit writer %d/%d: %v", attempts-1, attempts, err)
	}
}

package issues

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTaggedPool returns a pool whose backends all report application_name
// = appName, so a test can single out ITS OWN connections in
// pg_stat_activity / pg_locks.
//
// The suite shares one database with every other package (and with any
// human poking at it), so "is someone blocked on a lock right now?" is not
// a question a test may ask database-globally: an unrelated blocked
// backend answers yes and the test proceeds before the thing it is
// actually waiting for has happened. Tagging the connection turns the
// question into "is THIS backend blocked?", which only this test can
// cause.
func newTaggedPool(t *testing.T, appName string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	// application_name is truncated to NAMEDATALEN-1 (63) bytes by the
	// server; a silently truncated tag would match the wrong backends.
	if len(appName) > 63 {
		t.Fatalf("application_name %q is %d bytes, max 63", appName, len(appName))
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURL, err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", testDatabaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", testDatabaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// uniqueTag returns a short, unique, identifier-safe suffix for
// per-test database objects (application_name, sequence / trigger names).
func uniqueTag() string {
	return strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
}

// releaseOutboxBacklog marks every still-unpublished event of the given
// project as published, so the test leaves no outbox backlog behind.
//
// The suite shares one database and runs `go test -p 1`, which makes
// unpublished `events` rows cumulative ACROSS packages. internal/outbox's
// drainProject ticks the GLOBAL outbox in id order (BatchSize 10 × 100
// ticks = 1000 rows) before it can reach its own freshly-inserted row, so
// an earlier package that leaves a few hundred unpublished events behind
// pushes that test past its budget and fails it — with a message about the
// outbox, nowhere near the actual cause.
//
// The create/claim loop below writes several events per iteration, enough
// to matter. Cleaning up keeps this file's footprint at zero instead of
// tuning the loop count to sneak under someone else's threshold. Mirrors
// internal/outbox's own resetOutbox idiom, scoped to the seeded project.
func releaseOutboxBacklog(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE events SET published_at = now()
		  WHERE published_at IS NULL AND project_id = $1`, projectID); err != nil {
		t.Fatalf("release outbox backlog: %v", err)
	}
}

// sk-202 — Claim must not report `already_claimed` for a serialization
// failure.
//
// Claim runs its (SELECT → UPDATE) under SERIALIZABLE. Postgres raises
// SQLSTATE 40001 on that UPDATE whenever ANY concurrent transaction wrote
// the same row after Claim's snapshot was taken — and also, under SSI, for
// read/write dependency cycles that involve rows Claim never touched. The
// pre-fix code funnelled every 40001 into ErrAlreadyClaimed on the premise
// that "another claim landed first" (store.go), which is only one of many
// ways to get a 40001. The transaction rolls back, so the row keeps
// locked_by=NULL / assignee=NULL / version=1 while the caller is told the
// issue belongs to somebody else — the exact prod signature reported in
// sk-202, where an identical retry ~18s later succeeded.
//
// The tests below force a real 40001 with a concurrent writer that touches
// the row WITHOUT bumping version (the shape AddLabel / RemoveLabel and the
// comment-touch paths use: `UPDATE issues SET updated_at = now()`), then
// assert Claim tells the truth about a free issue.

// TestClaim_SerializationFailure_NotReportedAsAlreadyClaimed seeds a free
// issue, parks a concurrent writer on its row, runs Claim until it blocks on
// that writer's row lock, then commits the writer so Claim's UPDATE takes
// SQLSTATE 40001 — and asserts Claim does not answer "already claimed".
func TestClaim_SerializationFailure_NotReportedAsAlreadyClaimed(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	// Claim runs on its own tagged pool so the "is Claim blocked yet?"
	// poll below can name the exact backend it is waiting on. The
	// concurrent writer and the poller use the untagged pool.
	appName := "sk202-claim-" + uniqueTag()
	claimPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	claimStore := NewStore(claimPool)

	dto, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title:    "sk-202 serialization repro",
			Type:     "task",
			Priority: 2,
			// No assignee → the issue is free: assignee NULL, locked_by NULL.
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.Assignee != nil || dto.Version != 1 {
		t.Fatalf("precondition: want free issue at version 1, got assignee=%v version=%d",
			dto.Assignee, dto.Version)
	}

	// Concurrent writer: touches updated_at only — no version bump, no
	// lock, no assignee. Mirrors AddLabel / RemoveLabel / comment-touch.
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()

	if _, err := other.Exec(ctx,
		`UPDATE issues SET updated_at = now() WHERE id = $1`, dto.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	// Claim races on another connection; it will park on the row lock the
	// uncommitted writer above holds.
	//
	// Claim gets its OWN cancellable context rather than t.Context(): the
	// test context is cancelled the moment the test function returns, which
	// would race the in-flight claim. WithoutCancel keeps the ctx values and
	// drops only that automatic cancellation; the explicit cancel below is
	// what guarantees the goroutine can always be wound up.
	type claimResult struct {
		dto *IssueDetail
		err error
	}
	// resCh is buffered so the goroutine never blocks on send even if the
	// test bailed out and nobody reads it; claimDone reports termination
	// independently of whether the result was consumed.
	resCh := make(chan claimResult, 1)
	claimDone := make(chan struct{})
	claimCtx, cancelClaim := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(claimDone)
		d, err := claimStore.Claim(claimCtx, ClaimInput{
			ID:        dto.ID,
			SessionID: "sess-sk202-victim",
			Actor:     HumanActor(creator, creatorName),
		})
		resCh <- claimResult{d, err}
	}()
	// Join the goroutine on EVERY exit path, including the t.Fatalf calls
	// below. Cleanups run LIFO and newTaggedPool registered its pool.Close
	// first, so this runs before the claim's pool is torn out from under it —
	// otherwise an orchestration failure leaves Claim in flight against a
	// closing pool.
	t.Cleanup(func() {
		cancelClaim()
		select {
		case <-claimDone:
		case <-time.After(5 * time.Second):
			t.Errorf("claim goroutine did not finish after cancellation")
		}
	})

	// Synchronise: wait until Claim's UPDATE is genuinely blocked.
	//
	// Scoped to Claim's own backend via application_name. The database-wide
	// variant of this poll ("any ungranted lock whose query looks like an
	// UPDATE on issues") could be satisfied by unrelated traffic on the
	// shared test database and let the test commit the writer before Claim
	// had blocked — which would silently stop reproducing sk-202 while
	// still passing.
	blocked := false
	deadline := time.Now().Add(10 * time.Second)
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
			blocked = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !blocked {
		t.Fatalf("could not orchestrate the race: Claim never blocked on the concurrent writer's row lock")
	}

	// Release the writer → Claim's UPDATE hits 40001.
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got claimResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("Claim did not return within 30s")
	}

	if errors.Is(got.err, ErrAlreadyClaimed) {
		// The sk-202 bug: nobody holds this issue.
		var lockedBy, assignee *string
		var status string
		var version int
		if err := pool.QueryRow(ctx,
			`SELECT locked_by, assignee, status, version FROM issues WHERE id = $1`, dto.ID,
		).Scan(&lockedBy, &assignee, &status, &version); err != nil {
			t.Fatalf("post-state read: %v", err)
		}
		t.Fatalf("Claim returned already_claimed after a serialization failure; "+
			"row state: locked_by=%v assignee=%v status=%s version=%d "+
			"(nobody holds the issue — the caller was lied to)",
			deref(lockedBy), deref(assignee), status, version)
	}
	if got.err != nil {
		t.Fatalf("Claim: unexpected error %v", got.err)
	}
	if got.dto == nil {
		t.Fatalf("Claim returned nil dto without error")
	}
	if got.dto.LockedBy == nil || *got.dto.LockedBy != "sess-sk202-victim" {
		t.Fatalf("lockedBy = %v, want sess-sk202-victim", got.dto.LockedBy)
	}
	if got.dto.Status != StatusInProgress {
		t.Fatalf("status = %s, want in_progress", got.dto.Status)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *s)
}

// TestClaim_LockedByOther_CarriesBranchAndRowState pins the sk-202 AC-1
// data contract: a genuine already_claimed must carry the predicate that
// rejected AND the row state that justified it, so the MCP layer can log
// why without re-reading the row. A bare ErrAlreadyClaimed with no detail
// is the shape that made the production incident undiagnosable.
func TestClaim_LockedByOther_CarriesBranchAndRowState(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)

	dto, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-202 lock detail", Type: "task", Priority: 3},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if _, err := store.Claim(ctx, ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-owner",
		Actor:     HumanActor(creator, creatorName),
	}); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	// A different session now legitimately loses.
	_, err = store.Claim(ctx, ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-intruder",
		Actor:     HumanActor(creator, creatorName),
	})
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second Claim err = %v, want ErrAlreadyClaimed", err)
	}

	var acErr *AlreadyClaimedError
	if !errors.As(err, &acErr) {
		t.Fatalf("already_claimed carries no *AlreadyClaimedError detail (%T) — "+
			"the MCP layer would log branch=unknown", err)
	}
	if acErr.Branch != "locked_by_other" {
		t.Errorf("branch = %q, want locked_by_other", acErr.Branch)
	}
	if acErr.LockedBy != "sess-owner" {
		t.Errorf("lockedBy = %q, want sess-owner", acErr.LockedBy)
	}
	if acErr.Status != StatusInProgress {
		t.Errorf("status = %q, want in_progress", acErr.Status)
	}
	if acErr.Assignee != creatorName {
		t.Errorf("assignee = %q, want %q", acErr.Assignee, creatorName)
	}
}

// TestClaim_ImmediatelyAfterCreate_Succeeds walks an agent's canonical
// create→claim sequence, which is where sk-202 was observed: a claim issued
// seconds after skein_create came back `already_claimed` on an issue nobody
// held.
//
// Honest about its own power: this loop is a smoke test, not the proof. It
// only catches the bug if a concurrent writer happens to touch the row in
// the same instant, which nothing here forces — a green run here does NOT
// mean the race is gone. TestClaim_SerializationFailure_NotReportedAsAlready
// Claimed above is the deterministic proof; this one guards the plain,
// uncontended path against regressions.
func TestClaim_ImmediatelyAfterCreate_Succeeds(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)

	const iterations = 50
	for i := 0; i < iterations; i++ {
		dto, err := store.CreateIssue(ctx, pid, prefix, "counter",
			CreateIssueInput{
				Title:    fmt.Sprintf("sk-202 create-then-claim %d", i),
				Type:     "task",
				Priority: 3,
			},
			creator,
			HumanActor(creator, creatorName),
		)
		if err != nil {
			t.Fatalf("iteration %d: CreateIssue: %v", i, err)
		}

		claimed, err := store.Claim(ctx, ClaimInput{
			ID:        dto.ID,
			SessionID: fmt.Sprintf("sess-sk202-loop-%d", i),
			Actor:     HumanActor(creator, creatorName),
		})
		if err != nil {
			t.Fatalf("iteration %d: Claim on freshly-created %s: %v", i, dto.ID, err)
		}
		if claimed.Status != StatusInProgress {
			t.Fatalf("iteration %d: status = %s, want in_progress", i, claimed.Status)
		}
	}
}

// forceIssueUpdateAbort makes EVERY UPDATE of issueID raise sqlstate, and
// counts how many times it fired. Returns that counter's reader.
//
// This is how the retry-exhaustion branch is driven deterministically. The
// alternative — orchestrating three real conflicts back-to-back inside
// Claim's ~25-125ms backoff window — is inherently racy, and a fake pool is
// not available (Store holds a concrete *pgxpool.Pool). A BEFORE UPDATE
// trigger raising the SQLSTATE Postgres itself would raise is neither: the
// error is a genuine *pgconn.PgError travelling the real pgx → BeginTxFunc
// → claimWithRetry → Claim path, with no test hook in production code.
//
// The counter is a SEQUENCE precisely because nextval() is non-transactional
// — it survives the rollback that the RAISE triggers, so it can count
// attempts that all rolled back. The trigger is scoped to one issue id by
// its WHEN clause and dropped on cleanup, so it cannot touch other tests.
func forceIssueUpdateAbort(t *testing.T, pool *pgxpool.Pool, issueID, sqlstate string) func() int {
	t.Helper()
	ctx := context.Background()
	tag := uniqueTag()
	seq := "sk202_attempts_" + tag
	fn := "sk202_abort_" + tag
	trg := "sk202_trg_" + tag

	exec := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install abort trigger (%s): %v", q, err)
		}
	}
	exec(fmt.Sprintf(`CREATE SEQUENCE %s`, seq))
	exec(fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    PERFORM nextval('%s');
    RAISE EXCEPTION 'sk-202 forced transaction abort' USING ERRCODE = '%s';
END
$fn$`, fn, seq, sqlstate))
	exec(fmt.Sprintf(`
CREATE TRIGGER %s BEFORE UPDATE ON issues
    FOR EACH ROW WHEN (NEW.id = '%s')
    EXECUTE FUNCTION %s()`, trg, issueID, fn))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON issues`, trg))
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP SEQUENCE IF EXISTS %s`, seq))
	})

	return func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM %s`, seq)).Scan(&n); err != nil {
			t.Fatalf("read attempt counter: %v", err)
		}
		return n
	}
}

// TestClaim_RetriesExhausted_ReturnsSerializationRetry covers the branch the
// sk-202 fix introduced but left untested: what a caller gets when the retry
// budget runs out. The deterministic repro above proves "one 40001 → retry →
// success"; this proves "every attempt lost → ErrSerializationRetry", never
// ErrAlreadyClaimed and never a bare internal error.
//
// The 40P01 case additionally pins the sk-202 review fix: claimWithRetry
// retries deadlocks as well as serialization failures (isRetryableTxError),
// so a deadlocked claim reaches the same honest "nothing written, retry"
// answer instead of surfacing as internal_error.
//
// The 23505 case is the boundary control: a non-retryable error must abort
// after ONE attempt and must NOT be laundered into ErrSerializationRetry.
func TestClaim_RetriesExhausted_ReturnsSerializationRetry(t *testing.T) {
	cases := []struct {
		name         string
		sqlstate     string
		wantAttempts int
		wantRetryErr bool
	}{
		{"serialization_failure", pgSerializationFailure, claimMaxAttempts, true},
		{"deadlock_detected", pgDeadlockDetected, claimMaxAttempts, true},
		{"non_retryable_unique_violation", pgUniqueViolation, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			ctx := t.Context()

			creator := seedUser(t, pool)
			creatorName := readUsername(t, pool, creator)
			pid, prefix := seedProject(t, pool, creator)
			t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
			store := NewStore(pool)

			dto, err := store.CreateIssue(ctx, pid, prefix, "counter",
				CreateIssueInput{Title: "sk-202 exhausted " + tc.name, Type: "task", Priority: 3},
				creator, HumanActor(creator, creatorName))
			if err != nil {
				t.Fatalf("CreateIssue: %v", err)
			}

			attempts := forceIssueUpdateAbort(t, pool, dto.ID, tc.sqlstate)

			_, err = store.Claim(ctx, ClaimInput{
				ID:        dto.ID,
				SessionID: "sess-sk202-exhausted",
				Actor:     HumanActor(creator, creatorName),
			})

			if err == nil {
				t.Fatalf("Claim succeeded despite a forced %s on every UPDATE", tc.sqlstate)
			}
			if errors.Is(err, ErrAlreadyClaimed) {
				t.Fatalf("Claim returned already_claimed for a %s — the sk-202 bug: "+
					"nobody holds this issue", tc.sqlstate)
			}
			if got := errors.Is(err, ErrSerializationRetry); got != tc.wantRetryErr {
				t.Fatalf("errors.Is(err, ErrSerializationRetry) = %v, want %v (err = %v)",
					got, tc.wantRetryErr, err)
			}
			if got := attempts(); got != tc.wantAttempts {
				t.Errorf("UPDATE attempted %d times, want %d", got, tc.wantAttempts)
			}

			// "Nothing was written, retry verbatim" must be literally true:
			// every attempt rolled back, so the row is as it was created.
			var lockedBy, assignee *string
			var status string
			var version int
			if err := pool.QueryRow(ctx,
				`SELECT locked_by, assignee, status, version FROM issues WHERE id = $1`, dto.ID,
			).Scan(&lockedBy, &assignee, &status, &version); err != nil {
				t.Fatalf("post-state read: %v", err)
			}
			if lockedBy != nil || status != StatusOpen || version != 1 {
				t.Errorf("issue was mutated by a fully-rolled-back claim: "+
					"locked_by=%s status=%s version=%d, want <nil>/open/1",
					deref(lockedBy), status, version)
			}
		})
	}
}

// TestSerializationPredicates_AreDeliberatelyDifferent pins the sk-202
// review decision that the issues package carries TWO SQLSTATE predicates
// rather than one widened helper.
//
// isSerializationFailure gates the "→ ErrVersionConflict" mapping used by
// Patch / UpdateStatus / ReplaceComponents / AddLabel / RemoveLabel / Close.
// Teaching it 40P01 would make every deadlock on those paths report
// version_conflict quoting a version that never changed — an agent following
// the "re-read and retry on the fresh version" contract would loop. So the
// claim path, which replays the transaction instead of blaming the version,
// gets its own predicate. Both facts are asserted here so neither can be
// "simplified" back into one.
func TestSerializationPredicates_AreDeliberatelyDifferent(t *testing.T) {
	cases := []struct {
		name                     string
		err                      error
		wantSerializationFailure bool
		wantRetryable            bool
	}{
		{"40001 serialization_failure", &pgconn.PgError{Code: pgSerializationFailure}, true, true},
		{"40P01 deadlock_detected", &pgconn.PgError{Code: pgDeadlockDetected}, false, true},
		{"23505 unique_violation", &pgconn.PgError{Code: pgUniqueViolation}, false, false},
		{"wrapped 40P01", fmt.Errorf("claiming issue: %w", &pgconn.PgError{Code: pgDeadlockDetected}), false, true},
		{"non-pg error", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSerializationFailure(tc.err); got != tc.wantSerializationFailure {
				t.Errorf("isSerializationFailure(%v) = %v, want %v", tc.err, got, tc.wantSerializationFailure)
			}
			if got := isRetryableTxError(tc.err); got != tc.wantRetryable {
				t.Errorf("isRetryableTxError(%v) = %v, want %v", tc.err, got, tc.wantRetryable)
			}
		})
	}
}

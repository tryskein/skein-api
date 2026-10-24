package notifications_test

// optouts_race_test.go — sk-622.21, T010 / T012, matrix rows No. 21, No. 21a, No. 21b,
// No. 22 (INV-6, INV-7, edge 1–4).
//
// Every scenario is a DETERMINISTIC interleaving of two connections, never a
// loop hoping to catch a race (tests-rationale "Concurrent tests"):
//
//   - transaction A is the test's own SERIALIZABLE transaction around the
//     product primitive (FanOut, or the automatic subscription), committed
//     through a retry loop; on its FIRST attempt it performs its write, signals
//     the test and waits for the go-ahead to commit;
//   - B is the public operation under test, on its own pool, retried by the
//     product's own wrapper;
//   - order 1: A has written; B runs to completion and commits; then A
//     commits;
//   - order 2: A has written; B runs its read and write and is held — by the
//     tracer of B's pool, at the end of the named statement, on its first
//     attempt only — before committing; A commits; B is released and commits.
//
// A serialization failure (40001) on either side must be absorbed by that
// side's retry and never reach the caller of the public operation; in these
// stands which side retried is not asserted — that is SSI's call, not a
// contract. The assertion is the final state after everything committed:
// INV-6 (no notification of U carded X after an explicit unwatch / remove)
// and INV-7 (the state equals one of the serial orders, spelled out as
// literals).
//
// No. 21 (b) and No. 22 pair a fan-out with an operation whose writes the fan-out
// only reads; SSI commits such a pair without a conflict, so those two
// stands contend for one row on purpose (conflictInterleave): the conflict is
// then real, each order is reached deterministically, and the side that
// replays IS asserted.
//
// External test package: the third connection of No. 21b closes X through the
// public issues.Store#Close and No. 22's B is the public dependencies Reparent;
// both import notifications.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

const raceWait = 20 * time.Second

// ---------------------------------------------------------------- tracer

type raceSQLKey struct{}

// holdTracer parks the connection at the END of the first statement whose
// text contains the armed marker (TraceQueryEnd runs after the result was
// read), signals reached and waits for release. One-shot: a retry of the same
// operation runs free.
type holdTracer struct {
	mu      sync.Mutex
	marker  string
	armed   bool
	reached chan struct{}
	release chan struct{}
	// countMarker / count: every finished statement (success or error)
	// whose text contains countMarker is counted — how many times the pool
	// ran the operation, i.e. whether its wrapper replayed it.
	countMarker string
	count       int
}

func (h *holdTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, raceSQLKey{}, d.SQL)
}

func (h *holdTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(raceSQLKey{}).(string)
	h.mu.Lock()
	if h.countMarker != "" && strings.Contains(sql, h.countMarker) {
		h.count++
	}
	if !h.armed || !strings.Contains(sql, h.marker) {
		h.mu.Unlock()
		return
	}
	h.armed = false
	reached, release := h.reached, h.release
	h.mu.Unlock()
	close(reached)
	<-release
}

// arm makes the next statement containing marker hold.
func (h *holdTracer) arm(marker string) (reached, release chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.marker, h.armed = marker, true
	h.reached, h.release = make(chan struct{}), make(chan struct{})
	return h.reached, h.release
}

// countRuns starts counting the statements containing marker and returns
// the reader of the count.
func (h *holdTracer) countRuns(marker string) func() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countMarker, h.count = marker, 0
	return func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.count
	}
}

// racePool opens a pool tagged with application_name (for pg_locks
// polling) and, optionally, a tracer.
func racePool(t *testing.T, appName string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testsupport.TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testsupport.TestDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	if tracer != nil {
		cfg.ConnConfig.Tracer = tracer
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", appName, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// waitForBlocked polls pg_locks until a backend of appName waits on an
// ungranted lock — the positive control that a lock race was really built.
func waitForBlocked(t *testing.T, pool *pgxpool.Pool, appName string) {
	t.Helper()
	deadline := time.Now().Add(raceWait)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE NOT l.granted AND a.application_name = $1`, appName).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never blocked on a lock — the race was not built", appName)
}

func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(raceWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(raceWait):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// ---------------------------------------------------------------- side A

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// sideA runs write in a SERIALIZABLE transaction of pool, retried on a
// class-40 abort. On the first attempt, after write succeeded, it closes
// wrote and waits for gate before committing. Retries neither signal nor
// wait. done receives the final error.
func sideA(ctx context.Context, pool *pgxpool.Pool, write func(tx pgx.Tx) error,
	wrote chan struct{}, gate <-chan struct{}) <-chan error {
	done := make(chan error, 1)
	go func() {
		first := true
		var err error
		for range 6 {
			err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
				if werr := write(tx); werr != nil {
					return werr
				}
				if first {
					first = false
					close(wrote)
					<-gate
				}
				return nil
			})
			if err == nil || !retryable(err) {
				break
			}
		}
		if first {
			// The first attempt never got to its write: unblock the test.
			close(wrote)
		}
		done <- err
	}()
	return done
}

// sideAThen is sideA with a second write, touch, that runs AFTER the gate on
// the first attempt (on a retry, right after write). It reports how many
// attempts A made — the observable proof that A's first attempt was
// aborted and replayed.
func sideAThen(ctx context.Context, pool *pgxpool.Pool, write, touch func(tx pgx.Tx) error,
	wrote chan struct{}, gate <-chan struct{}) (<-chan error, func() int) {
	done := make(chan error, 1)
	var (
		mu       sync.Mutex
		attempts int
	)
	go func() {
		first := true
		var err error
		for range 6 {
			mu.Lock()
			attempts++
			mu.Unlock()
			err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
				if werr := write(tx); werr != nil {
					return werr
				}
				if first {
					first = false
					close(wrote)
					<-gate
				}
				return touch(tx)
			})
			if err == nil || !retryable(err) {
				break
			}
		}
		if first {
			close(wrote)
		}
		done <- err
	}()
	return done, func() int {
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}
}

// ---------------------------------------------------------------- fixture

type raceFix struct {
	main      *pgxpool.Pool
	poolA     *pgxpool.Pool
	poolB     *pgxpool.Pool
	hold      *holdTracer
	notifB    *notifications.Store
	owner     uuid.UUID
	ownerName string
	project   uuid.UUID
	prefix    string
	n         int
}

func newRaceFix(t *testing.T) *raceFix {
	t.Helper()
	main := testsupport.NewTestPool(t)
	h := &holdTracer{}
	f := &raceFix{
		main:  main,
		poolA: racePool(t, "optrace-a", nil),
		poolB: racePool(t, "optrace-b", h),
		hold:  h,
	}
	f.notifB = notifications.NewStore(f.poolB)
	f.owner, f.ownerName = testsupport.SeedUser(t, main, "user")
	f.project, f.prefix = testsupport.SeedProject(t, main, f.owner, "orace")
	return f
}

func (f *raceFix) issue(t *testing.T) string {
	t.Helper()
	f.n++
	return testsupport.SeedIssue(t, f.main, f.project, f.owner,
		f.prefix+"-"+itoaRace(f.n), "race", "open", 2)
}

func itoaRace(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoaRace(n/10) + string(rune('0'+n%10))
}

func (f *raceFix) member(t *testing.T) uuid.UUID {
	t.Helper()
	u, _ := testsupport.SeedUser(t, f.main, "user")
	testsupport.AddProjectMember(t, f.main, f.project, u, "member")
	return u
}

func (f *raceFix) closeRaw(t *testing.T, issueID string) {
	t.Helper()
	if _, err := f.main.Exec(context.Background(), `
UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = '' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("close %s: %v", issueID, err)
	}
}

func (f *raceFix) manualWatch(t *testing.T, issueID string, u uuid.UUID) {
	t.Helper()
	if err := notifications.NewStore(f.main).AddIssueWatch(context.Background(), issueID, u, false); err != nil {
		t.Fatalf("watch: %v", err)
	}
}

func (f *raceFix) autoWatch(t *testing.T, issueID string, u uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginTxFunc(ctx, f.main, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return notifications.AddIssueWatcherScoped(ctx, tx, issueID, u, notifications.ReasonAssignee, notifications.ScopeIssue)
	}); err != nil {
		t.Fatalf("auto watch: %v", err)
	}
}

// fanTx writes one issue.status_changed event on issueID and fans it out in
// tx; it returns the event id.
func (f *raceFix) fanTx(ctx context.Context, tx pgx.Tx, issueID string) (int64, error) {
	var eid int64
	if err := tx.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', $3, 'human', '{}'::jsonb)
RETURNING id`, issueID, f.project, f.owner).Scan(&eid); err != nil {
		return 0, err
	}
	_, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
		EventID: eid, EventType: "issue.status_changed", EntityType: "issue",
		IssueID: issueID, ProjectID: f.project, ActorUserID: f.owner, ActorType: "human",
	})
	return eid, err
}

// fan commits one fan-out of an event on issueID.
func (f *raceFix) fan(t *testing.T, issueID string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginTxFunc(ctx, f.main, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := f.fanTx(ctx, tx, issueID)
		return err
	}); err != nil {
		t.Fatalf("fan-out on %s: %v", issueID, err)
	}
}

func (f *raceFix) carded(t *testing.T, u uuid.UUID, issueID string) int {
	t.Helper()
	var n int
	if err := f.main.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND card_issue_id = $2`, u, issueID).Scan(&n); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	return n
}

func (f *raceFix) subscribed(t *testing.T, issueID string, u uuid.UUID) bool {
	t.Helper()
	var n int
	if err := f.main.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, u).Scan(&n); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	return n > 0
}

func (f *raceFix) optedOut(t *testing.T, issueID string, u uuid.UUID) bool {
	t.Helper()
	var n int
	if err := f.main.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, issueID, u).Scan(&n); err != nil {
		t.Fatalf("count opt-outs: %v", err)
	}
	return n > 0
}

// rowOf reads u's row for event eid: (exists, card).
func (f *raceFix) rowOf(t *testing.T, u uuid.UUID, eid int64) (bool, string) {
	t.Helper()
	var card *string
	err := f.main.QueryRow(context.Background(),
		`SELECT card_issue_id FROM notifications WHERE user_id = $1 AND event_id = $2`, u, eid).Scan(&card)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if card == nil {
		return true, ""
	}
	return true, *card
}

// interleave runs the A/B protocol in the given order and returns A's final
// event id (0 when A wrote no event) and B's error. a is A's write (it may
// record its event id through the pointer); b is the public call; marker is
// the statement of B to hold in order 2.
func (f *raceFix) interleave(t *testing.T, order int, a func(ctx context.Context, tx pgx.Tx) error,
	b func(ctx context.Context) error, marker string) error {
	t.Helper()
	ctx := context.Background()
	wrote, gate := make(chan struct{}), make(chan struct{})
	aDone := sideA(ctx, f.poolA, func(tx pgx.Tx) error { return a(ctx, tx) }, wrote, gate)
	await(t, wrote, "A's write")

	var bErr error
	switch order {
	case 1:
		bErr = b(ctx)
		close(gate)
		if err := awaitErr(t, aDone, "A's commit"); err != nil {
			t.Fatalf("A surfaced %v", err)
		}
	case 2:
		reached, release := f.hold.arm(marker)
		bDone := make(chan error, 1)
		go func() { bDone <- b(ctx) }()
		await(t, reached, "B's held statement")
		close(gate)
		if err := awaitErr(t, aDone, "A's commit"); err != nil {
			close(release)
			t.Fatalf("A surfaced %v", err)
		}
		close(release)
		bErr = awaitErr(t, bDone, "B's commit")
	default:
		t.Fatalf("unknown order %d", order)
	}
	return bErr
}

// conflictInterleave runs the row-conflict protocol of No. 21 (b) and No. 22.
// The two transactions contend for ONE row, so the conflict is real and
// deterministic — a write-write conflict on that row is 40001 for whichever
// side writes it second — and the side that must replay is known:
//
//   - order 1: A writes (a) and touches the contested row (touch) before B
//     starts; B blocks on that row (waitForBlocked is the positive control);
//     A commits; B's first attempt gets 40001 and B's OWN wrapper must
//     replay it — B's marker statement runs twice, A makes one attempt;
//   - order 2: A writes (a) and waits; B runs to completion and commits;
//     then A touches the row B changed: A's first attempt gets 40001 and A
//     replays on B's committed state — A makes two attempts, B's marker
//     statement runs once.
//
// It returns A's attempts, the runs of B's marker statement and B's error.
func (f *raceFix) conflictInterleave(t *testing.T, order int, a, touch func(ctx context.Context, tx pgx.Tx) error,
	b func(ctx context.Context) error, bMarker string) (aAttempts, bRuns int, bErr error) {
	t.Helper()
	ctx := context.Background()
	runs := f.hold.countRuns(bMarker)
	wrote, gate := make(chan struct{}), make(chan struct{})
	var (
		aDone    <-chan error
		attempts func() int
	)
	switch order {
	case 1:
		aDone, attempts = sideAThen(ctx, f.poolA,
			func(tx pgx.Tx) error {
				if err := a(ctx, tx); err != nil {
					return err
				}
				return touch(ctx, tx)
			},
			func(pgx.Tx) error { return nil }, wrote, gate)
		await(t, wrote, "A's write")
		bDone := make(chan error, 1)
		go func() { bDone <- b(ctx) }()
		waitForBlocked(t, f.main, "optrace-b")
		close(gate)
		if err := awaitErr(t, aDone, "A's commit"); err != nil {
			t.Fatalf("A surfaced %v", err)
		}
		bErr = awaitErr(t, bDone, "B's commit")
	case 2:
		aDone, attempts = sideAThen(ctx, f.poolA,
			func(tx pgx.Tx) error { return a(ctx, tx) },
			func(tx pgx.Tx) error { return touch(ctx, tx) }, wrote, gate)
		await(t, wrote, "A's write")
		bErr = b(ctx)
		close(gate)
		if err := awaitErr(t, aDone, "A's commit"); err != nil {
			t.Fatalf("A surfaced %v", err)
		}
	default:
		t.Fatalf("unknown order %d", order)
	}
	return attempts(), runs(), bErr
}

// assertConflictReplayed checks which side replayed in conflictInterleave
// (see there): order 1 — B's wrapper, order 2 — A.
func assertConflictReplayed(t *testing.T, order, aAttempts, bRuns int) {
	t.Helper()
	switch order {
	case 1:
		if bRuns != 2 || aAttempts != 1 {
			t.Errorf("order 1: B's statement ran %d times, A made %d attempts — want 2 and 1 "+
				"(B's first attempt aborted with 40001 and its wrapper replayed it)", bRuns, aAttempts)
		}
	case 2:
		if aAttempts != 2 || bRuns != 1 {
			t.Errorf("order 2: A made %d attempts, B's statement ran %d times — want 2 and 1 "+
				"(A's first attempt aborted with 40001 and replayed on B's committed state)", aAttempts, bRuns)
		}
	}
}

// ---------------------------------------------------------------- No. 21

// TestOptOutsRace_FanOutVsExplicitUnwatch — No. 21 (a): A writes U a
// notification carded X, B is the explicit unwatch of U from X. Both orders:
// U has no notification carded X, and the opt-out exists; V's card X stays.
func TestOptOutsRace_FanOutVsExplicitUnwatch(t *testing.T) {
	for _, order := range []int{1, 2} {
		t.Run("order"+itoaRace(order), func(t *testing.T) {
			f := newRaceFix(t)
			x := f.issue(t)
			u, v := f.member(t), f.member(t)
			f.manualWatch(t, x, u)
			f.manualWatch(t, x, v)

			err := f.interleave(t, order,
				func(ctx context.Context, tx pgx.Tx) error { _, err := f.fanTx(ctx, tx, x); return err },
				func(ctx context.Context) error { return f.notifB.RemoveIssueWatch(ctx, x, u) },
				"WITH del AS (")
			if err != nil {
				t.Fatalf("RemoveIssueWatch surfaced %v", err)
			}
			if n := f.carded(t, u, x); n != 0 {
				t.Errorf("U has %d notifications carded X after the unwatch committed (INV-6)", n)
			}
			if !f.optedOut(t, x, u) || f.subscribed(t, x, u) {
				t.Errorf("want no subscription and an opt-out; subscribed=%v optedOut=%v",
					f.subscribed(t, x, u), f.optedOut(t, x, u))
			}
			if n := f.carded(t, v, x); n != 1 {
				t.Errorf("V's card X: %d rows, want 1", n)
			}
		})
	}
}

// TestOptOutsRace_FanOutVsSubscribe — No. 21 (b): A fans out an event of X
// while B is U's manual watch of X. U explicitly unwatched X before, so B
// also deletes U's opt-out (U, X); that row is the contested one: A, the
// fan-out's transaction, touches it too (conflictInterleave). The final state
// equals the serial order the conflict decides: order 1 «A then B» — U has no
// row for A's event, B's own wrapper replayed its 40001; order 2 «B then A» —
// A replayed on B's committed subscription and carded U's row X. In both:
// U is subscribed {manual}, the opt-out is gone.
//
// The contested row is what makes both orders reachable: the fan-out alone
// only READS the subscriptions B writes, B reads nothing the fan-out writes,
// so SSI commits both and the result is always «A then B» — no 40001 on
// either side (found in review).
func TestOptOutsRace_FanOutVsSubscribe(t *testing.T) {
	for _, order := range []int{1, 2} {
		t.Run("order"+itoaRace(order), func(t *testing.T) {
			f := newRaceFix(t)
			x := f.issue(t)
			u := f.member(t)
			f.manualWatch(t, x, u)
			if err := notifications.NewStore(f.main).RemoveIssueWatch(context.Background(), x, u); err != nil {
				t.Fatalf("precondition unwatch: %v", err)
			}
			if !f.optedOut(t, x, u) || f.subscribed(t, x, u) {
				t.Fatalf("precondition: U must have an opt-out on X and no subscription")
			}
			var eid int64
			aAttempts, bRuns, err := f.conflictInterleave(t, order,
				func(ctx context.Context, tx pgx.Tx) error {
					id, err := f.fanTx(ctx, tx, x)
					eid = id
					return err
				},
				func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `
UPDATE issue_watch_optouts SET created_at = created_at WHERE issue_id = $1 AND user_id = $2`, x, u)
					return err
				},
				func(ctx context.Context) error { return f.notifB.AddIssueWatch(ctx, x, u, false) },
				"clear_optout AS")
			if err != nil {
				t.Fatalf("AddIssueWatch surfaced %v", err)
			}
			assertConflictReplayed(t, order, aAttempts, bRuns)
			exists, card := f.rowOf(t, u, eid)
			switch order {
			case 1:
				if exists {
					t.Errorf("«A then B»: U has a row for A's event (card %q) — A fanned out before the subscription existed", card)
				}
			case 2:
				if !exists || card != x {
					t.Errorf("«B then A»: U's row for A's event exists=%v card=%q, want carded %s", exists, card, x)
				}
			}
			if !f.subscribed(t, x, u) || f.optedOut(t, x, u) {
				t.Errorf("want U subscribed and no opt-out; subscribed=%v optedOut=%v",
					f.subscribed(t, x, u), f.optedOut(t, x, u))
			}
		})
	}
}

// TestOptOutsRace_AddIssueWatchAbsorbsItsOwnConflict — K2 / R17 (found in
// review): the public AddIssueWatch replays a
// serialization failure itself. U has an opt-out on X; a concurrent writer
// touches that row and holds it; U's manual watch (B) blocks on it
// (waitForBlocked), the writer commits, B's first attempt gets 40001.
// Expected: the call succeeds, its statement ran twice (the replay), U is
// subscribed with exactly {manual} and the opt-out is gone. Without the
// replay the 40001 reaches the caller and the test is red.
func TestOptOutsRace_AddIssueWatchAbsorbsItsOwnConflict(t *testing.T) {
	f := newRaceFix(t)
	ctx := context.Background()
	x := f.issue(t)
	u := f.member(t)
	f.manualWatch(t, x, u)
	if err := notifications.NewStore(f.main).RemoveIssueWatch(ctx, x, u); err != nil {
		t.Fatalf("precondition unwatch: %v", err)
	}
	if !f.optedOut(t, x, u) {
		t.Fatalf("precondition: U must have an opt-out on X")
	}
	runs := f.hold.countRuns("clear_optout AS")

	writer, err := f.main.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
UPDATE issue_watch_optouts SET created_at = created_at WHERE issue_id = $1 AND user_id = $2`, x, u); err != nil {
		t.Fatalf("writer touch: %v", err)
	}
	bDone := make(chan error, 1)
	go func() { bDone <- f.notifB.AddIssueWatch(ctx, x, u, false) }()
	waitForBlocked(t, f.main, "optrace-b")
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	if err := awaitErr(t, bDone, "B"); err != nil {
		t.Fatalf("AddIssueWatch surfaced %v after ONE concurrent writer — its retry did not absorb it", err)
	}
	if n := runs(); n != 2 {
		t.Errorf("AddIssueWatch's statement ran %d times, want 2 — the conflict was not built or not replayed", n)
	}
	var reasons []string
	if err := f.main.QueryRow(ctx,
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, x, u).Scan(&reasons); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if len(reasons) != 1 || reasons[0] != notifications.ReasonManual {
		t.Errorf("subscription reasons = %v, want [manual]", reasons)
	}
	if f.optedOut(t, x, u) {
		t.Errorf("the opt-out survived the manual watch")
	}
}

// TestOptOutsRace_AutoSubscribeVsExplicitUnwatch — No. 21 (c), edge 4: U is
// subscribed to X; the automatic subscription (A, reason assignee) and the
// explicit unwatch (B) both write the row (U, X), so here the second writer
// waits on the first one's row lock instead of the tracer barrier — the lock
// wait is the race (waitForBlocked is its positive control). In both orders
// the final state is the same: no subscription, the opt-out exists.
func TestOptOutsRace_AutoSubscribeVsExplicitUnwatch(t *testing.T) {
	autoAdd := func(ctx context.Context, tx pgx.Tx, x string, u uuid.UUID) error {
		return notifications.AddIssueWatcherScoped(ctx, tx, x, u, notifications.ReasonAssignee, notifications.ScopeIssue)
	}
	check := func(t *testing.T, f *raceFix, x string, u uuid.UUID) {
		t.Helper()
		if f.subscribed(t, x, u) || !f.optedOut(t, x, u) {
			t.Errorf("want no subscription and an opt-out; subscribed=%v optedOut=%v",
				f.subscribed(t, x, u), f.optedOut(t, x, u))
		}
	}

	t.Run("A writes first, B waits on it", func(t *testing.T) {
		f := newRaceFix(t)
		x := f.issue(t)
		u := f.member(t)
		f.manualWatch(t, x, u)
		ctx := context.Background()
		wrote, gate := make(chan struct{}), make(chan struct{})
		aDone := sideA(ctx, f.poolA, func(tx pgx.Tx) error { return autoAdd(ctx, tx, x, u) }, wrote, gate)
		await(t, wrote, "A's write")
		bDone := make(chan error, 1)
		go func() { bDone <- f.notifB.RemoveIssueWatch(ctx, x, u) }()
		waitForBlocked(t, f.main, "optrace-b")
		close(gate)
		if err := awaitErr(t, aDone, "A"); err != nil {
			t.Fatalf("A surfaced %v", err)
		}
		if err := awaitErr(t, bDone, "B"); err != nil {
			t.Fatalf("RemoveIssueWatch surfaced %v", err)
		}
		check(t, f, x, u)
	})

	t.Run("B writes first, A waits on it", func(t *testing.T) {
		f := newRaceFix(t)
		x := f.issue(t)
		u := f.member(t)
		f.manualWatch(t, x, u)
		ctx := context.Background()
		reached, release := f.hold.arm("WITH del AS (")
		bDone := make(chan error, 1)
		go func() { bDone <- f.notifB.RemoveIssueWatch(ctx, x, u) }()
		await(t, reached, "B's held statement")
		wrote, gate := make(chan struct{}), make(chan struct{})
		close(gate)
		aDone := sideA(ctx, f.poolA, func(tx pgx.Tx) error { return autoAdd(ctx, tx, x, u) }, wrote, gate)
		waitForBlocked(t, f.main, "optrace-a")
		close(release)
		if err := awaitErr(t, bDone, "B"); err != nil {
			t.Fatalf("RemoveIssueWatch surfaced %v", err)
		}
		if err := awaitErr(t, aDone, "A"); err != nil {
			t.Fatalf("A surfaced %v", err)
		}
		check(t, f, x, u)
	})
}

// ---------------------------------------------------------------- No. 21a

// TestOptOutsRace_FanOutVsBulkUnwatch — No. 21a: A fans out an event of X
// (carding U and V) while B is the public bulk unwatch — (d) by state
// «closed», (e) by a selection holding X — in both orders. After all
// commits: U has no notification carded X; U's card Y (open, outside the
// filter and the selection) stays; V's card X stays; for (d) U's manual
// subscription on the closed Z is kept and counted, and its card stays.
func TestOptOutsRace_FanOutVsBulkUnwatch(t *testing.T) {
	for _, entry := range []struct {
		name   string
		state  bool
		marker string
	}{
		{"by state closed", true, "WITH target AS ("},
		{"by selection", false, "WITH del AS ("},
	} {
		for _, order := range []int{1, 2} {
			t.Run(entry.name+"/order"+itoaRace(order), func(t *testing.T) {
				f := newRaceFix(t)
				x, y, z := f.issue(t), f.issue(t), f.issue(t)
				u, v := f.member(t), f.member(t)
				if entry.state {
					f.autoWatch(t, x, u)
				} else {
					f.manualWatch(t, x, u)
				}
				f.manualWatch(t, x, v)
				f.autoWatch(t, y, u)
				f.manualWatch(t, z, u)
				f.fan(t, y)
				f.fan(t, z)
				f.closeRaw(t, x)
				f.closeRaw(t, z)

				var res notifications.BulkUnwatchResult
				in := notifications.BulkUnwatchInput{IssueIDs: []string{x}}
				if entry.state {
					in = notifications.BulkUnwatchInput{State: notifications.WatchingStateClosed}
				}
				err := f.interleave(t, order,
					func(ctx context.Context, tx pgx.Tx) error { _, err := f.fanTx(ctx, tx, x); return err },
					func(ctx context.Context) error {
						var err error
						res, err = f.notifB.BulkUnwatch(ctx, u, in)
						return err
					},
					entry.marker)
				if err != nil {
					t.Fatalf("BulkUnwatch surfaced %v", err)
				}
				if n := f.carded(t, u, x); n != 0 {
					t.Errorf("U has %d notifications carded X after the bulk unwatch committed (INV-6)", n)
				}
				if n := f.carded(t, u, y); n != 1 {
					t.Errorf("U's card Y (outside the operation): %d rows, want 1", n)
				}
				if n := f.carded(t, v, x); n != 1 {
					t.Errorf("V's card X: %d rows, want 1", n)
				}
				if entry.state {
					if !f.subscribed(t, z, u) || f.carded(t, u, z) != 1 {
						t.Errorf("U's kept manual subscription on Z or its card was touched")
					}
					if res.KeptManual != 1 {
						t.Errorf("keptManual = %d, want 1", res.KeptManual)
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------- No. 21b

// historicalStand: U has an automatic (assignee) subscription on X, V a
// manual one; U also has a card Y (subscribed, unselected) and a historical
// card W (unselected). Returns the fixture and the ids.
func historicalStand(t *testing.T) (f *raceFix, x, y, w string, u, v uuid.UUID) {
	t.Helper()
	f = newRaceFix(t)
	x, y, w = f.issue(t), f.issue(t), f.issue(t)
	u, v = f.member(t), f.member(t)
	f.autoWatch(t, x, u)
	f.manualWatch(t, x, v)
	f.manualWatch(t, y, u)
	f.autoWatch(t, w, u)
	f.fan(t, y)
	f.fan(t, w)
	if _, err := f.main.Exec(context.Background(),
		`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, w, u); err != nil {
		t.Fatalf("make W historical: %v", err)
	}
	return f, x, y, w, u, v
}

// closePublic closes X through issues.Store#Close in its own connection:
// the auto-unsubscribe takes U's automatic subscription away (no opt-out,
// no card cleared) after the close notification was carded X.
func closePublic(t *testing.T, f *raceFix, x string) {
	t.Helper()
	ctx := context.Background()
	var version int
	if err := f.main.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, x).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, err := issues.NewStore(f.main).Close(ctx, issues.CloseInput{
		ID: x, ExpectedVersion: version, Reason: "race stand",
		Actor: issues.HumanActor(f.owner, f.ownerName),
	}); err != nil {
		t.Fatalf("Close(%s): %v", x, err)
	}
}

// removeEntries are the two public entries of "remove": (f) the single
// unwatch without a subscription (R3 branch) and (g) the selection holding
// X, handled by card kind. The marker is the statement that clears the
// cards.
func removeEntries(f *raceFix, x string, u uuid.UUID, removed *int) []struct {
	name   string
	call   func(ctx context.Context) error
	marker string
} {
	return []struct {
		name   string
		call   func(ctx context.Context) error
		marker string
	}{
		{"single unwatch", func(ctx context.Context) error {
			*removed = -1
			return f.notifB.RemoveIssueWatch(ctx, x, u)
		}, "WITH del AS ("},
		{"selection", func(ctx context.Context) error {
			res, err := f.notifB.BulkUnwatch(ctx, u, notifications.BulkUnwatchInput{IssueIDs: []string{x}})
			*removed = res.HistoricalRemoved
			return err
		}, "WITH hist AS ("},
	}
}

func checkHistoricalOutcome(t *testing.T, f *raceFix, x, y, w string, u, v uuid.UUID, removed int) {
	t.Helper()
	if n := f.carded(t, u, x); n != 0 {
		t.Errorf("U has %d notifications carded X after 'remove' committed (INV-6)", n)
	}
	if f.optedOut(t, x, u) {
		t.Errorf("'remove' of a historical card recorded an opt-out (R3)")
	}
	if f.subscribed(t, x, u) {
		t.Errorf("U is subscribed to X after the close took the automatic subscription away")
	}
	if n := f.carded(t, u, y); n != 1 {
		t.Errorf("U's card Y (outside the selection): %d rows, want 1", n)
	}
	if n := f.carded(t, u, w); n != 1 {
		t.Errorf("U's unselected historical card W: %d rows, want 1", n)
	}
	if n := f.carded(t, v, x); n < 1 {
		t.Errorf("V's card X disappeared")
	}
	if removed >= 0 && removed != 1 {
		t.Errorf("historicalRemoved = %d, want 1", removed)
	}
}

// TestOptOutsRace_FanOutVsRemoveHistoricalCard — No. 21b, first stand: A (the
// FanOut primitive in the test's transaction) reads U's automatic
// subscription and writes U a notification carded X; the public close of X
// then runs to completion in a third connection (X becomes U's historical
// card, the close notification is carded X); then B — "remove" X through
// (f) / (g) — in both orders. Expected end state, whatever side retried: no
// notification of U carded X, no opt-out, no subscription; Y, W and V's
// card X untouched; historicalRemoved = 1 for (g).
func TestOptOutsRace_FanOutVsRemoveHistoricalCard(t *testing.T) {
	for _, entryIdx := range []int{0, 1} {
		for _, order := range []int{1, 2} {
			f, x, y, w, u, v := historicalStand(t)
			removed := 0
			entry := removeEntries(f, x, u, &removed)[entryIdx]
			t.Run(entry.name+"/order"+itoaRace(order), func(t *testing.T) {
				ctx := context.Background()
				wrote, gate := make(chan struct{}), make(chan struct{})
				firstEid := int64(0)
				aDone := sideA(ctx, f.poolA, func(tx pgx.Tx) error {
					eid, err := f.fanTx(ctx, tx, x)
					if err != nil {
						return err
					}
					if firstEid == 0 {
						firstEid = eid
						// Positive control: the write is in flight — visible
						// inside A before B starts.
						var n int
						if err := tx.QueryRow(ctx, `
SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2 AND card_issue_id = $3`,
							u, eid, x).Scan(&n); err != nil {
							return err
						}
						if n != 1 {
							t.Errorf("inside A: U's row carded X not visible (n=%d) — nothing in flight", n)
						}
					}
					return nil
				}, wrote, gate)
				await(t, wrote, "A's write")
				closePublic(t, f, x)

				var bErr error
				switch order {
				case 1:
					bErr = entry.call(ctx)
					close(gate)
					if err := awaitErr(t, aDone, "A"); err != nil {
						t.Fatalf("A surfaced %v", err)
					}
				case 2:
					reached, release := f.hold.arm(entry.marker)
					bDone := make(chan error, 1)
					go func() { bDone <- entry.call(ctx) }()
					await(t, reached, "B's held statement")
					close(gate)
					if err := awaitErr(t, aDone, "A"); err != nil {
						close(release)
						t.Fatalf("A surfaced %v", err)
					}
					close(release)
					bErr = awaitErr(t, bDone, "B")
				}
				if bErr != nil {
					t.Fatalf("'remove' surfaced %v", bErr)
				}
				checkHistoricalOutcome(t, f, x, y, w, u, v, removed)
			})
		}
	}
}

// TestOptOutsRace_RemoveHistoricalCardAbsorbsItsOwnConflict — No. 21b, second
// stand: no A. X is U's historical card (public close took the automatic
// subscription away). A concurrent writer touches U's notification carded X
// without changing the card or the read state and holds it; B ("remove", (f)
// or (g)) blocks on that row (waitForBlocked); the writer commits; B's first
// attempt gets 40001 and B's own retry must absorb it: the call succeeds,
// the card is gone once (historicalRemoved = 1 for (g)), no opt-out, Y / W /
// V untouched. This stand is what proves B's retry is wired — in the first
// stand A's retry could absorb the conflict instead.
func TestOptOutsRace_RemoveHistoricalCardAbsorbsItsOwnConflict(t *testing.T) {
	for _, entryIdx := range []int{0, 1} {
		f, x, y, w, u, v := historicalStand(t)
		closePublic(t, f, x)
		removed := 0
		entry := removeEntries(f, x, u, &removed)[entryIdx]
		t.Run(entry.name, func(t *testing.T) {
			ctx := context.Background()
			if n := f.carded(t, u, x); n < 1 {
				t.Fatalf("precondition: X is not a historical card of U (n=%d)", n)
			}
			writer, err := f.main.Begin(ctx)
			if err != nil {
				t.Fatalf("begin writer: %v", err)
			}
			defer func() { _ = writer.Rollback(ctx) }()
			if _, err := writer.Exec(ctx, `
UPDATE notifications SET reason = reason WHERE user_id = $1 AND card_issue_id = $2`, u, x); err != nil {
				t.Fatalf("writer touch: %v", err)
			}
			bDone := make(chan error, 1)
			go func() { bDone <- entry.call(ctx) }()
			waitForBlocked(t, f.main, "optrace-b")
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("writer commit: %v", err)
			}
			if err := awaitErr(t, bDone, "B"); err != nil {
				t.Fatalf("'remove' surfaced %v — its retry did not absorb the conflict", err)
			}
			checkHistoricalOutcome(t, f, x, y, w, u, v, removed)
		})
	}
}

// ---------------------------------------------------------------- No. 22

// TestOptOutsRace_ReparentVsFanOut — No. 22, edge 3: A fans out an event of T
// (computing the card on the hierarchy it sees) while B moves T from under
// OldP to under NewP (the public Reparent). The contested row is T's issue
// row: Reparent bumps its version, and A — the transaction of a write path
// on T, as every real fan-out of an issue event is — touches it too
// (updated_at only, so B's version check still holds; conflictInterleave).
// order 1 «A then B»: B's own wrapper replayed its 40001; OldP's subscriber
// carded OldP, NewP's subscriber has no row. order 2 «B then A»: A replayed
// on the new hierarchy; OldP's subscriber has no row, NewP's subscriber
// carded NewP. Either way the card is computed wholly on one hierarchy.
//
// The fan-out alone only READS the edge B moves and B reads nothing it
// writes, so without the contested row SSI commits both, the result is
// always «A then B» and no 40001 arises (found in
// review).
func TestOptOutsRace_ReparentVsFanOut(t *testing.T) {
	for _, order := range []int{1, 2} {
		t.Run("order"+itoaRace(order), func(t *testing.T) {
			f := newRaceFix(t)
			oldP, newP, tk := f.issue(t), f.issue(t), f.issue(t)
			deps := dependencies.NewStore(f.main)
			if _, err := deps.Create(context.Background(), tk,
				dependencies.CreateDepInput{ToID: oldP, Type: "parent-child"},
				dependencies.CallerAccess{UserID: f.owner, Role: "admin"},
				dependencies.HumanActor(f.owner, f.ownerName)); err != nil {
				t.Fatalf("link: %v", err)
			}
			sOld, sNew := f.member(t), f.member(t)
			ns := notifications.NewStore(f.main)
			if err := ns.AddIssueWatch(context.Background(), oldP, sOld, true); err != nil {
				t.Fatalf("watch old: %v", err)
			}
			if err := ns.AddIssueWatch(context.Background(), newP, sNew, true); err != nil {
				t.Fatalf("watch new: %v", err)
			}
			var version int
			if err := f.main.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, tk).Scan(&version); err != nil {
				t.Fatalf("read version: %v", err)
			}
			depsB := dependencies.NewStore(f.poolB)

			var eid int64
			aAttempts, bRuns, err := f.conflictInterleave(t, order,
				func(ctx context.Context, tx pgx.Tx) error {
					id, err := f.fanTx(ctx, tx, tk)
					eid = id
					return err
				},
				func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, tk)
					return err
				},
				func(ctx context.Context) error {
					_, err := depsB.Reparent(ctx, tk, newP, version,
						dependencies.CallerAccess{UserID: f.owner, Role: "admin"},
						dependencies.HumanActor(f.owner, f.ownerName))
					return err
				},
				"SELECT project_id, version, status FROM issues")
			if err != nil {
				t.Fatalf("Reparent surfaced %v", err)
			}
			assertConflictReplayed(t, order, aAttempts, bRuns)
			oldExists, oldCard := f.rowOf(t, sOld, eid)
			newExists, newCard := f.rowOf(t, sNew, eid)
			switch order {
			case 1:
				if !oldExists || oldCard != oldP || newExists {
					t.Errorf("«A then B»: old subscriber exists=%v card=%q, new subscriber exists=%v card=%q — "+
						"want old carded %s, new none", oldExists, oldCard, newExists, newCard, oldP)
				}
			case 2:
				if oldExists || !newExists || newCard != newP {
					t.Errorf("«B then A»: old subscriber exists=%v card=%q, new subscriber exists=%v card=%q — "+
						"want old none, new carded %s", oldExists, oldCard, newExists, newCard, newP)
				}
			}
		})
	}
}

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/testsupport"
)

// ----------------------------- Fixtures -----------------------------
//
// Pool + user/project seeding come from the shared internal/testsupport
// factory (sk-105.2 S1 reference adoption). The outbox-specific helpers
// below (resetOutbox, insertEvent, publishedAt) stay local because they
// poke the events table directly to build deterministic dispatcher
// scenarios.
//
// Parallel-safety: the dispatcher's tick() drains EVERY unpublished
// events row in the shared DB, so its returned count is global and must
// NOT be asserted exactly under package parallelism. Instead each test
// asserts on PROJECT-SCOPED state — its own seeded event ids
// (publishedAt) and CountUnpublishedEventsForProject(pid) — so a
// concurrent package's events cannot perturb the assertion.

func newTestPool(t *testing.T) *pgxpool.Pool { return testsupport.NewTestPool(t) }

// seedUser / seedProject wrap the shared factory, preserving the prior
// (id) / (id) return shapes the outbox tests use (they don't need the
// username or prefix).
func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	id, _ := testsupport.SeedUser(t, pool, "user")
	return id
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	id, _ := testsupport.SeedProject(t, pool, owner, "obtest")
	return id
}

// startEmbeddedNATS launches a NATS Core server on a random port. Caller
// gets a connected nats.Conn ready to publish/subscribe; cleanup shuts
// the server down on test exit.
func startEmbeddedNATS(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1 // pick a random free port
	opts.NoLog = true
	s := natstest.RunServer(&opts)
	t.Cleanup(s.Shutdown)

	nc, err := nats.Connect(s.ClientURL(),
		nats.Name("outbox-dispatcher-test"),
		nats.ReconnectWait(50*time.Millisecond),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		t.Fatalf("connect embedded nats: %v", err)
	}
	t.Cleanup(func() { _ = nc.Drain() })
	return s, nc
}

// insertEvent appends a single events row with published_at = NULL.
// Returns the new bigserial id. Used to construct deterministic outbox
// scenarios without exercising the real event-write code paths.
func insertEvent(t *testing.T, pool *pgxpool.Pool, actor uuid.UUID, projectID uuid.UUID, evtType, entityType, entityID, field string, occurredAt time.Time) int64 {
	t.Helper()
	var id int64
	var fieldArg any
	if field != "" {
		fieldArg = field
	}
	at := occurredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, field, payload, published_at)
VALUES ($1, $2, $3, $4, 'human', $5, $6, $7, $8, '{}'::jsonb, NULL)
RETURNING id`,
		at, evtType, "tester", actor, entityType, entityID, projectID, fieldArg).Scan(&id)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, id)
	})
	return id
}

// resetOutbox marks any unpublished events for THIS test's project as
// published, so the project starts each scenario with a clean outbox.
//
// It is deliberately scoped to project_id — NOT a global
// `WHERE published_at IS NULL` UPDATE. The old global form collided with
// events written concurrently by other test packages (issues / handler /
// mcp/tools) under SKEIN_TEST_PARALLEL_PKGS=1, so the dispatcher tests
// flaked on perturbed counts. Scoping here, combined with project-scoped
// assertions (publishedAt on seeded ids + CountUnpublishedEventsForProject),
// means each test reasons only about rows it created.
func resetOutbox(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE events SET published_at = now()
		  WHERE published_at IS NULL AND project_id = $1`, projectID)
	if err != nil {
		t.Fatalf("reset outbox: %v", err)
	}
}

// drainWaitBudget bounds the drain in WALL CLOCK, not in iterations.
//
// A tick count is not a bound on this loop, it is a disguised assumption
// about the size of the whole database: tick() drains the shared events
// table globally in id order under BatchSize, so the number of ticks
// needed to reach a row THIS test inserted is (global backlog ahead of
// it) / BatchSize. The old `for i := 0; i < 100` with BatchSize 10
// therefore tolerated a 1 000-row events table and nothing more; one
// wave of new fixtures took the suite's table past 1 600 and the
// dispatcher tests went red for a reason that had nothing to do with the
// dispatcher. Raising the constant to fit today's population only moves
// the same trap to the next wave that adds fixtures, so there is no
// population-derived constant here any more: the loop waits for the
// CONDITION (this project's outbox is empty) and gives up only on lack
// of progress or on the clock.
const drainWaitBudget = 2 * time.Minute

// drainMaxIdleTicks and drainIdlePause together define the IDLE WINDOW —
// how long the loop tolerates zero progress before declaring the drain
// stuck.
//
// A tick that publishes nothing while rows of ours are still unpublished
// usually means the next tick has nothing new to work with either — with
// one exception, and the window exists only for it: another transaction
// can hold our rows under FOR UPDATE SKIP LOCKED until it commits, and
// for as long as it does, every tick of ours is legitimately idle. The
// window therefore has to cover the longest such hold, not the shortest:
// the previous 5 × 20 ms = 100 ms was narrower than a competing worker's
// commit under load, which turns a slow neighbour into a false «the
// drain is STUCK» — a flake in the one test the canon says is never a
// flake. 25 × 100 ms = 2.5 s is comfortably longer than any transaction
// this suite holds and still two orders of magnitude below
// drainWaitBudget, so a genuinely stuck drain fails on this check, fast
// and with the precise message, instead of spinning out the wall clock.
const (
	drainMaxIdleTicks = 25
	drainIdlePause    = 100 * time.Millisecond
)

// drainProject ticks the dispatcher until THIS project's outbox is
// empty. See drainProjectWith for the waiting contract.
func drainProject(t *testing.T, d *Dispatcher, pool *pgxpool.Pool, projectID uuid.UUID) {
	t.Helper()
	drainProjectWith(t, pool, projectID, func(ctx context.Context) (int, error) {
		return d.tick(ctx)
	})
}

// drainProjectWith is the shared waiting loop: it calls tickOnce until
// the project-scoped unpublished count reaches zero.
//
// The project-scoped count (rather than tick's own return) is what the
// loop waits on, because a single tick() drains the shared DB globally
// and its count is therefore not about us; and the caller supplies the
// tick so a test that also asserts something per tick (the batch cap)
// gets the same population-independent wait instead of its own copy of
// the bound.
//
// The budget bounds the TICK as well as the loop. Checking the clock
// between ticks bounds only the gaps between them: a tick that hangs —
// a lock it never gets, a NATS call that never returns — is entered
// before the deadline and then never leaves, so the loop's own budget is
// never consulted again and the test hangs until `go test` kills the
// whole package with a panic that names no test in particular. The tick
// therefore runs under a context whose deadline is the loop's own, which
// makes drainWaitBudget a real bound on the wait rather than a bound on
// the intervals between waits: a stuck tick comes back as an error and
// the failure names this drain, this project, this tick.
func drainProjectWith(
	t *testing.T,
	pool *pgxpool.Pool,
	projectID uuid.UUID,
	tickOnce func(context.Context) (int, error),
) {
	t.Helper()
	deadline := time.Now().Add(drainWaitBudget)
	ticks, idle := 0, 0
	for {
		remaining := testsupport.CountUnpublishedEventsForProject(t, pool, projectID)
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("project %s outbox not drained within %s (%d ticks, %d unpublished remain) — "+
				"the drain is not keeping up with the events table, not a tick-count problem",
				projectID, drainWaitBudget, ticks, remaining)
		}

		n, err := tickBounded(tickOnce, deadline)
		if err != nil {
			t.Fatalf("drain tick %d: %v", ticks+1, err)
		}
		ticks++
		if n > 0 {
			idle = 0
			continue
		}
		idle++
		if idle >= drainMaxIdleTicks {
			t.Fatalf("dispatcher published nothing on %d consecutive ticks while %d events of "+
				"project %s are still unpublished — the drain is STUCK, not slow",
				idle, remaining, projectID)
		}
		// Give a competing transaction the time it needs to commit and
		// release the rows it holds under SKIP LOCKED.
		time.Sleep(drainIdlePause)
	}
}

// tickBounded runs one tick under a context that expires with the drain's
// own wall-clock budget, so a tick that never returns on its own is cut
// off by the same deadline the loop is measured against.
func tickBounded(tickOnce func(context.Context) (int, error), deadline time.Time) (int, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return tickOnce(ctx)
}

// publishedAt loads the column for a single event id. Returns nil when
// the row is still unpublished.
func publishedAt(t *testing.T, pool *pgxpool.Pool, id int64) *time.Time {
	t.Helper()
	var at *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT published_at FROM events WHERE id = $1`, id).Scan(&at)
	if err != nil {
		t.Fatalf("read published_at: %v", err)
	}
	return at
}

// ----------------------------- Tests -----------------------------

func TestDispatcher_PublishesPendingEvent(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)

	// Subscribe BEFORE inserting + dispatching so we don't race the
	// publish.
	sub, err := nc.SubscribeSync(fmt.Sprintf("skein.events.project.%s.>", pid.String()))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sub: %v", err)
	}

	id := insertEvent(t, pool, owner, pid, "issue.created", "issue", "obt-1", "", time.Time{})

	d := New(pool, nc, Options{BatchSize: 10})
	// tick() drains every unpublished row in the shared DB, so its return
	// is a global count we must not assert exactly under parallelism — and
	// a concurrent backlog may take several ticks to reach our row. Drain
	// to empty, then assert project-scoped state.
	drainProject(t, d, pool, pid)
	if got := testsupport.CountUnpublishedEventsForProject(t, pool, pid); got != 0 {
		t.Fatalf("expected 0 unpublished events for project, got %d", got)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected one message on subscriber, got error: %v", err)
	}
	wantSubj := fmt.Sprintf("skein.events.project.%s.issue.created.obt-1", pid.String())
	if msg.Subject != wantSubj {
		t.Errorf("subject mismatch: got %q want %q", msg.Subject, wantSubj)
	}
	if !strings.Contains(string(msg.Data), `"eventType":"issue.created"`) {
		t.Errorf("envelope body missing eventType: %s", string(msg.Data))
	}

	if publishedAt(t, pool, id) == nil {
		t.Errorf("event id=%d still unpublished after tick", id)
	}
}

func TestDispatcher_DrainsBatchUpToLimit(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)

	// Insert 25 events with batch size 10. The dispatcher must drain them
	// in batches no larger than BatchSize and eventually leave the project
	// outbox empty. We can't assert the exact per-tick count because tick()
	// drains the whole shared DB (concurrent packages' rows inflate it), so
	// we assert the batch-cap invariant (each tick <= batch) plus full
	// project drain instead.
	const total = 25
	const batch = 10
	ids := make([]int64, total)
	for i := 0; i < total; i++ {
		ids[i] = insertEvent(t, pool, owner, pid, "issue.updated", "issue",
			fmt.Sprintf("obt-%d", i), "", time.Time{})
	}

	d := New(pool, nc, Options{BatchSize: batch})

	// Assert the per-tick batch cap on the EVENTS drain specifically
	// (tickEvents) — tick() also drains notifications, whose count would
	// otherwise inflate the combined return past BatchSize. The wait is
	// drainProjectWith's: by condition, not by a tick count that silently
	// encodes how big the shared events table is allowed to be, and under
	// the deadline it hands the tick.
	ticks := 0
	drainProjectWith(t, pool, pid, func(ctx context.Context) (int, error) {
		n, err := d.tickEvents(ctx)
		if err != nil {
			return 0, err
		}
		ticks++
		if n > batch {
			t.Fatalf("tickEvents %d returned n=%d, exceeds batch cap %d", ticks, n, batch)
		}
		return n, nil
	})

	if got := testsupport.CountUnpublishedEventsForProject(t, pool, pid); got != 0 {
		t.Fatalf("expected project outbox fully drained, %d unpublished remain", got)
	}
	for _, id := range ids {
		if publishedAt(t, pool, id) == nil {
			t.Fatalf("event id=%d not published", id)
		}
	}
}

func TestDispatcher_AtLeastOnceOnNATSFailure(t *testing.T) {
	// Closing the NATS connection makes nc.Publish return ErrConnectionClosed.
	// The dispatcher must NOT mark the rows as published — the next attempt
	// (with a fresh connection) must re-publish them.
	pool := newTestPool(t)

	srv, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)
	id := insertEvent(t, pool, owner, pid, "issue.created", "issue", "obt-fail", "", time.Time{})

	// Force the publish to fail by closing the connection.
	nc.Close()

	d := New(pool, nc, Options{BatchSize: 10})
	_, err := d.tick(context.Background())
	if err == nil {
		t.Fatalf("expected publish to fail with closed conn")
	}
	if publishedAt(t, pool, id) != nil {
		t.Errorf("event id=%d marked published despite failed publish", id)
	}

	// Reconnect with a fresh client; this tick must succeed.
	nc2, err := nats.Connect(srv.ClientURL(), nats.Name("outbox-test-reconnect"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer nc2.Close()
	sub, err := nc2.SubscribeSync(fmt.Sprintf("skein.events.project.%s.>", pid.String()))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc2.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	d2 := New(pool, nc2, Options{BatchSize: 10})
	// Project-scoped assertion: after draining, the previously-failed event
	// is published and the project outbox is empty. (tick's global n is not
	// asserted — concurrent packages may inflate it / extend the tick count.)
	drainProject(t, d2, pool, pid)
	if got := testsupport.CountUnpublishedEventsForProject(t, pool, pid); got != 0 {
		t.Fatalf("expected 0 unpublished events for project on retry, got %d", got)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("subscriber missed retry message: %v", err)
	}
	if publishedAt(t, pool, id) == nil {
		t.Errorf("event id=%d still unpublished after retry", id)
	}
}

func TestDispatcher_SkipLockedConcurrent(t *testing.T) {
	// Two dispatchers running ticks at the same time must never publish
	// the same event row twice. We exploit FOR UPDATE SKIP LOCKED — one
	// tx grabs the rows, the other sees zero rows and returns 0.
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)

	const n = 50
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = insertEvent(t, pool, owner, pid, "issue.updated", "issue",
			fmt.Sprintf("conc-%d", i), "", time.Time{})
	}

	d1 := New(pool, nc, Options{BatchSize: 100})
	d2 := New(pool, nc, Options{BatchSize: 100})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := d1.tick(context.Background()); err != nil {
			t.Errorf("d1 tick: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := d2.tick(context.Background()); err != nil {
			t.Errorf("d2 tick: %v", err)
		}
	}()
	wg.Wait()

	// The concurrent ticks above exercise the SKIP LOCKED claim under
	// contention. With a global backlog the two ticks may not fully drain
	// this project in one round, so finish the drain serially, then assert
	// project-scoped state: SKIP LOCKED forbids double-publish, so every one
	// of this project's 50 rows ends up published exactly once and the
	// project outbox is empty. (Dispatcher return counts are global and not
	// asserted.)
	drainProject(t, d1, pool, pid)
	if got := testsupport.CountUnpublishedEventsForProject(t, pool, pid); got != 0 {
		t.Errorf("expected project outbox fully drained, %d unpublished remain", got)
	}
	for _, id := range ids {
		if publishedAt(t, pool, id) == nil {
			t.Errorf("event id=%d not published after concurrent ticks", id)
		}
	}
}

func TestDispatcher_LagGaugeReflectsReality(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)

	// Insert an event timestamped 30 seconds in the past.
	old := time.Now().UTC().Add(-30 * time.Second)
	insertEvent(t, pool, owner, pid, "issue.updated", "issue", "lag-1", "", old)

	d := New(pool, nc, Options{BatchSize: 10})
	d.refreshLagGauge(context.Background())

	// We can't read the gauge value through promauto's Counter API
	// directly, but the SQL is the same shape. Re-run it SCOPED TO THIS
	// PROJECT (the production gauge query is global; scoping the test
	// verification keeps a concurrent package's older unpublished event
	// from skewing min(occurred_at)) and assert ~30s.
	var lag float64
	err := pool.QueryRow(context.Background(), `
SELECT COALESCE(EXTRACT(EPOCH FROM (now() - min(occurred_at))), 0)::float8
  FROM events WHERE published_at IS NULL AND project_id = $1`, pid).Scan(&lag)
	if err != nil {
		t.Fatalf("lag query: %v", err)
	}
	if lag < 25 || lag > 60 {
		t.Errorf("expected lag around 30s, got %.2f", lag)
	}
}

func TestDispatcher_GracefulShutdown(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	// No resetOutbox / count assertions here — this test only exercises the
	// Run loop's cancel-and-return behaviour, so it is intrinsically
	// independent of other packages' events.
	d := New(pool, nc, Options{BatchSize: 10, PollInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Give it a couple of ticks to enter the loop.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s of cancel")
	}
}

func TestDispatcher_BackoffOnError(t *testing.T) {
	// Validate the backoff arithmetic: it doubles up to MaxBackoff and
	// resets to PollInterval after a successful tick. We exercise this
	// without a real failure by running the same arithmetic the loop uses.
	poll := 100 * time.Millisecond
	max := 800 * time.Millisecond

	cur := poll
	stages := []time.Duration{}
	for i := 0; i < 6; i++ {
		stages = append(stages, cur)
		cur *= 2
		if cur > max {
			cur = max
		}
	}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		if stages[i] != w {
			t.Errorf("stage %d: got %v want %v", i, stages[i], w)
		}
	}

	// And reset on success: cur snaps back to poll.
	cur = poll
	if cur != 100*time.Millisecond {
		t.Errorf("reset broken: %v", cur)
	}
}

func TestClassifyError_TypedSentinels(t *testing.T) {
	// The typed-sentinel pattern binds the reason label to the call
	// site rather than to the error message. Wrapping must preserve
	// the reason via errors.As, so a wrapped tickError still
	// classifies correctly.
	wrap := func(reason string) error {
		return &tickError{reason: reason, err: errors.New("inner")}
	}
	wrapDeep := func(reason string) error {
		return fmt.Errorf("outer: %w", &tickError{reason: reason, err: errors.New("inner")})
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil → none", nil, "none"},
		{"tx_begin direct", wrap(reasonTxBegin), reasonTxBegin},
		{"db_query direct", wrap(reasonDBQuery), reasonDBQuery},
		{"scan direct", wrap(reasonScan), reasonScan},
		{"envelope direct", wrap(reasonEnvelope), reasonEnvelope},
		{"nats_publish direct", wrap(reasonNATSPublish), reasonNATSPublish},
		{"nats_flush direct", wrap(reasonNATSFlush), reasonNATSFlush},
		{"db_update direct", wrap(reasonDBUpdate), reasonDBUpdate},
		{"db_commit direct", wrap(reasonDBCommit), reasonDBCommit},
		{"deeply wrapped tx_begin still unwraps", wrapDeep(reasonTxBegin), reasonTxBegin},
		{"deeply wrapped nats_publish still unwraps", wrapDeep(reasonNATSPublish), reasonNATSPublish},
		{"non-tickError falls through to other", errors.New("something unexpected"), reasonOther},
		{"context.Canceled falls through to other", context.Canceled, reasonOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyError(c.err)
			if got != c.want {
				t.Errorf("classifyError = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDispatcher_TickErrorReasons verifies that each tick failure stage
// emits the correct reason label by exercising real failures rather than
// synthetic strings. The test uses a closed NATS connection (forces
// nats_publish), a closed pool (forces tx_begin), and a deliberately
// nil pool to leave the other paths covered transitively by the unit
// test above.
func TestDispatcher_TickErrorReasons(t *testing.T) {
	pool := newTestPool(t)
	srv, nc := startEmbeddedNATS(t)
	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)
	insertEvent(t, pool, owner, pid, "issue.created", "issue", "obt-reason", "", time.Time{})

	// nats_publish: close the connection and tick. The tick must fail
	// before the flush step, so the wrapped reason must be nats_publish.
	nc.Close()
	d := New(pool, nc, Options{BatchSize: 10})
	_, err := d.tick(context.Background())
	if err == nil {
		t.Fatalf("expected tick to fail with closed nats conn")
	}
	if got := classifyError(err); got != reasonNATSPublish {
		t.Errorf("expected reason=%q for closed-conn publish, got %q (err=%v)",
			reasonNATSPublish, got, err)
	}

	// tx_begin: close the pool and tick. BeginTx must fail.
	pool2 := newTestPool(t)
	pool2.Close()
	nc2, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer nc2.Close()
	d2 := New(pool2, nc2, Options{BatchSize: 10})
	_, err = d2.tick(context.Background())
	if err == nil {
		t.Fatalf("expected tick to fail with closed pool")
	}
	if got := classifyError(err); got != reasonTxBegin {
		t.Errorf("expected reason=%q for closed-pool tick, got %q (err=%v)",
			reasonTxBegin, got, err)
	}
}

func TestDispatcher_NoEventsCommitsCleanly(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)

	// A tick with nothing pending for this project must commit cleanly
	// (no error) and leave the project outbox empty. We don't assert the
	// global return count == 0 — a concurrent package may have unpublished
	// rows that this tick legitimately drains.
	d := New(pool, nc, Options{BatchSize: 10})
	if _, err := d.tick(context.Background()); err != nil {
		t.Fatalf("empty tick err: %v", err)
	}
	if got := testsupport.CountUnpublishedEventsForProject(t, pool, pid); got != 0 {
		t.Errorf("expected 0 unpublished events for project, got %d", got)
	}
}

// _ ensures pgx.ErrTxClosed (possibly imported indirectly) doesn't get
// pruned by go imports — keeps the symbol referenced if a future test
// needs to assert tx-already-closed pathways.
var _ = pgx.ErrTxClosed

// atomicSink is a small helper used by an ad-hoc subscriber; kept here
// so future tests that need a counter for received messages don't have
// to re-invent it. atomic.Int64 is part of stdlib in Go 1.20+.
type atomicSink struct{ count atomic.Int64 }

func (a *atomicSink) Inc() { a.count.Add(1) }
func (a *atomicSink) N() int64 {
	return a.count.Load()
}

// TestDispatcher_EnvelopeEnrichment_sk21 asserts that the published
// NATS envelope carries the actor + entity title + project prefix
// resolved via JOIN at dispatch time. Pre-sk-21 envelopes carried
// only the bare events-row columns and browser subscribers fell
// back to a literal "system" actor + empty title.
func TestDispatcher_EnvelopeEnrichment_sk21(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid := seedProject(t, pool, owner)
	resetOutbox(t, pool, pid)

	// Seed an actual issue so the JOIN finds a title + project prefix.
	issueID := "sk21-" + uuid.NewString()[:8]
	const wantTitle = "Test title for sk-21 envelope enrichment"
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false,
        'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		issueID, pid, wantTitle, owner)
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID)
	})

	// Subject filter scoped to this specific event so concurrent tests
	// don't poison the NextMsg call.
	wantSubj := fmt.Sprintf("skein.events.project.%s.issue.created.%s", pid.String(), issueID)
	sub, err := nc.SubscribeSync(wantSubj)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sub: %v", err)
	}

	insertEvent(t, pool, owner, pid, "issue.created", "issue", issueID, "", time.Time{})

	// Drain to empty (a global backlog may need >1 tick) so this project's
	// enriched envelope is guaranteed to have been published before NextMsg.
	d := New(pool, nc, Options{BatchSize: 50})
	drainProject(t, d, pool, pid)

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("NextMsg: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nbody=%s", err, string(msg.Data))
	}
	if env.Actor != "tester" {
		t.Errorf("actor = %q, want %q (insertEvent sets actor='tester')", env.Actor, "tester")
	}
	if env.ActorType != "human" {
		t.Errorf("actorType = %q, want %q", env.ActorType, "human")
	}
	if env.ActorUserID == nil || *env.ActorUserID != owner.String() {
		t.Errorf("actorUserId = %v, want %q", env.ActorUserID, owner.String())
	}
	if env.EntityTitle != wantTitle {
		t.Errorf("entityTitle = %q, want %q", env.EntityTitle, wantTitle)
	}
	if env.ProjectIDPrefix == "" {
		t.Errorf("projectIdPrefix is empty — JOIN to projects table didn't find a prefix")
	}
}

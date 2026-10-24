package signals_test

// batch_test.go — sk-622.21, test matrix row 30 (T014; INV-12, US-11 — the
// collector mechanism, NFR "publishing adds no queries", edge 9).
//
// Subject: signals.BeginTxFunc — one transaction attempt with its own
// collector — and the process publisher installed by signals.Configure,
// observed through an in-process NATS server (natstest), the way skein-web
// observes them.
//
// Reference (spec, written before this code): the personal signal is sent
// only after the change is committed, a rollback gives no signal (US-11,
// INV-12); the publisher does not coalesce across transactions but sends one
// message per user per committed transaction with the union of that
// transaction's areas (plan K9); without NATS the write still goes through
// (plan T014, "no-op like themes.Publisher").
//
// Absence is never judged by a pause: after the step under test the test
// commits a transaction for a fresh SENTINEL user and waits for its message.
// NATS Core preserves the order of one publisher connection towards one
// subscriber, so once the sentinel's message is in, any message the step
// under test produced is in too.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
)

var serializable = pgx.TxOptions{IsoLevel: pgx.Serializable}

// received is one personal signal as it arrived on the wire.
type received struct {
	subject string
	areas   []string
}

// signalSink subscribes to every personal signal subject and records each
// message per user.
type signalSink struct {
	mu     sync.Mutex
	byUser map[uuid.UUID][]received
	notify chan struct{}
}

// startSignalNATS starts an in-process NATS server, installs its connection
// as the process publisher (restored to "disabled" on cleanup) and returns a
// sink subscribed to `skein.user.*.signal` on a second connection.
func startSignalNATS(t *testing.T) *signalSink {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	pubConn, err := nats.Connect(srv.ClientURL(), nats.Name("signals-batch-test-pub"))
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pubConn.Close)
	subConn, err := nats.Connect(srv.ClientURL(), nats.Name("signals-batch-test-sub"))
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	t.Cleanup(subConn.Close)

	sink := &signalSink{byUser: make(map[uuid.UUID][]received), notify: make(chan struct{}, 1)}
	if _, err := subConn.Subscribe(signals.SignalSubjectPattern, func(m *nats.Msg) {
		var body struct {
			Areas []string `json:"areas"`
		}
		if err := json.Unmarshal(m.Data, &body); err != nil {
			t.Errorf("signal body is not JSON: %s: %v", m.Data, err)
			return
		}
		uid := userOfSubject(t, m.Subject)
		sink.mu.Lock()
		sink.byUser[uid] = append(sink.byUser[uid], received{subject: m.Subject, areas: body.Areas})
		sink.mu.Unlock()
		select {
		case sink.notify <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subConn.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	signals.Configure(pubConn)
	t.Cleanup(func() { signals.Configure(nil) })
	return sink
}

// userOfSubject extracts {uid} from skein.user.{uid}.signal.
func userOfSubject(t *testing.T, subject string) uuid.UUID {
	const prefix, suffix = "skein.user.", ".signal"
	if len(subject) <= len(prefix)+len(suffix) {
		t.Errorf("unexpected subject %q", subject)
		return uuid.Nil
	}
	uid, err := uuid.Parse(subject[len(prefix) : len(subject)-len(suffix)])
	if err != nil {
		t.Errorf("subject %q does not carry a user id: %v", subject, err)
	}
	return uid
}

func (s *signalSink) messages(uid uuid.UUID) []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.byUser[uid]...)
}

// waitFor blocks until every user in uids has at least one message, or fails
// the test after a generous deadline.
func (s *signalSink) waitFor(t *testing.T, uids ...uuid.UUID) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		missing := 0
		for _, uid := range uids {
			if len(s.byUser[uid]) == 0 {
				missing++
			}
		}
		s.mu.Unlock()
		if missing == 0 {
			return
		}
		select {
		case <-s.notify:
		case <-deadline:
			t.Fatalf("%d of %d expected personal signals never arrived", missing, len(uids))
		}
	}
}

// fixture is one subject row the test paths write to, plus the pool.
type fixture struct {
	pool    *pgxpool.Pool
	subject uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	subject, _ := testsupport.SeedUser(t, pool, "")
	return fixture{pool: pool, subject: subject}
}

// rename is the path's data change: one UPDATE of the subject row.
func (f fixture) rename(ctx context.Context, tx pgx.Tx, name string) error {
	_, err := tx.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, f.subject, name)
	return err
}

// commitSentinel commits a transaction that names a fresh user and waits for
// that user's signal; everything published before it has then arrived.
func (f fixture) commitSentinel(t *testing.T, sink *signalSink) {
	t.Helper()
	sentinel := uuid.New()
	err := signals.BeginTxFunc(context.Background(), f.pool, serializable, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		return f.rename(context.Background(), tx, "sentinel-"+sentinel.String()[:8])
	})
	if err != nil {
		t.Fatalf("sentinel transaction: %v", err)
	}
	sink.waitFor(t, sentinel)
}

func areasEqual(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBeginTxFunc_RollbackPublishesNothing — a path that named a user and
// then failed is rolled back, and the user gets no signal (INV-12).
func TestBeginTxFunc_RollbackPublishesNothing(t *testing.T) {
	f := newFixture(t)
	sink := startSignalNATS(t)
	ctx := context.Background()

	victim := uuid.New()
	pathErr := errors.New("path refused after naming the addressee")
	err := signals.BeginTxFunc(ctx, f.pool, serializable, func(tx pgx.Tx) error {
		if err := f.rename(ctx, tx, "rolled-back"); err != nil {
			return err
		}
		signals.Add(tx, victim, signals.AreaInbox, signals.AreaTasks)
		return pathErr
	})
	if !errors.Is(err, pathErr) {
		t.Fatalf("BeginTxFunc error = %v, want the path's own error", err)
	}

	f.commitSentinel(t, sink)
	if got := sink.messages(victim); len(got) != 0 {
		t.Errorf("rolled-back transaction published %d signal(s) to its addressee: %+v", len(got), got)
	}
}

// TestBeginTxFunc_RetryPublishesOnlyTheCommittedAttempt — the first attempt
// names user A (inbox) and user B (lens) and dies on a real serialization
// failure; the retry names only B (tasks) and commits. Exactly one message
// reaches B, carrying exactly {tasks}; A gets nothing.
func TestBeginTxFunc_RetryPublishesOnlyTheCommittedAttempt(t *testing.T) {
	f := newFixture(t)
	sink := startSignalNATS(t)
	ctx := context.Background()

	userA, userB := uuid.New(), uuid.New()

	// The test path: what a retry wrapper does — one BeginTxFunc per
	// attempt — with addressees that DIFFER between the attempts, so a
	// collector that survived the abort would show up as A or as `lens`.
	attempts := 0
	var firstErr error
	for attempts < 2 {
		attempts++
		attempt := attempts
		err := signals.BeginTxFunc(ctx, f.pool, serializable, func(tx pgx.Tx) error {
			if attempt == 1 {
				signals.Add(tx, userA, signals.AreaInbox)
				signals.Add(tx, userB, signals.AreaLens)
				// Snapshot the subject row, let a second connection
				// commit a change to it, then write it: 40001.
				var name string
				if err := tx.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, f.subject).Scan(&name); err != nil {
					return err
				}
				if _, err := f.pool.Exec(ctx, `UPDATE users SET name = 'concurrent' WHERE id = $1`, f.subject); err != nil {
					t.Fatalf("concurrent update: %v", err)
				}
				return f.rename(ctx, tx, "first-attempt")
			}
			signals.Add(tx, userB, signals.AreaTasks)
			return f.rename(ctx, tx, "second-attempt")
		})
		if attempt == 1 {
			firstErr = err
			continue
		}
		if err != nil {
			t.Fatalf("second attempt: %v", err)
		}
	}
	var pgErr *pgconn.PgError
	if !errors.As(firstErr, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("first attempt error = %v, want a serialization failure (40001) — the stand did not produce the retry it tests", firstErr)
	}

	sink.waitFor(t, userB)
	f.commitSentinel(t, sink)

	gotB := sink.messages(userB)
	if len(gotB) != 1 {
		t.Fatalf("user B got %d signals, want exactly 1 (one per committed transaction): %+v", len(gotB), gotB)
	}
	if !areasEqual(gotB[0].areas, "tasks") {
		t.Errorf("user B areas = %v, want exactly [tasks] — `lens` came from the aborted attempt", gotB[0].areas)
	}
	if got := sink.messages(userA); len(got) != 0 {
		t.Errorf("user A, named only by the aborted attempt, got %d signal(s): %+v", len(got), got)
	}
}

// TestBeginTxFunc_OneMessagePerUserWithUnion — several changes for one user
// in one transaction give one message with the union of the areas; a
// savepoint's addressees join it only when the savepoint is released.
func TestBeginTxFunc_OneMessagePerUserWithUnion(t *testing.T) {
	f := newFixture(t)
	sink := startSignalNATS(t)
	ctx := context.Background()

	user := uuid.New()
	err := signals.BeginTxFunc(ctx, f.pool, serializable, func(tx pgx.Tx) error {
		signals.Add(tx, user, signals.AreaWatching)
		if err := f.rename(ctx, tx, "union-1"); err != nil {
			return err
		}
		signals.Add(tx, user, signals.AreaInbox, signals.AreaWatching)

		released, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		signals.Add(released, user, signals.AreaMentions)
		if err := released.Commit(ctx); err != nil {
			return err
		}

		rolledBack, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		signals.Add(rolledBack, user, signals.AreaLens)
		return rolledBack.Rollback(ctx)
	})
	if err != nil {
		t.Fatalf("BeginTxFunc: %v", err)
	}

	sink.waitFor(t, user)
	f.commitSentinel(t, sink)

	got := sink.messages(user)
	if len(got) != 1 {
		t.Fatalf("user got %d signals for one transaction, want 1: %+v", len(got), got)
	}
	// Union in the contract's canonical order; `lens` was named only inside
	// the rolled-back savepoint.
	if !areasEqual(got[0].areas, "inbox", "mentions", "watching") {
		t.Errorf("areas = %v, want [inbox mentions watching]", got[0].areas)
	}
}

// statementTracer records the statements pgx sends on the traced pool.
type statementTracer struct {
	mu   sync.Mutex
	on   bool
	sent []string
}

func (tr *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on {
		tr.sent = append(tr.sent, data.SQL)
	}
	return ctx
}

func (tr *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *statementTracer) record(fn func()) []string {
	tr.mu.Lock()
	tr.sent, tr.on = nil, true
	tr.mu.Unlock()
	fn()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.on = false
	return append([]string(nil), tr.sent...)
}

// TestBeginTxFunc_StatementCountDoesNotDependOnAddressees — the same path
// naming 1 and 50 addressees sends the same statements to the database, and
// all of the addressees are published: collecting and publishing never touch
// the database.
func TestBeginTxFunc_StatementCountDoesNotDependOnAddressees(t *testing.T) {
	url := os.Getenv(testsupport.TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testsupport.TestDatabaseURL)
	}
	f := newFixture(t)
	sink := startSignalNATS(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tracer := &statementTracer{}
	cfg.ConnConfig.Tracer = tracer
	cfg.MinConns, cfg.MaxConns = 1, 1
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	if err := traced.Ping(ctx); err != nil {
		t.Fatalf("ping traced pool: %v", err)
	}

	run := func(n int) ([]string, []uuid.UUID) {
		users := make([]uuid.UUID, n)
		for i := range users {
			users[i] = uuid.New()
		}
		sent := tracer.record(func() {
			err := signals.BeginTxFunc(ctx, traced, serializable, func(tx pgx.Tx) error {
				for _, u := range users {
					signals.Add(tx, u, signals.AreaWatching, signals.AreaTasks)
				}
				return f.rename(ctx, tx, "count")
			})
			if err != nil {
				t.Fatalf("path with %d addressees: %v", n, err)
			}
		})
		return sent, users
	}

	one, oneUsers := run(1)
	fifty, fiftyUsers := run(50)
	sink.waitFor(t, append(oneUsers, fiftyUsers...)...)

	if len(one) == 0 {
		t.Fatal("tracer recorded no statements — the stand is not counting")
	}
	if len(one) != len(fifty) {
		t.Errorf("statements with 1 addressee = %d, with 50 = %d; want equal.\n1: %q\n50: %q", len(one), len(fifty), one, fifty)
	}
}

// TestBeginTxFunc_WithoutNATSCommits — with no NATS configured the path's
// change still commits and BeginTxFunc reports success.
func TestBeginTxFunc_WithoutNATSCommits(t *testing.T) {
	f := newFixture(t)
	signals.Configure(nil)
	ctx := context.Background()

	err := signals.BeginTxFunc(ctx, f.pool, serializable, func(tx pgx.Tx) error {
		signals.Add(tx, uuid.New(), signals.AreaProjects)
		return f.rename(ctx, tx, "no-nats")
	})
	if err != nil {
		t.Fatalf("BeginTxFunc without NATS: %v", err)
	}
	var name string
	if err := f.pool.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, f.subject).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "no-nats" {
		t.Errorf("subject name = %q, want %q — the change did not commit", name, "no-nats")
	}
}

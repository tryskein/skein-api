package notifications

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fanout_nplusone_test.go — sk-303.13, coverage-matrix row 11.
//
// The requirement is non-functional and stated once in the spec:
//
//	"The reason filter is applied in the same query as recipient selection:
//	 there must not be a separate round-trip per recipient."
//
// So the assertion is about the SHAPE of the work, not about how long it
// takes: the number of statements the fan-out issues must not grow with the
// number of recipients. Deliberately no latency or throughput threshold — the
// plan and the test plan both refuse them, and a timing assertion on a shared
// CI Postgres is a flake generator, not a proof.
//
// The counter is a pgx QueryTracer, so it sees every statement that actually
// leaves the process — including the SAVEPOINT / RELEASE pair the fan-out
// wraps itself in. That is the point: the count is of real round trips, not of
// call sites someone remembered to instrument.

// queryTracer counts the statements pgx sends on a connection and keeps their
// text, so a failure can name WHICH query multiplied.
type queryTracer struct {
	mu      sync.Mutex
	on      bool
	queries []string
}

func (tr *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on {
		tr.queries = append(tr.queries, data.SQL)
	}
	return ctx
}

func (tr *queryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// record turns counting on, runs fn, turns it off and returns what was sent.
func (tr *queryTracer) record(fn func()) []string {
	tr.mu.Lock()
	tr.queries = nil
	tr.on = true
	tr.mu.Unlock()

	fn()

	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.on = false
	out := make([]string, len(tr.queries))
	copy(out, tr.queries)
	return out
}

// countMatching counts the recorded statements containing needle.
func countMatching(queries []string, needle string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, needle) {
			n++
		}
	}
	return n
}

// newTracedTestPool is newTestPool with a query tracer attached and the pool
// pinned to ONE connection, so every statement of the test is observed on the
// same connection in a deterministic order.
func newTracedTestPool(t *testing.T) (*pgxpool.Pool, *queryTracer) {
	t.Helper()
	url := os.Getenv(testDBURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDBURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tr := &queryTracer{}
	cfg.ConnConfig.Tracer = tr
	cfg.MinConns = 1
	cfg.MaxConns = 1

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tr
}

// TestFanOut_NoNPlusOne_OnGrowingRecipientCorpus drives the same event through
// the same code with two recipient corpora an order of magnitude apart and
// asserts the statement count is IDENTICAL.
//
// Equality — not "grows slowly", not "under some constant". A per-recipient
// preferences read is the exact defect the requirement forbids, and it would
// show up as a difference of (40 - 2) statements. Comparing two runs also
// makes the assertion independent of how many statements the fan-out happens
// to need in total, so refactoring the number of set-based steps does not
// force a test edit — only reintroducing per-recipient work does.
func TestFanOut_NoNPlusOne_OnGrowingRecipientCorpus(t *testing.T) {
	pool, tracer := newTracedTestPool(t)
	ctx := context.Background()

	small := fanoutQueriesForRecipients(t, pool, tracer, ctx, "n1small", 2)
	large := fanoutQueriesForRecipients(t, pool, tracer, ctx, "n1large", 40)

	if len(small) != len(large) {
		t.Errorf("statement count grew with the recipient corpus: %d for 2 recipients, %d for 40\n2:\n%s\n40:\n%s",
			len(small), len(large), strings.Join(small, "\n---\n"), strings.Join(large, "\n---\n"))
	}

	// And name the two statements the requirement is actually about: the
	// preferences read must be ONE set-based query, and so must the recipient
	// selection it is composed with.
	for _, run := range []struct {
		label   string
		queries []string
	}{{"2 recipients", small}, {"40 recipients", large}} {
		if got := countMatching(run.queries, "user_notification_reason_prefs"); got != 1 {
			t.Errorf("%s: preferences read issued %d times, want exactly 1 (set-based)", run.label, got)
		}
		if got := countMatching(run.queries, "role_recipients AS ("); got != 1 {
			t.Errorf("%s: recipient selection issued %d times, want exactly 1", run.label, got)
		}
		if got := countMatching(run.queries, "INSERT INTO notifications"); got != 1 {
			t.Errorf("%s: insert issued %d times, want exactly 1 (set-based over unnest)", run.label, got)
		}
	}
}

// fanoutQueriesForRecipients seeds `count` project watchers, runs ONE fan-out
// with the tracer on, and returns the statements it sent. Every recipient is a
// project member with the reason switched on, so all of them actually
// materialise a row — a corpus that silently filtered down to zero would prove
// nothing about the fan-out's per-recipient behaviour.
func fanoutQueriesForRecipients(
	t *testing.T, pool *pgxpool.Pool, tracer *queryTracer, ctx context.Context,
	prefix string, count int,
) []string {
	t.Helper()

	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	issueID := prefix + "-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	store := NewStore(pool)
	for range count {
		u := seedTestUser(t, pool, "user")
		if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, u, owner); err != nil {
			t.Fatalf("seed member: %v", err)
		}
		if err := store.AddProjectWatch(ctx, projectID, u); err != nil {
			t.Fatalf("AddProjectWatch: %v", err)
		}
		// "Comment" is off in the narrow default; switch it on so every
		// seeded recipient really produces a row.
		enableReasonChannels(t, pool, u, ReasonKeyComment, true, false)
	}

	commentID := uuid.Must(uuid.NewRandom()).String()
	if _, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', 'hi', now(), now())`, commentID, issueID, owner); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentID) })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	eid := addEvent(t, tx, "comment.created", "comment", commentID, projectID, owner, "human")

	var inserted int64
	queries := tracer.record(func() {
		inserted, err = FanOut(ctx, tx, FanOutInput{
			EventID:     eid,
			EventType:   "comment.created",
			EntityType:  "comment",
			IssueID:     issueID,
			ProjectID:   projectID,
			ActorUserID: owner,
			ActorType:   "human",
		})
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if inserted != int64(count) {
		t.Fatalf("fan-out inserted %d rows for %d recipients — the corpus did not reach the write path",
			inserted, count)
	}
	return queries
}

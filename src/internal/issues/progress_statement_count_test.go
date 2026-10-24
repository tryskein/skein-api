package issues

// progress_statement_count_test.go — sk-489, T005, matrix row 19.
//
// The spec's non-functional constraint, verbatim: "The new numbers are in the same queries as
// today's, with no extra database round trip per row".
//
// A claim about the SHAPE of the work, not its duration: the number of statements
// the query sends to the database does not grow with the number of rows that
// have progress. There is deliberately no time threshold here — on a shared CI
// Postgres it produces flakes, not evidence.
//
// TWO runs are compared, with corpora an order of magnitude apart in size, rather than
// one run against a constant: this way the claim does not depend on how many
// statements the query needs overall, and rewriting the number of set-oriented
// steps does not break the test — only moving work back to per-row breaks it.
//
// What the test does NOT see (a boundary, not a default): it counts STATEMENTS and does not
// distinguish "one statement that became heavier". The "no work" leg is the fourth
// correlated subquery in the same projection, and its cost is not
// measured by this counter. For the NFR "no round trip per row" that is enough: the ban
// is worded about round trips.

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

	"skein-api/internal/authz"
)

// progressTracer counts the statements pgx sends and keeps their text, so a
// failure can name WHICH query multiplied. Modelled on
// notifications/fanout_nplusone_test.go#queryTracer.
type progressTracer struct {
	mu      sync.Mutex
	on      bool
	queries []string
}

func (tr *progressTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on {
		tr.queries = append(tr.queries, data.SQL)
	}
	return ctx
}

func (tr *progressTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *progressTracer) record(fn func()) []string {
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

// newTracedProgressPool — newTestPool with a tracer and a pool pinned to
// ONE connection: otherwise the statements of a run scatter over connections and
// the count stops being deterministic.
func newTracedProgressPool(t *testing.T) (*pgxpool.Pool, *progressTracer) {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tr := &progressTracer{}
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

// seedProgressCorpus seeds `epics` epics, each with two children —
// "done" and "won't do" — so that ALL progress legs, including the new one, are
// non-empty on every row. A corpus where the leg is always zero would prove
// nothing about the cost of that leg.
func seedProgressCorpus(t *testing.T, pool *pgxpool.Pool, epics int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	for i := 1; i <= epics; i++ {
		epicID := prefix + "-" + itoa(i)
		seedTypedIssue(t, pool, pid, owner, epicID, "corpus epic", TypeEpic, nil)
		done := epicID + ".1"
		noWork := epicID + ".2"
		seedTypedIssue(t, pool, pid, owner, done, "corpus done", TypeTask, &epicID)
		seedTypedIssue(t, pool, pid, owner, noWork, "corpus cancelled", TypeTask, &epicID)
		setClosedWithOutcome(t, pool, done, CloseOutcomeDone)
		setClosedWithOutcome(t, pool, noWork, CloseOutcomeWontDo)
	}
	return owner, pid
}

// TestProgressNumbers_NoPerRowStatement — row 19: neither the list nor the tree
// sends more statements to the database when the rows with progress become
// an order of magnitude more numerous.
func TestProgressNumbers_NoPerRowStatement(t *testing.T) {
	pool, tracer := newTracedProgressPool(t)
	store := NewStore(pool)

	run := func(epics int) (list, tree []string) {
		owner, pid := seedProgressCorpus(t, pool, epics)
		ctx := callerCtx(t, owner, "user")
		list = tracer.record(func() {
			if _, err := store.ListIssues(ctx, ListIssuesArgs{
				ProjectID: pid, Caller: &owner, Limit: 100,
			}); err != nil {
				t.Fatalf("ListIssues (%d epics): %v", epics, err)
			}
		})
		tree = tracer.record(func() {
			if _, err := store.IssueTree(ctx, IssueTreeArgs{
				Scope:  authz.ForUser(owner),
				Caller: owner,
				Types:  []string{"parent-child"},
			}); err != nil {
				t.Fatalf("IssueTree (%d epics): %v", epics, err)
			}
		})
		return list, tree
	}

	smallList, smallTree := run(2)
	largeList, largeTree := run(20)

	for _, c := range []struct {
		surface      string
		small, large []string
	}{
		{"ListIssues", smallList, largeList},
		{"IssueTree", smallTree, largeTree},
	} {
		if len(c.small) != len(c.large) {
			t.Errorf("%s: the number of statements grew with the corpus — %d on 2 epics, %d on 20.\n"+
				"The spec forbids an extra database round trip PER ROW, and the by-outcome numbers "+
				"must arrive in the same query as the old ones.\n2:\n%s\n20:\n%s",
				c.surface, len(c.small), len(c.large),
				strings.Join(c.small, "\n---\n"), strings.Join(c.large, "\n---\n"))
		}
	}

	// And name the statement the NFR is actually about: the branch progress batch
	// — exactly one per tree response, whatever the tree is.
	for _, c := range []struct {
		label   string
		queries []string
	}{{"2 epics", smallTree}, {"20 epics", largeTree}} {
		if got := countContaining(c.queries, TreeBranchProgressBatchLabel); got != 1 {
			t.Errorf("tree, %s: the branch progress batch was sent %d times, want exactly 1", c.label, got)
		}
	}
}

func countContaining(queries []string, needle string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, needle) {
			n++
		}
	}
	return n
}

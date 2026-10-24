package tools

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// queryCounter counts every statement the pool sends to Postgres. Each one is
// a round-trip — a message out and an answer back — and on a batch that is
// where the time goes: the work itself is trivial, the waiting is not.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// newCountingPool is newTestPool with a tracer attached.
func newCountingPool(t *testing.T) (*pgxpool.Pool, *queryCounter) {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURL, err)
	}
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
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
	return pool, counter
}

// updateManyRoundTripBudget is what a ten-row label batch may cost.
//
// Measured on the harness database after the change: 82 statements for ten
// rows with one label each — 8.2 a row.
//
// Before it, the row paid for GetIssue three times over: once to learn the
// project id, once inside AddLabel (whose post-state return the caller
// discarded), and once at the end to read a single version field. GetIssue
// is SEVEN statements on a non-epic — the issue row plus components,
// labels, comment count, commit refs, blockers and mentions — so those
// three reads alone were twenty-one a row, none of which reached the
// caller: a batch row carries {id, ok, version} and no prose.
//
// The budget is the measurement with room for a schema query or two, and it
// is a CEILING, not a target — the point is that a discarded hydration
// cannot creep back in unnoticed. One GetIssue re-added to the per-row path
// costs seven statements a row and lands seventy over.
//
// It guards the labels path only: patch, status and component rows are not
// exercised here, so a hydration sneaking back onto those steps would not
// trip it.
//
// If a legitimate change pushes the real number up, re-measure and move the
// constant with the reason in the commit — do not raise it to make a red
// test green.
const updateManyRoundTripBudget = 110

// TestUpdateMany_RoundTripBudget guards the cost of the batch path.
//
// It is deterministic — it counts statements, not milliseconds, so it does
// not care how loaded the machine or the database is. That matters because
// the incident this came from was a batch that took 17.9 seconds where the
// same call normally takes 2: the code was constant, the round-trip price
// was not. A timing assertion would have been flaky exactly when it mattered.
func TestUpdateMany_RoundTripBudget(t *testing.T) {
	pool, counter := newCountingPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "umc")

	const rows = 10
	items := make([]any, 0, rows)
	for i := 1; i <= rows; i++ {
		id := seedIssue(t, pool, pid, userID, prefix+"-"+itoa(i), "row", "open", 2)
		items = append(items, map[string]any{
			"id":              id,
			"expectedVersion": float64(1),
			"labelsAdd":       []any{"budget"},
		})
	}

	handler := handleUpdateMany(pool, issues.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}

	before := counter.n.Load()
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	spent := counter.n.Load() - before

	_, summary := updateManyRows(t, res)
	if summary["succeeded"] != float64(rows) {
		t.Fatalf("summary = %+v, want %d succeeded — a cheap batch that does not work is not the goal", summary, rows)
	}
	if spent > updateManyRoundTripBudget {
		t.Fatalf("batch of %d rows spent %d statements, budget %d — a discarded post-state load is the usual cause",
			rows, spent, updateManyRoundTripBudget)
	}
	t.Logf("batch of %d rows: %d statements (%.1f per row)", rows, spent, float64(spent)/float64(rows))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// withShortBatchDeadline shrinks the loop budget for one test and restores it.
func withShortBatchDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	prev := batchLoopDeadline
	batchLoopDeadline = d
	t.Cleanup(func() { batchLoopDeadline = prev })
}

func updateManyRows(t *testing.T, res *mcpgo.CallToolResult) ([]any, map[string]any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("batch answered with an error envelope; the caller must still get its rows: %+v", res)
	}
	body := extractToolJSON(t, res)
	rows, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("results missing or wrong type: %T", body["results"])
	}
	summary, _ := body["summary"].(map[string]any)
	return rows, summary
}

func rowErrorCode(t *testing.T, row any) string {
	t.Helper()
	obj, ok := row.(map[string]any)
	if !ok {
		t.Fatalf("row wrong type: %T", row)
	}
	errObj, ok := obj["error"].(map[string]any)
	if !ok {
		t.Fatalf("row carries no error object: %+v", obj)
	}
	code, _ := errObj["code"].(string)
	return code
}

func seedTwoIssues(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, user uuid.UUID, prefix string) (string, string) {
	t.Helper()
	return seedIssue(t, pool, pid, user, prefix+"-1", "first", "open", 2),
		seedIssue(t, pool, pid, user, prefix+"-2", "second", "open", 2)
}

// TestUpdateMany_DeadlineExceeded_AnswersInsteadOfVanishing is the contract
// this deadline exists for: when the server gives up it still returns the
// rows, and the ones it did not run say so with their own code — not with
// internal_error, which is also what a row that broke while being written
// returns.
func TestUpdateMany_DeadlineExceeded_AnswersInsteadOfVanishing(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "bdl")
	idA, idB := seedTwoIssues(t, pool, pid, userID, prefix)

	withShortBatchDeadline(t, time.Nanosecond)

	handler := handleUpdateMany(pool, issues.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{"id": idA, "expectedVersion": float64(1), "labelsAdd": []any{"x"}},
			map[string]any{"id": idB, "expectedVersion": float64(1), "labelsAdd": []any{"y"}},
		},
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	rows, summary := updateManyRows(t, res)
	if len(rows) != 2 {
		t.Fatalf("results = %d rows, want 2 — every input row must be accounted for", len(rows))
	}
	for i, row := range rows {
		if got := rowErrorCode(t, row); got != ErrCodeBatchDeadlineExceeded {
			t.Fatalf("results[%d].error.code = %q, want %q", i, got, ErrCodeBatchDeadlineExceeded)
		}
	}
	if summary["failed"] != float64(2) || summary["succeeded"] != float64(0) {
		t.Fatalf("summary = %+v, want 0 succeeded / 2 failed", summary)
	}
	// Nothing ran, so nothing may have been written.
	var labels int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_labels WHERE issue_id = ANY($1)`,
		[]string{idA, idB}).Scan(&labels); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	if labels != 0 {
		t.Fatalf("labels written = %d, want 0", labels)
	}
}

// TestUpdateMany_CallerGone_UsesItsOwnCode separates the two stop reasons.
// "The caller hung up" and "we ran out of time" ask for different responses
// from whoever retries, so they must not share a code.
func TestUpdateMany_CallerGone_UsesItsOwnCode(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "bdc")
	idA, _ := seedTwoIssues(t, pool, pid, userID, prefix)

	ctx, cancel := context.WithCancel(withCtx(t, userID, "admin"))
	cancel() // the caller is already gone when the loop starts

	handler := handleUpdateMany(pool, issues.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{"id": idA, "expectedVersion": float64(1), "labelsAdd": []any{"x"}},
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	rows, _ := updateManyRows(t, res)
	if got := rowErrorCode(t, rows[0]); got != ErrCodeRequestCancelled {
		t.Fatalf("results[0].error.code = %q, want %q", got, ErrCodeRequestCancelled)
	}
}

// TestBatchStopReason_NoStopWhenBothLive pins the negative: a healthy batch
// must not be talked into stopping.
func TestBatchStopReason_NoStopWhenBothLive(t *testing.T) {
	parent := context.Background()
	loop, cancel := context.WithTimeout(parent, time.Hour)
	defer cancel()

	if code, message, stop := batchStopReason(parent, loop); stop {
		t.Fatalf("stop=true on a live batch (code=%q, message=%q)", code, message)
	}
}

// TestBatchStopReason_ParentWinsOverDeadline: when both are done the caller
// being gone is the more specific truth, and the one that decides whether a
// response is worth writing at all.
func TestBatchStopReason_ParentWinsOverDeadline(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	// Deriving from an already-cancelled parent makes the child done at
	// once, so both conditions hold without waiting for a clock.
	loop, cancelLoop := context.WithTimeout(parent, time.Nanosecond)
	defer cancelLoop()
	if loop.Err() == nil {
		t.Fatal("precondition: the derived context should already be done")
	}

	code, _, stop := batchStopReason(parent, loop)
	if !stop || code != ErrCodeRequestCancelled {
		t.Fatalf("code=%q stop=%v, want %q", code, stop, ErrCodeRequestCancelled)
	}
}

// TestDepAddMany_DeadlineExceeded_MarksEveryRemainingRow covers the third
// batch tool on the same path. It is not redundant with the update_many
// case: each tool wires the stop block itself, into its own row type, and a
// wiring mistake here would be invisible in the other two — which is exactly
// how this tool ended up marking one row and walking the rest of the batch
// against a dead context.
func TestDepAddMany_DeadlineExceeded_MarksEveryRemainingRow(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "damd")
	idA := seedIssue(t, pool, pid, userID, prefix+"-1", "A", "open", 2)
	idB := seedIssue(t, pool, pid, userID, prefix+"-2", "B", "open", 2)

	withShortBatchDeadline(t, time.Nanosecond)

	handler := handleDepAddMany(pool, dependencies.NewStore(pool), issues.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{"fromId": idA, "toId": idB, "type": dependencies.DepBlocks},
			map[string]any{"fromId": idB, "toId": idA, "type": dependencies.DepRelated},
		},
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("batch answered with an error envelope instead of rows: %+v", res)
	}
	body := extractToolJSON(t, res)
	rows, ok := body["results"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("results = %v, want 2 rows", body["results"])
	}
	for i, row := range rows {
		if got := rowErrorCode(t, row); got != ErrCodeBatchDeadlineExceeded {
			t.Fatalf("results[%d].error.code = %q, want %q", i, got, ErrCodeBatchDeadlineExceeded)
		}
		obj, _ := row.(map[string]any)
		if obj["fromId"] == "" || obj["fromId"] == nil {
			t.Fatalf("results[%d] lost its fromId; an unrun row must still name what it was going to do: %+v", i, obj)
		}
	}
	var edges int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dependencies WHERE from_id = ANY($1)`,
		[]string{idA, idB}).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 0 {
		t.Fatalf("edges written = %d, want 0", edges)
	}
}

// holdInTx runs stmt in a transaction that stays open until the test ends,
// so whatever lock stmt takes is held that long. A row whose write needs
// that lock waits on it for as long as the test wants — which is how a row
// is kept "in progress" inside one chosen statement of its store call.
func holdInTx(t *testing.T, pool *pgxpool.Pool, stmt string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("lock holder %q: %v", stmt, err)
	}
}

// taggedPool is a pool whose backends carry application_name = appName, so
// waitForLockWait can find the handler's backend in pg_locks.
func taggedPool(t *testing.T, appName string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// waitForLockWait reports whether a backend of appName comes to wait on an
// ungranted lock before done is closed (the handler returned — then it never
// will). Polling is the observation, not the verdict: the bound only stops a
// broken fixture from hanging the run.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool, appName string, done <-chan struct{}) bool {
	t.Helper()
	bound := time.Now().Add(10 * time.Second)
	for time.Now().Before(bound) {
		select {
		case <-done:
			return false
		default:
		}
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE NOT l.granted AND a.application_name = $1`, appName).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// controlLoopContext replaces batchLoopContext for one test with a context
// the test ends itself, and returns the function that ends it. Ending it is
// what the batch deadline does in production: the loop's context is done,
// the caller's is not.
func controlLoopContext(t *testing.T) func() {
	t.Helper()
	prev := batchLoopContext
	stop := make(chan struct{})
	var once sync.Once
	batchLoopContext = func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		return ctx, cancel
	}
	t.Cleanup(func() {
		once.Do(func() { close(stop) })
		batchLoopContext = prev
	})
	return func() { once.Do(func() { close(stop) }) }
}

// stopRowInProgress runs handler in a goroutine, waits until its backend is
// parked on the lock the test holds — inside the write under test — and only
// then ends either the batch loop's context (stopLoop) or the caller's. It
// returns the single result row. Red when the row never parks: then the
// stop would not have landed where the test claims.
func stopRowInProgress(t *testing.T, observer *pgxpool.Pool, appName string, byCaller bool,
	call func(ctx context.Context) (*mcpgo.CallToolResult, error)) any {
	t.Helper()
	stopLoop := controlLoopContext(t)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	type outcome struct {
		res *mcpgo.CallToolResult
		err error
	}
	resCh := make(chan outcome, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := call(parent)
		resCh <- outcome{res, err}
	}()
	if !waitForLockWait(t, observer, appName, done) {
		t.Fatalf("the row never waited on the held lock — the stop would not land inside the write")
	}
	if byCaller {
		cancelParent()
	} else {
		stopLoop()
	}
	var out outcome
	select {
	case out = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("handler did not return within 30s of the stop")
	}
	if out.err != nil {
		t.Fatalf("handler error: %v", out.err)
	}
	rows, summary := updateManyRows(t, out.res)
	if len(rows) != 1 || summary["failed"] != float64(1) {
		t.Fatalf("results = %v, summary = %+v; want one failed row", rows, summary)
	}
	return rows[0]
}

// withCtxFrom is withCtx on top of parent, so the test keeps the power to
// cancel the caller's context.
func withCtxFrom(parent context.Context, userID uuid.UUID, role string) context.Context {
	ctx := auth.WithUserID(parent, userID)
	ctx = auth.WithUserRole(ctx, role)
	ctx = auth.WithSessionID(ctx, "test-session-"+userID.String()[:8])
	return auth.WithAPIKeyID(ctx, uuid.Must(uuid.NewRandom()))
}

var rowStops = []struct {
	name     string
	byCaller bool
	want     string
}{
	{"batch deadline", false, ErrCodeBatchDeadlineExceeded},
	{"caller gone", true, ErrCodeRequestCancelled},
}

// TestDepAddMany_StoppedInsideCreate is the sk-647.2 regression. In
// production the batch deadline fired inside Store.Create (the cycle check
// was spilling to disk) and the row answered internal_error — the code of a
// row that broke — so the caller could not tell "the server stopped" from a
// defect, and did not resend.
//
// The row is parked inside Create: its dependency INSERT waits for the KEY
// SHARE lock the foreign key takes on the FROM issue, which the test holds
// FOR UPDATE. Nothing else in the row waits on a lock, so the observed wait
// IS Create. Then the loop's context (or the caller's) ends. The transaction
// rolls back: no edge, no event.
func TestDepAddMany_StoppedInsideCreate(t *testing.T) {
	for _, stop := range rowStops {
		t.Run(stop.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, prefix := seedProject(t, pool, userID, "dadc")
			idA := seedIssue(t, pool, pid, userID, prefix+"-1", "A", "open", 2)
			idB := seedIssue(t, pool, pid, userID, prefix+"-2", "B", "open", 2)
			holdInTx(t, pool, `SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, idA)

			appName := "sk6472d-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			hp := taggedPool(t, appName)
			handler := handleDepAddMany(hp, dependencies.NewStore(hp), issues.NewStore(hp))
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"items": []any{
					map[string]any{"fromId": idA, "toId": idB, "type": dependencies.DepBlocks},
				},
			}
			row := stopRowInProgress(t, pool, appName, stop.byCaller, func(ctx context.Context) (*mcpgo.CallToolResult, error) {
				return handler(withCtxFrom(ctx, userID, "admin"), req)
			})
			if got := rowErrorCode(t, row); got != stop.want {
				t.Fatalf("results[0].error.code = %q, want %q", got, stop.want)
			}

			var edges, events int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2`, idA, idB).Scan(&edges); err != nil {
				t.Fatalf("count edges: %v", err)
			}
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM events WHERE type = $1 AND project_id = $2`,
				dependencies.EventDependencyCreated, pid).Scan(&events); err != nil {
				t.Fatalf("count events: %v", err)
			}
			if edges != 0 || events != 0 {
				t.Fatalf("after the stop: %d edges, %d dependency.created events, want 0 and 0", edges, events)
			}
		})
	}
}

// TestUpdateMany_StoppedInsideChain: the same defect on skein_update_many —
// a row whose update chain was cut answered internal_error. The chain
// commits step by step, and the test pins that: the field patch (an early
// step) commits, the label add (a late step) is parked on the test's own
// uncommitted insert of the same (issue, label) key — ON CONFLICT waits for
// it — and the stop lands there. State afterwards: the title is renamed and
// the version moved; the label is absent.
func TestUpdateMany_StoppedInsideChain(t *testing.T) {
	for _, stop := range rowStops {
		t.Run(stop.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, prefix := seedProject(t, pool, userID, "umdc")
			idA, _ := seedTwoIssues(t, pool, pid, userID, prefix)
			holdInTx(t, pool, `INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'late')`, idA)

			appName := "sk6472u-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			hp := taggedPool(t, appName)
			handler := handleUpdateMany(hp, issues.NewStore(hp))
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"items": []any{
					map[string]any{"id": idA, "expectedVersion": float64(1),
						"title": "renamed", "labelsAdd": []any{"late"}},
				},
			}
			row := stopRowInProgress(t, pool, appName, stop.byCaller, func(ctx context.Context) (*mcpgo.CallToolResult, error) {
				return handler(withCtxFrom(ctx, userID, "admin"), req)
			})
			if got := rowErrorCode(t, row); got != stop.want {
				t.Fatalf("results[0].error.code = %q, want %q", got, stop.want)
			}

			var (
				title   string
				version int
				labels  int
			)
			if err := pool.QueryRow(context.Background(),
				`SELECT title, version FROM issues WHERE id = $1`, idA).Scan(&title, &version); err != nil {
				t.Fatalf("read issue: %v", err)
			}
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM issue_labels WHERE issue_id = $1`, idA).Scan(&labels); err != nil {
				t.Fatalf("count labels: %v", err)
			}
			if title != "renamed" || version != 2 {
				t.Fatalf("early step: title %q version %d, want %q at version 2 — the patch commits before the label step",
					title, version, "renamed")
			}
			if labels != 0 {
				t.Fatalf("late step: %d labels committed, want 0", labels)
			}
		})
	}
}

// TestClassifyDepAddError_ContextStops: the classifier's two context codes,
// with the error wrapped the way the store wraps it, and the negative — a
// context error whose contexts are both alive is someone else's failure.
func TestClassifyDepAddError_ContextStops(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	live := context.Background()

	cases := []struct {
		name         string
		parent, loop context.Context
		err          error
		want         string
	}{
		{"loop deadline", live, expired, fmt.Errorf("running cycle check: %w", context.DeadlineExceeded), ErrCodeBatchDeadlineExceeded},
		{"caller gone", gone, gone, fmt.Errorf("loading issue x-1: %w", context.Canceled), ErrCodeRequestCancelled},
		{"both alive", live, live, fmt.Errorf("running cycle check: %w", context.DeadlineExceeded), ErrCodeInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDepAddError(tc.parent, tc.loop, tc.err, "u", "x-1", "x-2", dependencies.DepBlocks)
			if got.Code != tc.want {
				t.Fatalf("classifyDepAddError(%v) code = %q, want %q", tc.err, got.Code, tc.want)
			}
		})
	}
}

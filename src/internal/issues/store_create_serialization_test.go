package issues

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// sk-305 — concurrent creates under ONE parent must all succeed.
//
// The defect this pins: child-id allocation writes the per-parent
// child_counters row (nextChildID, id.go), so N agents creating children
// under the same parent at the same time serialise on that row and
// Postgres aborts the losers with SQLSTATE 40001. Before
// withSerializableRetry covered CreateIssue, that abort travelled to the
// caller as internal_error — production saw 10 of 226 creates fail that
// way in one 6h window.
//
// Why this test is honest rather than tautological: it does NOT assert
// "CreateIssue returned nil". It asserts that every concurrent create
// succeeded AND that the ids they got form the exact contiguous run
// parent.1 … parent.N with no gap and no duplicate. A retry that
// "succeeded" by handing two children the same suffix, or by skipping
// one, fails here — which is the property the counter row exists to
// provide and the one a careless retry would break.
//
// Red without the fix: with plain BeginTxFunc the losers come back with
// 40001 and the success count falls short of N.
func TestCreateIssue_ConcurrentChildren_NoSerializationError(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "parent epic", Type: "epic", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// 8 is above the pool's practical concurrency for one row and well
	// above the 3-way pile-up the retry budget is sized for, so the
	// contention is real rather than theoretical.
	const children = 8

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  []string
		errs []error
	)
	start := make(chan struct{})
	for i := range children {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together — that is the contention
			dto, cerr := store.CreateIssue(context.Background(), pid, prefix, "counter",
				CreateIssueInput{
					Title:    fmt.Sprintf("child %d", i),
					Type:     "task",
					Priority: 2,
					ParentID: ptrStr(parent.ID),
				},
				creator, HumanActor(creator, creatorName),
			)
			mu.Lock()
			defer mu.Unlock()
			if cerr != nil {
				errs = append(errs, cerr)
				return
			}
			ids = append(ids, dto.ID)
		}(i)
	}
	close(start)
	wg.Wait()

	for _, e := range errs {
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) &&
			(pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected) {
			t.Errorf("create lost a serialization race and was NOT retried: SQLSTATE %s: %v",
				pgErr.Code, e)
			continue
		}
		t.Errorf("create failed: %v", e)
	}
	if len(ids) != children {
		t.Fatalf("succeeded %d/%d creates", len(ids), children)
	}

	// The ids must be exactly parent.1 … parent.children — contiguous,
	// no duplicates. This is what the counter row guarantees and what a
	// wrong retry would corrupt.
	suffixes := make([]int, 0, len(ids))
	for _, id := range ids {
		rest, ok := strings.CutPrefix(id, parent.ID+".")
		if !ok {
			t.Fatalf("child id %q is not under parent %q", id, parent.ID)
		}
		n, cerr := strconv.Atoi(rest)
		if cerr != nil {
			t.Fatalf("child id %q has a non-numeric suffix %q", id, rest)
		}
		suffixes = append(suffixes, n)
	}
	sort.Ints(suffixes)
	for i, got := range suffixes {
		if want := i + 1; got != want {
			t.Fatalf("child suffixes %v are not the contiguous run 1..%d — "+
				"a gap or duplicate means the counter row was mis-replayed",
				suffixes, children)
		}
	}
}

// TestCreateIssue_SerializationFailure_Exhausted is sk-350.3 — the same
// exhaustion gap sk-303.17 closed for AddComment / UpdateComment /
// DeleteComment / CreateEpicWithChildren, deliberately left open here at
// the time: CreateIssue IS the sk-305 site the whole retry mechanism was
// built for, and TestCreateIssue_ConcurrentChildren_NoSerializationError
// above already proves createMaxAttempts absorbs realistic contention.
// This test proves the OTHER half: what happens when the budget is fully
// exhausted rather than absorbed.
//
// Before this fix, CreateIssue's withSerializableRetry result was
// returned to the caller verbatim on exhaustion — a raw *pgconn.PgError
// with SQLSTATE 40001/40P01 — which fell through every named branch in
// mapCreateError (MCP) and mapIssueErr (REST) into their ErrCodeInternalError
// / "internal_error" default. That is the exact defect shape sk-305 was
// meant to close, just pushed to the tail of the retry budget instead of
// the front.
//
// Drives the SAME project_counters collision createMaxAttempts times in a
// row (forceSerializationExhaustion) against a TOP-LEVEL create (no
// ParentID — contends on project_counters via nextCounterID, not on a
// per-parent child_counters row) and asserts the store returns
// errors.Is(err, ErrSerializationRetry), not a raw pgError.
//
// Red on the pre-fix code: errors.Is(err, ErrSerializationRetry) is false
// (Store.CreateIssue had no mapSerializationExhausted call at all), and
// errors.As(err, &pgErr) with SQLSTATE 40001 is true instead — verified by
// temporarily reverting the store.go hunk that wraps the
// withSerializableRetry result and re-running this test, which failed
// with exactly that mismatch before the fix was restored.
func TestCreateIssue_SerializationFailure_Exhausted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk3503-create-exhaust-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	opStore := NewStore(opPool)

	type createResult struct {
		dto *IssueDetail
		err error
	}
	resCh := make(chan createResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Started by forceSerializationExhaustion itself, ONLY once writer 0
	// already holds the row — see that function's doc comment on why the
	// target must never race writer 0 for who grabs a still-free row.
	start := func() {
		go func() {
			defer close(done)
			dto, err := opStore.CreateIssue(opCtx, pid, prefix, "counter",
				CreateIssueInput{Title: "sk-350.3 exhaustion repro", Type: "task", Priority: 2},
				creator, HumanActor(creator, creatorName),
			)
			resCh <- createResult{dto, err}
		}()
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("create goroutine did not finish after cancellation")
		}
	})

	forceSerializationExhaustion(t, pool, appName, createMaxAttempts, start,
		`UPDATE project_counters SET counter = counter WHERE project_id = $1`, pid)

	var got createResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("CreateIssue did not return within 30s")
	}

	if got.err == nil {
		t.Fatalf("CreateIssue: unexpected success after %d colliding writers "+
			"(createMaxAttempts=%d should have been fully exhausted, not absorbed)",
			createMaxAttempts, createMaxAttempts)
	}
	if !errors.Is(got.err, ErrSerializationRetry) {
		t.Fatalf("CreateIssue error = %v, want errors.Is(err, ErrSerializationRetry) — "+
			"an exhausted retry budget must surface as a business error, not a raw pgError "+
			"that the MCP/REST layers map to internal_error", got.err)
	}
}

// isRetryableTxError is the predicate withSerializableRetry branches on,
// so what it does and does not accept IS the retry policy. The claim-path
// suite already covers 40001/40P01; this pins the half that matters for a
// caller who cancelled — a create whose context is gone must not burn
// attempts re-running a transaction nobody is waiting for.
func TestIsRetryableTxError_ContextErrorsAreNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped cancel", fmt.Errorf("claiming child id: %w", context.Canceled), false},
		{"plain error", errors.New("boom"), false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"serialization failure", &pgconn.PgError{Code: pgSerializationFailure}, true},
		{"deadlock detected", &pgconn.PgError{Code: pgDeadlockDetected}, true},
		{"wrapped serialization failure",
			fmt.Errorf("ensuring child_counter row: %w",
				&pgconn.PgError{Code: pgSerializationFailure}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableTxError(tc.err); got != tc.want {
				t.Errorf("isRetryableTxError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

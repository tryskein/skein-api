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

// sk-303.17 — CreateEpicWithChildren ran its multi-insert transaction
// under plain pgx.BeginTxFunc with no retry and no isSerializationFailure
// fallback. Its top-level id claim (epicInput.ParentID is always nil —
// CreateEpicWithChildren forces it) advances project_counters via
// nextCounterID, the SAME per-project row every OTHER top-level create in
// the project — including plain CreateIssue, already covered by
// createMaxAttempts since sk-305 — must serialise through. That is
// structural contention, not incidental: this test measured 7 losses out
// of 8 concurrent epic creates in one project with ZERO retries (plain
// BeginTxFunc), worse than CreateIssue's own 8-way sibling fan-out (1/8
// lost at 3 attempts, per sk-305's TestCreateIssue_ConcurrentChildren_
// NoSerializationError) — plausibly because this transaction holds the
// SERIALIZABLE snapshot open through 1+len(children) inserts instead of
// one, widening the window a concurrent top-level create can land in.
//
// Honest rather than tautological, same standard as sk-305's sibling
// test: asserts every concurrent epic create succeeded, that the epics'
// top-level ids form the EXACT contiguous run prefix-1 .. prefix-N with
// no gap or duplicate (checked by sorting the suffixes and comparing
// against i+1 — the sibling TestCreateIssue_ConcurrentChildren_
// NoSerializationError's own technique, not just a uniqueness check,
// which passes on {1,2,4,...,9} just as readily as on {1..8}), AND that
// every epic's children are themselves the contiguous run epicID.1..
// epicID.3 with no gap or duplicate — the property BOTH project_counters
// (epic ids) and each epic's own child_counters row (child ids) exist to
// provide, and the one a careless retry (or a wrong replay that reruns
// only PART of the transaction) would break.
//
// sk-303.17 review: an earlier version of this test only
// checked suffix UNIQUENESS, not the exact contiguous range, and never
// looked at the returned children at all — so it could not have caught a
// mis-replay that skipped a suffix or dropped/duplicated a child. This
// version pins both.
//
// Red without the fix: with plain BeginTxFunc, most concurrent epic
// creates in the same project return a raw pgconn.PgError (40001)
// instead of an epic; with the fix but exhausting epicCreateMaxAttempts,
// they return issues.ErrSerializationRetry instead of succeeding.
func TestCreateEpicWithChildren_ConcurrentEpics_NoSerializationError(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)

	// 8 concurrent epics, each with 3 children — deliberately above the
	// realistic incidental-collision count, matching the sk-305 sibling
	// test's own justification for using 8.
	const epics = 8
	const childrenPerEpic = 3

	type epicResult struct {
		id       string
		children []string
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []epicResult
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < epics; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			epicDto, childDtos, err := store.CreateEpicWithChildren(context.Background(),
				pid, prefix, "counter",
				CreateIssueInput{Title: fmt.Sprintf("epic %d", i), Type: TypeEpic, Priority: 2},
				[]CreateIssueInput{
					{Title: "child a", Type: "task", Priority: 2},
					{Title: "child b", Type: "task", Priority: 2},
					{Title: "child c", Type: "task", Priority: 2},
				},
				SourceHuman, creator, HumanActor(creator, creatorName),
			)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			childIDs := make([]string, 0, len(childDtos))
			for _, c := range childDtos {
				childIDs = append(childIDs, c.ID)
			}
			results = append(results, epicResult{id: epicDto.ID, children: childIDs})
		}(i)
	}
	close(start)
	wg.Wait()

	for _, e := range errs {
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) &&
			(pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected) {
			t.Errorf("CreateEpicWithChildren lost a serialization race and was NOT retried: SQLSTATE %s: %v",
				pgErr.Code, e)
			continue
		}
		if errors.Is(e, ErrSerializationRetry) {
			t.Errorf("CreateEpicWithChildren exhausted epicCreateMaxAttempts=%d retries under %d-way contention: %v",
				epicCreateMaxAttempts, epics, e)
			continue
		}
		t.Errorf("CreateEpicWithChildren failed: %v", e)
	}
	if len(results) != epics {
		t.Fatalf("succeeded %d/%d epic creates", len(results), epics)
	}

	// The epic ids must be exactly prefix-1 .. prefix-epics — contiguous,
	// no duplicates. This is what project_counters guarantees and what a
	// wrong retry would corrupt. Sorting before comparing against i+1
	// (not just checking pairwise uniqueness) is what actually catches a
	// gap, matching the sk-305 sibling test's own technique.
	suffixes := make([]int, 0, len(results))
	for _, r := range results {
		rest, ok := strings.CutPrefix(r.id, prefix+"-")
		if !ok {
			t.Fatalf("epic id %q is not under prefix %q", r.id, prefix)
		}
		n, cerr := strconv.Atoi(rest)
		if cerr != nil {
			t.Fatalf("epic id %q has a non-numeric suffix %q", r.id, rest)
		}
		suffixes = append(suffixes, n)
	}
	sort.Ints(suffixes)
	for i, got := range suffixes {
		if want := i + 1; got != want {
			t.Fatalf("epic suffixes %v are not the contiguous run 1..%d — "+
				"a gap or duplicate means project_counters was mis-replayed",
				suffixes, epics)
		}
	}

	// Each epic's own children must be exactly epicID.1 .. epicID.3 —
	// contiguous, no duplicates, and exactly childrenPerEpic of them.
	// child_counters is a PER-EPIC row (not the shared project_counters
	// that the epics themselves contend on), so this mainly guards
	// against a mis-replay dropping/duplicating a child within one
	// epic's own transaction rather than against cross-epic contention.
	for _, r := range results {
		if len(r.children) != childrenPerEpic {
			t.Fatalf("epic %s: got %d children, want %d: %v",
				r.id, len(r.children), childrenPerEpic, r.children)
		}
		childSuffixes := make([]int, 0, len(r.children))
		for _, cid := range r.children {
			rest, ok := strings.CutPrefix(cid, r.id+".")
			if !ok {
				t.Fatalf("child id %q is not under epic %q", cid, r.id)
			}
			n, cerr := strconv.Atoi(rest)
			if cerr != nil {
				t.Fatalf("child id %q has a non-numeric suffix %q", cid, rest)
			}
			childSuffixes = append(childSuffixes, n)
		}
		sort.Ints(childSuffixes)
		for i, got := range childSuffixes {
			if want := i + 1; got != want {
				t.Fatalf("epic %s children suffixes %v are not the contiguous run 1..%d — "+
					"a gap or duplicate means the epic's child_counters was mis-replayed",
					r.id, childSuffixes, childrenPerEpic)
			}
		}
	}
}

// sk-303.17 review — the N-way test above proves the
// budget absorbs REALISTIC contention, but it is a probabilistic N-way
// race: nothing in it forces the Postgres abort to actually happen, so a
// lucky scheduling could pass green even on the pre-fix code. Unlike
// AddComment's mention fan-out (genuine multi-way SSI contention with no
// single blocking row to synchronize against — see the comment on
// TestAddComment_ConcurrentMentions_NoSerializationError for why
// waitForBlocked does not apply there), CreateEpicWithChildren's
// contention IS a plain row-lock: ClaimNextCounter's `UPDATE
// project_counters SET counter = counter + 1 WHERE project_id = $1` (id.go)
// blocks a concurrent updater on the SAME row exactly like setPinned's
// `UPDATE issues` or UpdateComment's `FOR UPDATE` — so the sk-202
// single-competitor technique applies here too.
//
// This test deterministically parks ONE competitor on project_counters,
// waits for CreateEpicWithChildren to block on it, and releases — the
// SAME technique as TestSetPinned_SerializationFailure_Retries /
// TestUpdateComment_SerializationFailure_Retries, retargeted at the
// per-project counter row instead of an issue/comment row.
//
// Red without the fix: with plain BeginTxFunc, CreateEpicWithChildren's
// UPDATE takes SQLSTATE 40001 and the raw pgconn.PgError travels straight
// out of the store call.
func TestCreateEpicWithChildren_SerializationFailure_Retries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-epic-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	opStore := NewStore(opPool)

	// Concurrent writer: touches the SAME project_counters row
	// ClaimNextCounter advances, without changing its value — the
	// sk-202 technique, retargeted at the counter row instead of an
	// issue/comment row.
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx,
		`UPDATE project_counters SET counter = counter WHERE project_id = $1`, pid); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	type epicResult struct {
		epic     *IssueDetail
		children []*IssueDetail
		err      error
	}
	resCh := make(chan epicResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(done)
		epicDto, childDtos, err := opStore.CreateEpicWithChildren(opCtx,
			pid, prefix, "counter",
			CreateIssueInput{Title: "sk-303.17 epic repro", Type: TypeEpic, Priority: 2},
			[]CreateIssueInput{
				{Title: "child a", Type: "task", Priority: 2},
			},
			SourceHuman, creator, HumanActor(creator, creatorName),
		)
		resCh <- epicResult{epicDto, childDtos, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("epic-create goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: CreateEpicWithChildren never blocked on the project_counters row lock")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got epicResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("CreateEpicWithChildren did not return within 30s")
	}
	if got.err != nil {
		t.Fatalf("CreateEpicWithChildren: unexpected error after a retryable abort: %v "+
			"(epicCreateMaxAttempts=%d should have absorbed one concurrent writer)",
			got.err, epicCreateMaxAttempts)
	}
	if got.epic == nil || len(got.children) != 1 {
		t.Fatalf("CreateEpicWithChildren succeeded but shape is wrong: epic=%+v children=%d",
			got.epic, len(got.children))
	}
}

// sk-303.17 review (critical) — same exhaustion gap as
// UpdateComment/DeleteComment (store_comment_row_serialization_test.go):
// before this fix, CreateEpicWithChildren had NO fallback on exhaustion
// at all, so a genuine class-40 abort that survived every one of
// epicCreateMaxAttempts attempts left the store as a raw pgconn.PgError
// — past mapEpicCreateError's every named branch, into its
// ErrCodeInternalError default. Drives the SAME project_counters
// collision epicCreateMaxAttempts times in a row
// (forceSerializationExhaustion) and asserts the store returns
// errors.Is(err, ErrSerializationRetry).
//
// Red on the pre-fix code: errors.Is(err, ErrSerializationRetry) is
// false, and errors.As(err, &pgErr) with SQLSTATE 40001/40P01 is true
// instead (possibly wrapped in *ErrEpicChildFailed if the losing attempt
// happened on a child insert rather than the epic's own counter claim —
// isRetryableTxError unwraps through that either way).
func TestCreateEpicWithChildren_SerializationFailure_Exhausted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-epic-exhaust-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	opStore := NewStore(opPool)

	type epicResult struct {
		epic     *IssueDetail
		children []*IssueDetail
		err      error
	}
	resCh := make(chan epicResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Started by forceSerializationExhaustion itself, ONLY once writer 0
	// already holds the row — see that function's doc comment on why the
	// target must never race writer 0 for who grabs a still-free row.
	start := func() {
		go func() {
			defer close(done)
			epicDto, childDtos, err := opStore.CreateEpicWithChildren(opCtx,
				pid, prefix, "counter",
				CreateIssueInput{Title: "sk-303.17 epic exhaustion repro", Type: TypeEpic, Priority: 2},
				[]CreateIssueInput{
					{Title: "child a", Type: "task", Priority: 2},
				},
				SourceHuman, creator, HumanActor(creator, creatorName),
			)
			resCh <- epicResult{epicDto, childDtos, err}
		}()
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("epic-create goroutine did not finish after cancellation")
		}
	})

	forceSerializationExhaustion(t, pool, appName, epicCreateMaxAttempts, start,
		`UPDATE project_counters SET counter = counter WHERE project_id = $1`, pid)

	var got epicResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("CreateEpicWithChildren did not return within 30s")
	}

	if got.err == nil {
		t.Fatalf("CreateEpicWithChildren: unexpected success after %d colliding writers "+
			"(epicCreateMaxAttempts=%d should have been fully exhausted, not absorbed)",
			epicCreateMaxAttempts, epicCreateMaxAttempts)
	}
	if !errors.Is(got.err, ErrSerializationRetry) {
		t.Fatalf("CreateEpicWithChildren error = %v, want errors.Is(err, ErrSerializationRetry) — "+
			"an exhausted retry budget must surface as a business error, not a raw pgError "+
			"that the MCP layer maps to internal_error", got.err)
	}
}

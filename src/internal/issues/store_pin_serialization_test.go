package issues

import (
	"context"
	"testing"
	"time"
)

// sk-303.17 — setPinned (Pin/Unpin) ran its read-then-UPDATE under
// SERIALIZABLE with plain pgx.BeginTxFunc, same as CreateIssue before
// sk-305: a genuine SQLSTATE 40001/40P01 abort travelled straight to the
// caller as a raw pgError, which the MCP/REST layers have no mapping for
// and surface as internal_error. Unlike PatchIssue/ReplaceComponents/etc.
// (which already catch isSerializationFailure and degrade to
// ErrVersionConflict), setPinned had NO fallback at all — the pre-sk-305
// defect shape, not the "spurious version conflict" shape.
//
// The contention this forces is a single concurrent writer touching the
// SAME issue row without changing its version (e.g. a comment or a claim
// landing between setPinned's SELECT and its UPDATE) — the sk-202 repro
// technique, not an N-way structural fan-out. That is the "accidental,
// single competitor" shape claimMaxAttempts was sized for, and this test
// is green at that same budget (pinMaxAttempts = claimMaxAttempts).
//
// Red without the fix: with plain BeginTxFunc, setPinned's UPDATE takes
// 40001 and Pin returns the raw pgconn.PgError up through the store call.
func TestSetPinned_SerializationFailure_Retries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-pin-" + uniqueTag()
	pinPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	pinStore := NewStore(pinPool)

	dto, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-303.17 pin repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Concurrent writer: touches updated_at only, on the SAME row, without
	// bumping version — mirrors the sk-202 technique exactly.
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx,
		`UPDATE issues SET updated_at = now() WHERE id = $1`, dto.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	type pinResult struct {
		dto *IssueDetail
		err error
	}
	resCh := make(chan pinResult, 1)
	done := make(chan struct{})
	pinCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(done)
		d, err := pinStore.Pin(pinCtx, dto.ID, dto.Version, HumanActor(creator, creatorName))
		resCh <- pinResult{d, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("pin goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: Pin never blocked on the concurrent writer's row lock")
	}

	// Release the writer → Pin's UPDATE hits 40001.
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got pinResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("Pin did not return within 30s")
	}

	if got.err != nil {
		t.Fatalf("Pin: unexpected error after a retryable abort: %v "+
			"(pinMaxAttempts=%d should have absorbed one concurrent writer)",
			got.err, pinMaxAttempts)
	}
	if got.dto == nil || !got.dto.Pinned {
		t.Fatalf("Pin succeeded but issue is not pinned: %+v", got.dto)
	}
	if got.dto.Version != dto.Version+1 {
		t.Fatalf("version = %d, want %d (exactly one committed UPDATE, not double-applied by a mis-replay)",
			got.dto.Version, dto.Version+1)
	}
}

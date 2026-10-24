package issues

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sk-303.17 — UpdateComment and DeleteComment both run `SELECT ... FOR
// UPDATE` on the comments row, then write it, under plain SERIALIZABLE
// BeginTxFunc with no isSerializationFailure fallback at all. A concurrent
// writer that blocks the FOR UPDATE and then commits is the textbook
// Postgres SSI "dangerous structure" for a blocked reader — the sk-202
// repro technique — and it reliably drives the blocked transaction into
// SQLSTATE 40001 once released, which pre-fix travelled straight to the
// caller as a raw pgError (mapCommentWriteError's ErrCodeInternalError
// default branch).
//
// This is a single accidental competitor on one row — Claim's contention
// shape exactly — so commentRowMaxAttempts reuses claimMaxAttempts'
// value; both tests are green at that budget.

func TestUpdateComment_SerializationFailure_Retries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-updc-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-303.17 update-comment repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	comment, err := store.AddComment(ctx, issue.ID, "original body", creator, creatorName,
		CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	// Concurrent writer: touches the comment row without bumping its
	// version — same technique as sk-202 / setPinned, retargeted at
	// `comments`.
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx,
		`UPDATE comments SET updated_at = now() WHERE id = $1`, comment.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	type updResult struct {
		dto *CommentDTO
		err error
	}
	resCh := make(chan updResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(done)
		d, err := opStore.UpdateComment(opCtx, comment.ID, "edited body", comment.Version,
			HumanActor(creator, creatorName))
		resCh <- updResult{d, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("update goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: UpdateComment never blocked on the concurrent writer's row lock")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got updResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("UpdateComment did not return within 30s")
	}
	if got.err != nil {
		t.Fatalf("UpdateComment: unexpected error after a retryable abort: %v "+
			"(commentRowMaxAttempts=%d should have absorbed one concurrent writer)",
			got.err, commentRowMaxAttempts)
	}
	if got.dto == nil || got.dto.Body != "edited body" {
		t.Fatalf("UpdateComment succeeded but body is wrong: %+v", got.dto)
	}
	if got.dto.Version != comment.Version+1 {
		t.Fatalf("version = %d, want %d (exactly one committed UPDATE, not double-applied by a mis-replay)",
			got.dto.Version, comment.Version+1)
	}
}

func TestDeleteComment_SerializationFailure_Retries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-delc-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-303.17 delete-comment repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	comment, err := store.AddComment(ctx, issue.ID, "to be deleted", creator, creatorName,
		CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx,
		`UPDATE comments SET updated_at = now() WHERE id = $1`, comment.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	resCh := make(chan error, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(done)
		resCh <- opStore.DeleteComment(opCtx, comment.ID, "cleanup", HumanActor(creator, creatorName))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("delete goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: DeleteComment never blocked on the concurrent writer's row lock")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var gotErr error
	select {
	case gotErr = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("DeleteComment did not return within 30s")
	}
	if gotErr != nil {
		t.Fatalf("DeleteComment: unexpected error after a retryable abort: %v "+
			"(commentRowMaxAttempts=%d should have absorbed one concurrent writer)",
			gotErr, commentRowMaxAttempts)
	}

	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM comments WHERE id = $1`, comment.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("post-state read: %v", err)
	}
	if deletedAt == nil {
		t.Fatalf("DeleteComment returned success but deleted_at is still NULL")
	}
}

// sk-303.17 review (critical) — the two tests above only prove
// that ONE colliding writer is absorbed. They say nothing about what
// happens once commentRowMaxAttempts is truly exhausted: before this
// review fix, UpdateComment/DeleteComment had NO fallback on exhaustion
// at all (unlike setPinned, which already degraded to
// ErrVersionConflict) — a raw *pgconn.PgError went straight out of the
// store, past mapCommentWriteError's every named branch, into its
// ErrCodeInternalError default. That is exactly the sk-305 defect shape
// the retry was supposed to close, just moved to the tail of the budget.
//
// These two drive the SAME row-lock collision commentRowMaxAttempts
// times in a row (forceSerializationExhaustion — every one of the
// target's attempts loses, not just the first) and assert the store
// returns errors.Is(err, ErrSerializationRetry) — an honest "nothing was
// written, retry verbatim" — instead of a raw pgconn.PgError.
//
// Red on the pre-fix code (return nil, err / return
// s.withSerializableRetry(...) directly, no mapSerializationExhausted
// call): errors.Is(err, ErrSerializationRetry) is false, and
// errors.As(err, &pgErr) with SQLSTATE 40001/40P01 is true instead.

func TestUpdateComment_SerializationFailure_Exhausted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-updc-exhaust-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-303.17 update-comment exhaustion repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	comment, err := store.AddComment(ctx, issue.ID, "original body", creator, creatorName,
		CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	type updResult struct {
		dto *CommentDTO
		err error
	}
	resCh := make(chan updResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Started by forceSerializationExhaustion itself, ONLY once writer 0
	// already holds the row — see that function's doc comment on why the
	// target must never race writer 0 for who grabs a still-free row.
	start := func() {
		go func() {
			defer close(done)
			d, err := opStore.UpdateComment(opCtx, comment.ID, "edited body", comment.Version,
				HumanActor(creator, creatorName))
			resCh <- updResult{d, err}
		}()
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("update goroutine did not finish after cancellation")
		}
	})

	// Collide commentRowMaxAttempts times in a row — every attempt loses,
	// not just the first, so the target's budget is provably exhausted.
	forceSerializationExhaustion(t, pool, appName, commentRowMaxAttempts, start,
		`UPDATE comments SET updated_at = now() WHERE id = $1`, comment.ID)

	var got updResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("UpdateComment did not return within 30s")
	}

	if got.err == nil {
		t.Fatalf("UpdateComment: unexpected success after %d colliding writers "+
			"(commentRowMaxAttempts=%d should have been fully exhausted, not absorbed)",
			commentRowMaxAttempts, commentRowMaxAttempts)
	}
	if !errors.Is(got.err, ErrSerializationRetry) {
		t.Fatalf("UpdateComment error = %v, want errors.Is(err, ErrSerializationRetry) — "+
			"an exhausted retry budget must surface as a business error, not a raw pgError "+
			"that the MCP/REST layer maps to internal_error", got.err)
	}
}

func TestDeleteComment_SerializationFailure_Exhausted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk30317-delc-exhaust-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-303.17 delete-comment exhaustion repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	comment, err := store.AddComment(ctx, issue.ID, "to be deleted", creator, creatorName,
		CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	resCh := make(chan error, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Started by forceSerializationExhaustion itself, ONLY once writer 0
	// already holds the row — see that function's doc comment on why the
	// target must never race writer 0 for who grabs a still-free row.
	start := func() {
		go func() {
			defer close(done)
			resCh <- opStore.DeleteComment(opCtx, comment.ID, "cleanup", HumanActor(creator, creatorName))
		}()
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("delete goroutine did not finish after cancellation")
		}
	})

	forceSerializationExhaustion(t, pool, appName, commentRowMaxAttempts, start,
		`UPDATE comments SET updated_at = now() WHERE id = $1`, comment.ID)

	var gotErr error
	select {
	case gotErr = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("DeleteComment did not return within 30s")
	}

	if gotErr == nil {
		t.Fatalf("DeleteComment: unexpected success after %d colliding writers "+
			"(commentRowMaxAttempts=%d should have been fully exhausted, not absorbed)",
			commentRowMaxAttempts, commentRowMaxAttempts)
	}
	if !errors.Is(gotErr, ErrSerializationRetry) {
		t.Fatalf("DeleteComment error = %v, want errors.Is(err, ErrSerializationRetry) — "+
			"an exhausted retry budget must surface as a business error, not a raw pgError "+
			"that the MCP/REST layer maps to internal_error", gotErr)
	}

	// The comment must be untouched — every attempt's UPDATE rolled back
	// with its transaction, so a mis-mapped "exhausted" path double-
	// deleting or otherwise mutating the row would be its own regression.
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM comments WHERE id = $1`, comment.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("post-state read: %v", err)
	}
	if deletedAt != nil {
		t.Fatalf("DeleteComment exhausted its retries but the row was soft-deleted anyway: deleted_at=%v", *deletedAt)
	}
}

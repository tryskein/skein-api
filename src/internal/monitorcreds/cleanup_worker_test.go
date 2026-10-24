package monitorcreds

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cleanup_worker_test — loop-level tests for RunCleanupWorker (sk-142.1, G5).
// Store.Cleanup's row-selection correctness is covered by
// TestCleanupDeletesDeadRows in store_test.go; this suite drives the goroutine:
// the immediate boot tick, survival across multiple ticks, and clean shutdown
// on ctx cancel.

// waitGone polls until the credential identified by plaintext is gone, or
// fails after timeout. Used to observe the worker's asynchronous delete.
func waitGone(t *testing.T, pool *pgxpool.Pool, plaintext string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !exists(t, pool, plaintext) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("credential still present after %s — cleanup worker did not delete it", timeout)
}

// TestRunCleanupWorker_ReapsAndStops asserts the worker (a) reaps a
// past-deadline row on its immediate boot tick, (b) leaves a live row alone,
// (c) keeps running across ticks (a second dead row minted AFTER boot is
// reaped by a later tick — proving the loop wasn't a one-shot), and (d)
// returns promptly when ctx is cancelled.
func TestRunCleanupWorker_ReapsAndStops(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A live credential — must survive every sweep.
	livePlain, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint live: %v", err)
	}
	// A dead (past-absolute-deadline) credential — must be reaped on boot.
	deadPlain, deadCred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint dead: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET absolute_deadline = now() - interval '1 hour' WHERE id = $1`,
		deadCred.ID); err != nil {
		t.Fatalf("age out deadline: %v", err)
	}

	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		RunCleanupWorker(wctx, f.store, CleanupConfig{
			Interval:         50 * time.Millisecond,
			RevokedRetention: time.Hour,
		})
		close(done)
	}()

	// (a) boot tick fires immediately → dead row gone fast.
	waitGone(t, f.pool, deadPlain, 3*time.Second)
	// (b) the live row is untouched.
	if !exists(t, f.pool, livePlain) {
		t.Fatalf("live credential was deleted by the worker")
	}

	// (c) loop survives across ticks: a second dead row minted after boot must
	// be reaped by a subsequent ticker tick.
	dead2Plain, dead2Cred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint dead2: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET absolute_deadline = now() - interval '1 hour' WHERE id = $1`,
		dead2Cred.ID); err != nil {
		t.Fatalf("age out deadline (dead2): %v", err)
	}
	waitGone(t, f.pool, dead2Plain, 3*time.Second)

	// (d) cancel → the worker returns promptly.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("cleanup worker did not return within 3s of ctx cancel")
	}

	if !exists(t, f.pool, livePlain) {
		t.Fatalf("live credential was deleted before shutdown")
	}
}

// TestRunCleanupWorker_DisabledOnNonPositiveInterval proves the defensive
// guard: a non-positive interval makes the worker a no-op that returns
// immediately without touching the store. Pure unit — the store (nil pool) is
// never dereferenced because the function returns before the first tick.
func TestRunCleanupWorker_DisabledOnNonPositiveInterval(t *testing.T) {
	done := make(chan struct{})
	go func() {
		RunCleanupWorker(context.Background(), New(nil, 6*time.Hour), CleanupConfig{Interval: 0})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunCleanupWorker with interval<=0 did not return immediately")
	}
}

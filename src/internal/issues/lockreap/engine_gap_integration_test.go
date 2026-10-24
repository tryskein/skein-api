package lockreap

import (
	"context"
	"testing"
	"time"
)

// sk-105.4 (S3 engine gaps) — stale-lock reaper, driven via the EXPORTED
// synchronous seam Reaper.Tick(ctx) (no background goroutine started in
// test setup, per spec §5 M7).
//
// The package already covers the all-stale release path and the
// fresh-lock-left-alone path. This test adds a MIXED scenario in a single
// project: one stale lock (eligible) + one fresh lock (ineligible). A
// single Tick must release exactly the stale one and emit exactly one
// issue.unclaimed event scoped to that issue — proving the threshold gate
// is per-row, not per-tick.
func TestReaper_MixedThreshold_ReleasesOnlyStale(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	staleID := prefix + "-1"
	freshID := prefix + "-2"
	// 2h-old lock — past the 30m threshold → eligible.
	seedLockedIssue(t, pool, pid, owner, staleID, "sess-stale", 2*time.Hour)
	// 5m-old lock — under threshold → must survive.
	seedLockedIssue(t, pool, pid, owner, freshID, "sess-fresh", 5*time.Minute)
	// issues.project_id is ON DELETE RESTRICT, so the project cleanup
	// registered by seedProject cannot drop them — clean the issues up
	// explicitly (LIFO: this runs before the project DELETE).
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issues WHERE id = ANY($1)`, []string{staleID, freshID})
	})

	r := New(pool, Config{
		Interval:  time.Minute, // unused by direct Tick()
		Threshold: 30 * time.Minute,
		BatchSize: 200,
	})
	r.Tick(ctx) // synchronous seam

	// Stale lock released, version bumped 1 → 2.
	var staleLockedBy *string
	var staleVersion int
	if err := pool.QueryRow(ctx,
		`SELECT locked_by, version FROM issues WHERE id = $1`, staleID).Scan(&staleLockedBy, &staleVersion); err != nil {
		t.Fatalf("read stale issue: %v", err)
	}
	if staleLockedBy != nil {
		t.Fatalf("stale lock not released: locked_by = %v", *staleLockedBy)
	}
	if staleVersion != 2 {
		t.Fatalf("stale issue version = %d, want 2", staleVersion)
	}

	// Fresh lock untouched.
	var freshLockedBy *string
	var freshVersion int
	if err := pool.QueryRow(ctx,
		`SELECT locked_by, version FROM issues WHERE id = $1`, freshID).Scan(&freshLockedBy, &freshVersion); err != nil {
		t.Fatalf("read fresh issue: %v", err)
	}
	if freshLockedBy == nil || *freshLockedBy != "sess-fresh" {
		t.Fatalf("fresh lock was reaped: locked_by = %v", freshLockedBy)
	}
	if freshVersion != 1 {
		t.Fatalf("fresh issue version = %d, want 1 (untouched)", freshVersion)
	}

	// Exactly one issue.unclaimed event, scoped to the stale issue, with
	// the reaper's reason + system actor.
	var staleEvents int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE entity_type = 'issue' AND entity_id = $1
   AND type = 'issue.unclaimed'
   AND reason = 'auto-released-stale-lock'
   AND actor_type = 'system'`, staleID).Scan(&staleEvents); err != nil {
		t.Fatalf("count stale unclaimed events: %v", err)
	}
	if staleEvents != 1 {
		t.Fatalf("expected 1 issue.unclaimed event on stale issue, got %d", staleEvents)
	}

	// No unclaim event for the fresh issue.
	var freshEvents int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.unclaimed'`,
		freshID).Scan(&freshEvents); err != nil {
		t.Fatalf("count fresh unclaimed events: %v", err)
	}
	if freshEvents != 0 {
		t.Fatalf("expected 0 unclaimed events on fresh issue, got %d", freshEvents)
	}
}

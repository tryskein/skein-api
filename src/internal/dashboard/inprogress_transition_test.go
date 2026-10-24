package dashboard

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/redact"
	"skein-api/internal/testsupport"
)

// S2 (sk-105.3) transition-reassert coverage for the dashboard
// in-progress surface (store.go::InProgress ~1172-1226). The existing
// TestInProgress_HappyPath snapshots membership of the result; this file
// drives status transitions to and from in_progress / deferred / closed
// and re-asserts that the issue enters and leaves the surface. The
// surface predicate is `(status='in_progress' OR locked_by IS NOT NULL)
// AND status <> 'closed'`.
//
// The count is scoped to the seeded project's id_prefix so concurrent
// packages' in-progress rows never perturb the assertion.

// inProgressCountForProject returns how many in-progress rows belong to
// the seeded project. Scoped via projectFilter, so it's a project-scoped
// assert (never a global count).
func inProgressCountForProject(t *testing.T, store *Store, pid uuid.UUID) int {
	t.Helper()
	res, err := store.InProgress(context.Background(),
		map[uuid.UUID]struct{}{pid: {}}, false, &pid, 200, nil /*reach*/, redact.None)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}
	return len(res.Items)
}

// hasIssue reports whether the seeded issue is currently on the surface.
func hasIssue(t *testing.T, store *Store, pid uuid.UUID, idStr string) bool {
	t.Helper()
	res, err := store.InProgress(context.Background(),
		map[uuid.UUID]struct{}{pid: {}}, false, &pid, 200, nil /*reach*/, redact.None)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}
	for _, r := range res.Items {
		if r.IssueID == idStr {
			return true
		}
	}
	return false
}

// TestInProgress_StatusTransitions takes one issue through
// open→in_progress→deferred→closed→in_progress and re-asserts presence on
// the dashboard surface after each move.
func TestInProgress_StatusTransitions(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := New(pool)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tsdip")

	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, id, "dashboard mover", "open", 2)

	// open → NOT on the surface.
	if hasIssue(t, store, pid, id) {
		t.Fatalf("open issue %s should not be on the in-progress surface", id)
	}
	if n := inProgressCountForProject(t, store, pid); n != 0 {
		t.Fatalf("project in-progress count (open) = %d, want 0", n)
	}

	// open → in_progress: appears.
	testsupport.SetStatus(t, pool, id, "in_progress")
	if !hasIssue(t, store, pid, id) {
		t.Fatalf("in_progress issue %s should be on the surface", id)
	}
	if n := inProgressCountForProject(t, store, pid); n != 1 {
		t.Fatalf("project in-progress count (in_progress) = %d, want 1", n)
	}

	// in_progress → deferred: leaves (not in_progress, not locked).
	testsupport.SetStatus(t, pool, id, "deferred")
	if hasIssue(t, store, pid, id) {
		t.Fatalf("deferred issue %s should leave the surface", id)
	}
	if n := inProgressCountForProject(t, store, pid); n != 0 {
		t.Fatalf("project in-progress count (deferred) = %d, want 0", n)
	}

	// deferred → in_progress again: re-appears.
	testsupport.SetStatus(t, pool, id, "in_progress")
	if !hasIssue(t, store, pid, id) {
		t.Fatalf("re-in_progress issue %s should be back on the surface", id)
	}

	// in_progress → closed: leaves (status <> 'closed' clause).
	testsupport.SetStatus(t, pool, id, "closed")
	if hasIssue(t, store, pid, id) {
		t.Fatalf("closed issue %s should leave the surface", id)
	}
	if n := inProgressCountForProject(t, store, pid); n != 0 {
		t.Fatalf("project in-progress count (closed) = %d, want 0", n)
	}
}

// TestInProgress_LockedClosedExcluded asserts the `AND status <> 'closed'`
// guard wins even when locked_by is set: a closed-but-still-locked issue
// (a pathological state, but the predicate must hold) is NOT on the
// surface. We reach it by locking an in_progress issue, asserting it
// shows, then closing it — closing clears nothing about the lock here, so
// the row stays locked but closed.
func TestInProgress_LockedClosedExcluded(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := New(pool)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tsdlk")

	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, id, "locked then closed", "open", 2)
	// Lock it (status stays open) → on the surface via the locked_by arm.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET locked_by='mcp-session-s2', locked_at=now() WHERE id=$1`, id); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if !hasIssue(t, store, pid, id) {
		t.Fatalf("locked open issue %s should be on the surface", id)
	}

	// Close it while still locked. The `status <> 'closed'` clause must
	// drop it despite locked_by being non-NULL.
	testsupport.SetStatus(t, pool, id, "closed")
	if hasIssue(t, store, pid, id) {
		t.Fatalf("closed-but-locked issue %s must NOT be on the surface (status<>'closed' wins)", id)
	}
}

// TestInProgress_StandingExcluded pins the allow-list assumption behind the
// raw `i.status <> 'closed'` filter at store.go:1226. The InProgress surface
// predicate is `(status='in_progress' OR locked_by IS NOT NULL) AND status <>
// 'closed'`. A standing issue is neither in_progress nor closed, and standing
// never holds a lock (locked_by stays NULL) — so it must NEVER appear on the
// dashboard in-progress/active surface. The store.go comment allow-lists the
// status-blacklist (rather than a status-whitelist) precisely BECAUSE standing
// can't reach this surface via the lock arm; this test makes that assumption a
// guarded invariant rather than a comment.
//
// We assert both arms of the predicate:
//  1. a plain standing issue (no lock) is absent — it can't enter via the
//     status arm (status != in_progress) nor the lock arm (locked_by NULL);
//  2. promoting an in_progress issue to standing REMOVES it from the surface,
//     proving standing is treated as non-active even on transition.
func TestInProgress_StandingExcluded(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := New(pool)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tsdst")

	// A plain standing issue (no lock) — must be absent.
	standing := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, standing, "perpetual", "standing", 2)
	if hasIssue(t, store, pid, standing) {
		t.Fatalf("standing issue %s must NOT be on the in-progress surface (status arm: not in_progress; lock arm: no lock)", standing)
	}

	// A control in_progress issue — present — so we know the surface is live
	// and the standing exclusion above isn't a false negative from an empty
	// result.
	active := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, owner, active, "active", "in_progress", 2)
	if !hasIssue(t, store, pid, active) {
		t.Fatalf("control in_progress issue %s should be on the surface", active)
	}
	if n := inProgressCountForProject(t, store, pid); n != 1 {
		t.Fatalf("surface count = %d, want 1 (only the in_progress control; standing excluded)", n)
	}

	// Promote the control in_progress issue to standing → it must LEAVE the
	// surface. (SetStatus does not touch locked_by, and standing never locks,
	// so the lock arm stays NULL.)
	testsupport.SetStatus(t, pool, active, "standing")
	if hasIssue(t, store, pid, active) {
		t.Fatalf("issue %s promoted to standing must leave the in-progress surface", active)
	}
	if n := inProgressCountForProject(t, store, pid); n != 0 {
		t.Fatalf("surface count after promote-to-standing = %d, want 0", n)
	}
}

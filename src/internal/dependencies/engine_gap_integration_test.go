package dependencies

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

// sk-105.4 (S3 engine gaps) — dependency-store invariants:
//   - cross-project dep visibility on create (dependency_target_not_visible)
//   - cross-project edge redaction on read
//   - dep_remove on a missing edge → dependency_not_found (not silent)
//   - ListReady limit clamping to [1, 100] / default 50
//
// All assertions are project/entity-scoped; DB-backed, skip when
// TEST_DATABASE_URL is unset (testsupport.NewTestPool).

// TestDepCreate_CrossProjectTargetNotVisible verifies that a caller who is
// NOT a member of the TO-side project cannot create a cross-project edge
// into it — Create returns ErrDependencyTargetNotVisible.
func TestDepCreate_CrossProjectTargetNotVisible(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	// owner owns BOTH projects; member only belongs to projA.
	owner, _ := testsupport.SeedUser(t, pool, "user")
	member, _ := testsupport.SeedUser(t, pool, "user")

	pidA, prefixA := testsupport.SeedProject(t, pool, owner, "depvisA")
	pidB, prefixB := testsupport.SeedProject(t, pool, owner, "depvisB")
	testsupport.AddProjectMember(t, pool, pidA, member, "member")

	fromID := prefixA + "-1"
	toID := prefixB + "-1"
	testsupport.SeedIssue(t, pool, pidA, owner, fromID, "from", "open", 2)
	testsupport.SeedIssue(t, pool, pidB, owner, toID, "to", "open", 2)

	store := NewStore(pool)

	// member can see projA (FROM) but NOT projB (TO) → rejected.
	_, err := store.Create(ctx, fromID,
		CreateDepInput{ToID: toID, Type: "related"},
		CallerAccess{UserID: member, Role: "user"},
		Actor{UserID: member, Username: "member", Role: "human"},
	)
	if !errors.Is(err, ErrDependencyTargetNotVisible) {
		t.Fatalf("expected ErrDependencyTargetNotVisible, got %v", err)
	}

	// Sanity: the owner (member of both) CAN create the same edge.
	if _, err := store.Create(ctx, fromID,
		CreateDepInput{ToID: toID, Type: "related"},
		CallerAccess{UserID: owner, Role: "user"},
		Actor{UserID: owner, Username: "owner", Role: "human"},
	); err != nil {
		t.Fatalf("owner (member of both projects) should create the edge: %v", err)
	}
}

// TestDepList_RedactsInvisibleCrossProjectEdge verifies that on read, an
// edge into a project the caller cannot see has its issue payload redacted
// (Redacted=true, Issue=nil) while the edge itself stays visible.
func TestDepList_RedactsInvisibleCrossProjectEdge(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	viewer, _ := testsupport.SeedUser(t, pool, "user")

	pidA, prefixA := testsupport.SeedProject(t, pool, owner, "depredA")
	pidB, prefixB := testsupport.SeedProject(t, pool, owner, "depredB")
	testsupport.AddProjectMember(t, pool, pidA, viewer, "member")

	fromID := prefixA + "-1"
	toID := prefixB + "-1"
	testsupport.SeedIssue(t, pool, pidA, owner, fromID, "from", "open", 2)
	testsupport.SeedIssue(t, pool, pidB, owner, toID, "to-secret", "open", 2)
	testsupport.SeedDep(t, pool, fromID, toID, "related", owner)

	store := NewStore(pool)

	// viewer sees only projA. The outgoing edge points at projB (invisible)
	// → redacted.
	list, err := store.List(ctx, fromID, authz.ForUser(viewer))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Outgoing) != 1 {
		t.Fatalf("expected 1 outgoing edge, got %d", len(list.Outgoing))
	}
	edge := list.Outgoing[0]
	if !edge.Redacted {
		t.Fatalf("cross-project edge into invisible project should be redacted")
	}
	if edge.Issue != nil {
		t.Fatalf("redacted edge must have nil Issue payload, got %+v", edge.Issue)
	}
	// The edge itself stays visible (the caller knows a link exists), but
	// its hidden end is a synthetic token, never the real id (sk-674.18
	// T009).
	if edge.ToID != "hidden-1" || edge.Type != "related" {
		t.Fatalf("edge envelope wrong: toID=%q type=%q, want hidden-1 / related", edge.ToID, edge.Type)
	}

	// Control: the owner (member of both) sees the payload un-redacted.
	ownerList, err := store.List(ctx, fromID, authz.ForUser(owner))
	if err != nil {
		t.Fatalf("List owner: %v", err)
	}
	if len(ownerList.Outgoing) != 1 || ownerList.Outgoing[0].Redacted {
		t.Fatalf("owner should see the edge un-redacted, got %+v", ownerList.Outgoing)
	}
	if ownerList.Outgoing[0].Issue == nil || ownerList.Outgoing[0].Issue.Title != "to-secret" {
		t.Fatalf("owner should see TO-side title, got %+v", ownerList.Outgoing[0].Issue)
	}
}

// TestDepRemove_NotFound verifies that removing a non-existent edge returns
// ErrNotFound ("dependency not found") rather than a silent success — a
// stale dep list must surface so the caller re-reads.
func TestDepRemove_NotFound(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "deprm")
	a := prefix + "-1"
	b := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, owner, a, "a", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, b, "b", "open", 2)

	store := NewStore(pool)
	actor := Actor{UserID: owner, Username: "owner", Role: "human"}

	// No edge exists between a and b → remove must report not-found.
	err := store.Delete(ctx, a, b, "related", actor)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound removing missing edge, got %v", err)
	}

	// Now seed the edge, remove it once (success), remove again (not-found).
	testsupport.SeedDep(t, pool, a, b, "related", owner)
	if err := store.Delete(ctx, a, b, "related", actor); err != nil {
		t.Fatalf("first delete of existing edge should succeed: %v", err)
	}
	if err := store.Delete(ctx, a, b, "related", actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete must report ErrNotFound (not silent), got %v", err)
	}
}

// TestListReady_LimitBoundaries verifies the limit clamp: <1 → 50, >100 →
// 100, in-range passes through. We seed 3 ready issues and assert the
// effective cap by counting returned rows for small explicit limits, plus
// assert the clamp does not error for the boundary inputs.
func TestListReady_LimitBoundaries(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "rdylim")

	// Seed 3 open (ready) issues in this project.
	for i := 1; i <= 3; i++ {
		testsupport.SeedIssue(t, pool, pid, owner, prefix+"-"+strconv.Itoa(i), "ready", "open", 2)
	}

	store := NewStore(pool)

	// limit=2 (in-range) → at most 2 of OUR project's ready issues.
	got, err := store.ListReady(ctx, pid, ReadyFilters{}, 2)
	if err != nil {
		t.Fatalf("ListReady limit=2: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 should return 2 rows, got %d", len(got))
	}

	// limit=0 → clamped to default 50; our 3 issues all returned (the
	// project only has 3, so the clamp can't be observed as a cap here,
	// but the call must not error and must return all 3).
	got, err = store.ListReady(ctx, pid, ReadyFilters{}, 0)
	if err != nil {
		t.Fatalf("ListReady limit=0 (→default 50): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=0 (default 50) should return all 3 ready issues, got %d", len(got))
	}

	// limit=-5 (negative) → clamped to default 50 (no error, all 3).
	got, err = store.ListReady(ctx, pid, ReadyFilters{}, -5)
	if err != nil {
		t.Fatalf("ListReady limit=-5 (→default 50): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=-5 (default 50) should return all 3, got %d", len(got))
	}

	// limit=1000 (over cap) → clamped to 100 (no error). Our project has
	// only 3 ready issues, so all 3 come back and the call doesn't blow up
	// on an out-of-range LIMIT.
	got, err = store.ListReady(ctx, pid, ReadyFilters{}, 1000)
	if err != nil {
		t.Fatalf("ListReady limit=1000 (→clamp 100): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=1000 (clamped 100) should return all 3 ready issues, got %d", len(got))
	}

	// limit=1 → exactly 1 row (cap is honoured below 50).
	got, err = store.ListReady(ctx, pid, ReadyFilters{}, 1)
	if err != nil {
		t.Fatalf("ListReady limit=1: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("limit=1 should return exactly 1 row, got %d", len(got))
	}
}

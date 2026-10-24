package projects

import (
	"testing"

	"skein-api/internal/testsupport"
)

// sk-118.8 (F2g) — standing is excluded from the active project pills
// (open / in_progress / blocked / deferred / myAssigned) via the widened
// status.ActiveWork helper, and surfaced as its OWN `standingCount`
// aggregate on project + component stats (decision #4).

// TestProjectStats_StandingExcludedFromOpenInProgressBlockedDeferred asserts a
// standing issue lands in NONE of the active pills. Closed/deferred/blocked are
// the per-status pills; open/in_progress key on equality (auto-exclude); blocked
// derives through ActiveWork (now excludes standing). The only place the standing
// row shows up is standingCount and total.
func TestProjectStats_StandingExcludedFromOpenInProgressBlockedDeferred(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspstand")
	store := NewStore(pool)

	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, id, "perpetual", "open", 2)

	// Snapshot: one open issue.
	s := statsFor(t, store, pid, owner)
	if s.Open != 1 || s.Standing != 0 {
		t.Fatalf("snapshot: open=%d standing=%d, want 1/0", s.Open, s.Standing)
	}

	// Promote to standing: must drop out of EVERY active pill.
	testsupport.SetStatus(t, pool, id, "standing")
	s = statsFor(t, store, pid, owner)
	if s.Open != 0 || s.InProgress != 0 || s.Blocked != 0 || s.Deferred != 0 || s.Closed != 0 {
		t.Fatalf("after standing: active pills not all zero — open=%d in_progress=%d blocked=%d deferred=%d closed=%d",
			s.Open, s.InProgress, s.Blocked, s.Deferred, s.Closed)
	}
	if s.Total != 1 {
		t.Fatalf("after standing: total=%d, want 1 (standing still counts in total)", s.Total)
	}
}

// TestProjectStats_StandingCountReported asserts standingCount = N for N standing
// issues, alongside a separate open and closed issue to prove the buckets stay
// disjoint and the closed leg is unchanged.
func TestProjectStats_StandingCountReported(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspscnt")
	store := NewStore(pool)

	// 2 standing, 1 open, 1 closed.
	testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "standing one", "standing", 2)
	testsupport.SeedIssue(t, pool, pid, owner, prefix+"-2", "standing two", "standing", 2)
	testsupport.SeedIssue(t, pool, pid, owner, prefix+"-3", "open one", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, prefix+"-4", "closed one", "closed", 2)

	s := statsFor(t, store, pid, owner)
	if s.Standing != 2 {
		t.Fatalf("standingCount = %d, want 2", s.Standing)
	}
	if s.Open != 1 {
		t.Fatalf("open = %d, want 1 (standing not folded into open)", s.Open)
	}
	if s.Closed != 1 {
		t.Fatalf("closed = %d, want 1 (closed leg unchanged)", s.Closed)
	}
	if s.Total != 4 {
		t.Fatalf("total = %d, want 4", s.Total)
	}
}

// TestProjectStats_MyAssignedExcludesStanding pins the §11.3 / decision-#4
// invariant that the caller-scoped `MyAssigned` aggregate (which routes through
// status.ActiveWork in GetProjectWithStats, store.go:351-357) drops a standing
// issue exactly like the open/in_progress/blocked pills do. The ProjectStats
// doc-comment promises "standing issues are EXCLUDED from ... MyAssigned"; this
// test is the guard that keeps a future ActiveWork refactor from silently
// re-folding standing back into the caller's assigned count. Seed → assert →
// promote-to-standing → re-assert (the canonical leak-catching pattern).
func TestProjectStats_MyAssignedExcludesStanding(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	ownerName := testsupport.Username(t, pool, owner)
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspmyas")
	store := NewStore(pool)

	// One open issue assigned to the caller (owner) → counts in MyAssigned.
	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, id, "assigned to me", "open", 2)
	testsupport.SetAssignee(t, pool, id, ownerName)

	// Snapshot: open=1, myAssigned=1, standing=0.
	s := statsFor(t, store, pid, owner)
	if s.Open != 1 || s.MyAssigned != 1 || s.Standing != 0 {
		t.Fatalf("snapshot: open=%d myAssigned=%d standing=%d, want 1/1/0", s.Open, s.MyAssigned, s.Standing)
	}

	// Promote to standing → must drop out of MyAssigned (still assigned, but a
	// standing task is a perpetual obligation, not active assigned work) AND out
	// of open, landing only in the standing aggregate.
	testsupport.SetStatus(t, pool, id, "standing")
	s = statsFor(t, store, pid, owner)
	if s.MyAssigned != 0 {
		t.Fatalf("myAssigned after promote-to-standing = %d, want 0 (standing excluded from caller's assigned count)", s.MyAssigned)
	}
	if s.Open != 0 {
		t.Fatalf("open after promote-to-standing = %d, want 0", s.Open)
	}
	if s.Standing != 1 {
		t.Fatalf("standingCount after promote = %d, want 1", s.Standing)
	}
	// MyCreated is a created-by count (no status filter by design) — the standing
	// row the caller created still counts there. Asserting this pins the
	// intentional asymmetry so a future "exclude standing everywhere" sweep
	// doesn't wrongly strip MyCreated.
	if s.MyCreated != 1 {
		t.Fatalf("myCreated = %d, want 1 (created-by count is NOT status-filtered; standing still counts)", s.MyCreated)
	}
}

// TestProjectStats_ChildOfStandingCountedInOpen asserts that a NORMAL open child
// of a standing parent still counts as open — only the standing row itself is
// excluded. Standing acts via spawned children; those children are real work.
func TestProjectStats_ChildOfStandingCountedInOpen(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspchild")
	store := NewStore(pool)

	parent := prefix + "-1"
	child := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, owner, parent, "standing parent", "standing", 2)
	testsupport.SeedIssue(t, pool, pid, owner, child, "open child", "open", 2)

	s := statsFor(t, store, pid, owner)
	if s.Open != 1 {
		t.Fatalf("open = %d, want 1 (the open child must still count as open)", s.Open)
	}
	if s.Standing != 1 {
		t.Fatalf("standingCount = %d, want 1 (only the standing parent)", s.Standing)
	}
	if s.Total != 2 {
		t.Fatalf("total = %d, want 2", s.Total)
	}
}

// TestProjectStats_OutboxModeScopedStandingCount asserts the outbox-scoped
// standing pill on the ListProjects row (myOutboxStandingCount) honors the
// Delegated predicate AND excludes standing from the active outbox counters —
// the outboxMode invariant (CLAUDE.md) must apply uniformly to the standing pill.
func TestProjectStats_OutboxModeScopedStandingCount(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tspobox")
	store := NewStore(pool)

	// Caller-created, delegated to `other`: one standing, one open.
	standing := prefix + "-1"
	open := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, caller, standing, "delegated standing", "standing", 2)
	testsupport.SetAssignee(t, pool, standing, otherName)
	testsupport.SeedIssue(t, pool, pid, caller, open, "delegated open", "open", 2)
	testsupport.SetAssignee(t, pool, open, otherName)

	row := func() ProjectListItem {
		t.Helper()
		res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
			CallerID: caller,
		})
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		for _, it := range res.Items {
			if it.ID == pid {
				return it
			}
		}
		t.Fatalf("project %s not in list", pid)
		return ProjectListItem{}
	}

	it := row()
	if it.MyOutboxStandingCount != 1 {
		t.Fatalf("myOutboxStandingCount = %d, want 1", it.MyOutboxStandingCount)
	}
	if it.MyOutboxOpenCount != 1 {
		t.Fatalf("myOutboxOpenCount = %d, want 1 (standing must NOT inflate the open outbox pill)", it.MyOutboxOpenCount)
	}
	// Unscoped standing pill on the same row also reports the standing issue.
	if it.StandingCount != 1 {
		t.Fatalf("standingCount = %d, want 1", it.StandingCount)
	}
	if it.OpenCount != 1 {
		t.Fatalf("openCount = %d, want 1 (standing excluded from open)", it.OpenCount)
	}
}

// TestListProjects_LensDelegated_ExcludesStandingOnlyProject is the R5 lens test:
// a project whose ONLY delegated issue is standing must be dropped from the
// lens=delegated list, because buildLensHaving routes through status.ActiveWork
// (now excludes standing). Confirms the lens HAVING and the per-row counters stay
// consistent — the project neither appears in the lens nor reports an inflated
// delegated pill.
func TestListProjects_LensDelegated_ExcludesStandingOnlyProject(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	otherName := testsupport.Username(t, pool, other)
	store := NewStore(pool)

	// Project A: a delegated OPEN issue → survives lens=delegated.
	pidA, prefA := testsupport.SeedProject(t, pool, caller, "tslnsa")
	openID := prefA + "-1"
	testsupport.SeedIssue(t, pool, pidA, caller, openID, "delegated open", "open", 2)
	testsupport.SetAssignee(t, pool, openID, otherName)

	// Project B: a delegated STANDING issue (and nothing else) → dropped.
	pidB, prefB := testsupport.SeedProject(t, pool, caller, "tslnsb")
	standID := prefB + "-1"
	testsupport.SeedIssue(t, pool, pidB, caller, standID, "delegated standing", "standing", 2)
	testsupport.SetAssignee(t, pool, standID, otherName)

	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
		Lens:     "delegated",
	})
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var hasA, hasB bool
	for _, it := range res.Items {
		if it.ID == pidA {
			hasA = true
		}
		if it.ID == pidB {
			hasB = true
		}
	}
	if !hasA {
		t.Errorf("project A (delegated open) missing from lens=delegated list")
	}
	if hasB {
		t.Errorf("project B (only a delegated standing issue) returned in lens=delegated — standing must not keep a project in an active lens")
	}
}

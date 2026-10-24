package issues

import (
	"testing"
)

// F2h (sk-118.9, §11.5): a `standing` child is INVISIBLE to the epic
// rollup. It must NOT count toward Total, must NOT inflate OpenChildren /
// InProgressChildren / BlockedChildren, and must NOT keep the epic from
// being eligible-for-close. It is surfaced ONLY in the dedicated
// StandingChildren bucket. These tests pin that contract; the normal
// (non-standing) rollup semantics are guarded by the pre-existing tests in
// epic_rollup_test.go (which must stay green).

// TestEpicRollup_StandingChildExcludedFromTotal: an epic with one open
// child and one standing child reports TotalChildren = 1 (the standing
// child excluded) and StandingChildren = 1.
func TestEpicRollup_StandingChildExcludedFromTotal(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)

	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "open child", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "standing child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "standing")

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic == nil {
		t.Fatalf("Epic rollup nil; want populated for epic-typed issue")
	}
	got := *dto.Epic
	if got.TotalChildren != 1 {
		t.Errorf("TotalChildren = %d, want 1 (standing child excluded)", got.TotalChildren)
	}
	if got.OpenChildren != 1 {
		t.Errorf("OpenChildren = %d, want 1", got.OpenChildren)
	}
	if got.StandingChildren != 1 {
		t.Errorf("StandingChildren = %d, want 1", got.StandingChildren)
	}
}

// TestEpicRollup_EpicWithOnlyStandingChild_NotEligibleButCloseable
// follows §11.5: standing children are excluded from Total, so an epic
// whose ONLY child is standing has TotalChildren == 0. Per the empty-epic
// contract (Total > 0 required) this leaves EligibleForClose = false — the
// "Not eligible" half of the test name. Crucially the epic is still
// CLOSEABLE: there are zero countable open children to block a close. (No
// store-level hard block exists on epic close; the eligible flag is a UI
// affordance — §11.9 — and EpicOpenChildrenFor omits the standing child,
// so the close warning is empty.) The name's "NotEligibleButCloseable"
// captures exactly this: flag=false yet nothing strands the close.
func TestEpicRollup_EpicWithOnlyStandingChild_NotEligibleButCloseable(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "standing child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.1", "standing")

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic == nil {
		t.Fatalf("Epic rollup nil; want populated")
	}
	got := *dto.Epic
	if got.TotalChildren != 0 {
		t.Errorf("TotalChildren = %d, want 0 (only child is standing → excluded)", got.TotalChildren)
	}
	if got.StandingChildren != 1 {
		t.Errorf("StandingChildren = %d, want 1", got.StandingChildren)
	}
	// Empty countable set → not eligible (Total > 0 gate), but nothing
	// blocks the close: EpicOpenChildrenFor must omit the standing child.
	if got.EligibleForClose {
		t.Errorf("EligibleForClose = true; want false (no countable children, Total == 0)")
	}
	openChildren, err := store.EpicOpenChildrenFor(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("EpicOpenChildrenFor: %v", err)
	}
	if len(openChildren) != 0 {
		t.Errorf("EpicOpenChildrenFor returned %d children; want 0 (standing omitted, close not stranded)", len(openChildren))
	}
}

// TestEpicRollup_EpicWithStandingPlusAllClosed_Eligible is the positive
// flip of the contract: an epic with closed children AND a standing child
// is eligible-for-close. Because standing drops out of Total, Closed ==
// Total holds → EligibleForClose = true. This is the load-bearing §11.5
// guarantee that a standing child never strands an otherwise-finished epic.
func TestEpicRollup_EpicWithStandingPlusAllClosed_Eligible(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "closed child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.1", "closed")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "standing child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "standing")

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	got := *dto.Epic
	if got.TotalChildren != 1 || got.ClosedChildren != 1 {
		t.Errorf("counters = total %d / closed %d; want 1/1 (standing excluded from total)",
			got.TotalChildren, got.ClosedChildren)
	}
	if got.StandingChildren != 1 {
		t.Errorf("StandingChildren = %d, want 1", got.StandingChildren)
	}
	if !got.EligibleForClose {
		t.Errorf("EligibleForClose = false; want true (all countable children closed; standing excluded)")
	}
}

// TestEpicRollup_EpicOpenChildren_OmitsStanding: EpicOpenChildrenFor (the
// close-warning list) must omit standing children even though they are
// non-closed. It already routes through status.ActiveWork (F2g-widened);
// this asserts the end-to-end behavior.
func TestEpicRollup_EpicOpenChildren_OmitsStanding(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "open child", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "standing child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "standing")

	store := NewStore(pool)
	openChildren, err := store.EpicOpenChildrenFor(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("EpicOpenChildrenFor: %v", err)
	}
	if len(openChildren) != 1 {
		t.Fatalf("EpicOpenChildrenFor returned %d children; want 1 (only the open child)", len(openChildren))
	}
	if openChildren[0].ID != prefix+"-1.1" {
		t.Errorf("open child = %q; want %q (standing must be omitted)", openChildren[0].ID, prefix+"-1.1")
	}
}

// TestEpicRollup_BlockedChildren_OmitsStandingWithActiveBlocker is the
// F2g regression (matrix row 96): the BlockedChildren FILTER's leg
// was unaliased on the `children` CTE and missed the F2g widening. F2h
// widened it to NOT IN ('closed','standing'); sk-421.2 (R-11) then NARROWED
// it to `status IN ('open','in_progress')`, which admits neither standing
// nor deferred children. A standing child WITH an active (open) blocker
// must NOT inflate BlockedChildren → assert BlockedChildren == 0.
func TestEpicRollup_BlockedChildren_OmitsStandingWithActiveBlocker(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "epic", TypeEpic, nil)

	standingChild := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, standingChild, "standing child", TypeTask, &epicID)
	setIssueStatus(t, pool, standingChild, "standing")

	// An OPEN external blocker the standing child blocks-on (from = blocked
	// side, to = blocker). The blocker is open → it would gate readiness.
	blocker := prefix + "-2"
	seedTypedIssue(t, pool, pid, owner, blocker, "open external blocker", TypeTask, nil)
	seedDep(t, pool, standingChild, blocker, "blocks", owner)

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	got := *dto.Epic
	if got.BlockedChildren != 0 {
		t.Errorf("BlockedChildren = %d, want 0 (standing child must not inflate blocked count even with active blocker)", got.BlockedChildren)
	}
	if got.StandingChildren != 1 {
		t.Errorf("StandingChildren = %d, want 1", got.StandingChildren)
	}
	if got.TotalChildren != 0 {
		t.Errorf("TotalChildren = %d, want 0 (only child is standing)", got.TotalChildren)
	}
}

// TestEpicProgress_StandingChildExcludedFromTotal (R11): the inline
// (closed/total) progress chip surfaced on list/tree rows via
// IssueSummary.EpicProgress (epicSummaryTotalExpr) must exclude the
// standing child from the Total leg. An epic with 1 closed + 1 standing
// child reports EpicProgress{Closed:1, Total:1}, not Total:2.
func TestEpicProgress_StandingChildExcludedFromTotal(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "closed child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.1", "closed")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "standing child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "standing")

	store := NewStore(pool)
	res, err := store.ListIssues(storeFixtureCtx(), ListIssuesArgs{
		ProjectID: pid,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	var found bool
	for _, it := range res.Items {
		if it.ID != epicID {
			continue
		}
		found = true
		if it.EpicProgress == nil {
			t.Fatalf("epic %s EpicProgress nil; want populated", it.ID)
		}
		if it.EpicProgress.Total != 1 || it.EpicProgress.Closed != 1 {
			t.Errorf("epic %s EpicProgress = {Closed=%d Total=%d}; want {1, 1} (standing excluded from total)",
				it.ID, it.EpicProgress.Closed, it.EpicProgress.Total)
		}
	}
	if !found {
		t.Fatalf("epic %s not found in list", epicID)
	}
}

package issues

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
)

// seedTypedIssue is like seedIssue but lets the caller pick the type +
// optional parent_id. Used by the epic rollup tests to fabricate epic-
// and-children fixtures without going through CreateIssue.
//
// sk-35: when parentID is non-nil we ALSO write the canonical
// parent-child dep edge, because the readers (epic rollup, parent
// projection, list filters) now derive parent from the dep graph. The
// issues.parent_id column write stays as well — it's harmless under
// the frozen-column policy (column unread by the main code path) and
// preserves any future tests that still inspect it.
func seedTypedIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID,
	idStr, title, issueType string, parentID *string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, $3, 1, $4, '', $5, 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $6, now(), now(), '{}'::jsonb)`,
		idStr, projectID, parentID, title, issueType, creator)
	if err != nil {
		t.Fatalf("seed typed issue %s: %v", idStr, err)
	}
	if parentID != nil && *parentID != "" {
		seedParentChild(t, pool, idStr, *parentID, creator)
	}
}

// seedParentChild writes the canonical sk-35 parent-child dep edge
// (child→parent). Tests that seed issue rows with parent_id set must
// call this so the new reader paths (which derive parent from the dep
// graph) see the hierarchy.
func seedParentChild(t *testing.T, pool *pgxpool.Pool, childID, parentID string, creator uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())
ON CONFLICT (from_id, to_id, type) DO NOTHING`,
		childID, parentID, creator)
	if err != nil {
		t.Fatalf("seed parent-child %s -> %s: %v", childID, parentID, err)
	}
}

// TestEpicRollup_HierarchicalChildrenMixedStatuses verifies the five
// counters compute correctly when an epic has 5 hierarchical children
// (parent_id = epic.id) in mixed statuses. Validates the canonical
// expected mapping → wire shape.
func TestEpicRollup_HierarchicalChildrenMixedStatuses(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)

	// 1 open, 1 in_progress, 1 deferred, 2 closed. Closed children get
	// a closed_at set via setIssueStatus.
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "open child", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "wip child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "in_progress")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.3", "deferred child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.3", "deferred")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.4", "closed child a", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.4", "closed")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.5", "closed child b", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.5", "closed")

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic == nil {
		t.Fatalf("Epic rollup nil; want populated for epic-typed issue")
	}
	got := *dto.Epic
	if got.TotalChildren != 5 {
		t.Errorf("TotalChildren = %d, want 5", got.TotalChildren)
	}
	if got.OpenChildren != 1 {
		t.Errorf("OpenChildren = %d, want 1", got.OpenChildren)
	}
	if got.InProgressChildren != 1 {
		t.Errorf("InProgressChildren = %d, want 1", got.InProgressChildren)
	}
	if got.ClosedChildren != 2 {
		t.Errorf("ClosedChildren = %d, want 2", got.ClosedChildren)
	}
	if got.BlockedChildren != 0 {
		t.Errorf("BlockedChildren = %d, want 0 (no blocker edges)", got.BlockedChildren)
	}
	if got.EligibleForClose {
		t.Errorf("EligibleForClose = true; want false (2 of 5 closed)")
	}
}

// TestEpicRollup_EmptyEpic confirms an epic with no children reports
// TotalChildren=0 and EligibleForClose=false. Empty epics never become
// eligible (the UX contract — would be a false signal).
func TestEpicRollup_EmptyEpic(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "lone epic", TypeEpic, nil)

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic == nil {
		t.Fatalf("Epic rollup nil; want populated for epic-typed issue")
	}
	got := *dto.Epic
	if got.TotalChildren != 0 {
		t.Errorf("TotalChildren = %d, want 0", got.TotalChildren)
	}
	if got.EligibleForClose {
		t.Errorf("EligibleForClose = true on empty epic; want false")
	}
}

// (sk-33) The former TestEpicRollup_UnionHierarchicalAndDeps was
// removed: parent-child as a dep type was dropped, and parent_id is now
// the sole source of epic children. The TestEpicRollup_HierarchicalChildrenMixedStatuses
// above already exercises the parent_id path comprehensively.

// TestEpicRollup_EligibleForClose verifies the eligible-for-close gate:
// every child closed AND TotalChildren > 0 → eligible.
func TestEpicRollup_EligibleForClose(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)

	for i := 1; i <= 3; i++ {
		seedTypedIssue(t, pool, pid, owner, prefix+"-1."+itoa(i), "child "+itoa(i), TypeTask, &epicID)
		setIssueStatus(t, pool, prefix+"-1."+itoa(i), "closed")
	}

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !dto.Epic.EligibleForClose {
		t.Errorf("EligibleForClose = false; want true (all 3 closed)")
	}
	if dto.Epic.TotalChildren != 3 || dto.Epic.ClosedChildren != 3 {
		t.Errorf("counters off: total=%d closed=%d; want 3/3",
			dto.Epic.TotalChildren, dto.Epic.ClosedChildren)
	}
}

// TestEpicRollup_BlockedChildIsBothOpenAndBlocked checks that a blocked
// child which is itself OPEN contributes to both OpenChildren and
// BlockedChildren. Since sk-421.2 (R-11, owner answer N-1a) that overlap is
// the whole of it: BlockedChildren is a SUBSET of OpenChildren +
// InProgressChildren, so the epic card reads it as a breakdown of "not
// done" (B <= Y) rather than a free-floating attention flag. A DEFERRED
// child with an active blocker is counted in DeferredChildren only — see
// TestEpicRollup_DeferredChildWithBlockerIsNotBlocked.
func TestEpicRollup_BlockedChildIsBothOpenAndBlocked(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "epic", TypeEpic, nil)
	childID := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, childID, "child", TypeTask, &epicID)
	// Blocker outside the epic
	seedTypedIssue(t, pool, pid, owner, prefix+"-2", "external blocker", TypeTask, nil)
	seedDep(t, pool, childID, prefix+"-2", "blocks", owner)

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic.OpenChildren != 1 {
		t.Errorf("OpenChildren = %d, want 1", dto.Epic.OpenChildren)
	}
	if dto.Epic.BlockedChildren != 1 {
		t.Errorf("BlockedChildren = %d, want 1", dto.Epic.BlockedChildren)
	}
}

// TestEpicRollup_NilForNonEpic confirms IssueDetail.Epic stays nil for
// non-epic types (task, bug, feature, ...). Saves the round-trip on
// every non-epic GetIssue call.
func TestEpicRollup_NilForNonEpic(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "not an epic")

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic != nil {
		t.Errorf("Epic rollup non-nil on task-typed issue: %+v", *dto.Epic)
	}
}

// TestEpicProgress_OnSummary verifies IssueSummary.EpicProgress is
// populated for epic rows in list responses and nil for non-epic rows.
// Slice 2 of sk-32.
func TestEpicProgress_OnSummary(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	// One epic with 4 children (1 closed)
	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	for i := 1; i <= 4; i++ {
		seedTypedIssue(t, pool, pid, owner, prefix+"-1."+itoa(i), "child "+itoa(i), TypeTask, &epicID)
	}
	setIssueStatus(t, pool, prefix+"-1.1", "closed")
	// One regular task to confirm non-epics have EpicProgress=nil
	seedIssue(t, pool, pid, owner, prefix+"-2", "regular task")

	store := NewStore(pool)
	res, err := store.ListIssues(storeFixtureCtx(), ListIssuesArgs{
		ProjectID: pid,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	var foundEpic, foundTask bool
	for _, it := range res.Items {
		switch it.ID {
		case epicID:
			foundEpic = true
			if it.EpicProgress == nil {
				t.Errorf("epic %s EpicProgress nil; want populated", it.ID)
				break
			}
			if it.EpicProgress.Total != 4 || it.EpicProgress.Closed != 1 {
				t.Errorf("epic %s EpicProgress = {Closed=%d Total=%d}; want {1, 4}",
					it.ID, it.EpicProgress.Closed, it.EpicProgress.Total)
			}
		case prefix + "-2":
			foundTask = true
			if it.EpicProgress != nil {
				t.Errorf("task %s EpicProgress non-nil: %+v", it.ID, *it.EpicProgress)
			}
		}
	}
	if !foundEpic || !foundTask {
		t.Fatalf("expected to find both epic + task in list; foundEpic=%v foundTask=%v",
			foundEpic, foundTask)
	}
}

// TestEpicRollup_Reparenting_sk35 walks the sk-35 reparenting path:
// build epic A with one child, then move the child to epic B by
// deleting the A→child parent-child edge and inserting B→child. The
// rollup on A should drop to zero children; B's should reach one.
func TestEpicRollup_Reparenting_sk35(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicA := prefix + "-1"
	epicB := prefix + "-2"
	child := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, epicA, "epic a", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, epicB, "epic b", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, child, "the child", TypeTask, &epicA)

	store := NewStore(pool)
	a0, err := store.GetIssue(storeFixtureCtx(), epicA)
	if err != nil {
		t.Fatalf("GetIssue A: %v", err)
	}
	if a0.Epic == nil || a0.Epic.TotalChildren != 1 {
		t.Fatalf("initial A total = %v; want 1", a0.Epic)
	}

	// Re-parent: drop edge A and add edge B (multi-step that the
	// MoveIssueButton / MCP-driven flow would emit).
	if _, err := pool.Exec(context.Background(), `
DELETE FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'parent-child'`,
		child, epicA); err != nil {
		t.Fatalf("delete A edge: %v", err)
	}
	seedParentChild(t, pool, child, epicB, owner)

	a1, err := store.GetIssue(storeFixtureCtx(), epicA)
	if err != nil {
		t.Fatalf("GetIssue A after move: %v", err)
	}
	if a1.Epic == nil || a1.Epic.TotalChildren != 0 {
		t.Errorf("A total after move = %v; want 0", a1.Epic)
	}
	b1, err := store.GetIssue(storeFixtureCtx(), epicB)
	if err != nil {
		t.Fatalf("GetIssue B: %v", err)
	}
	if b1.Epic == nil || b1.Epic.TotalChildren != 1 {
		t.Errorf("B total after move = %v; want 1", b1.Epic)
	}
}

// TestEpicRollup_MultiParent_FirstWinsTree exercises the sk-35
// multi-parent allowance: a child may carry two parent-child edges.
// Both parents see the child in their rollup count (the rollup is
// graph-driven), but the breadcrumb / tree picks the first parent by
// created_at (which we verify through the IssueDetail.ParentID).
func TestEpicRollup_MultiParent_FirstWinsTree(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicA := prefix + "-1"
	epicB := prefix + "-2"
	child := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, epicA, "epic a", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, epicB, "epic b", TypeEpic, nil)
	// seedTypedIssue inserts the first parent-child edge (child→A).
	seedTypedIssue(t, pool, pid, owner, child, "child", TypeTask, &epicA)
	// Now add a SECOND edge — child→B. With sk-35 there's no UNIQUE
	// on the (from_id, type) pair so both rows coexist.
	seedParentChild(t, pool, child, epicB, owner)

	store := NewStore(pool)
	d, err := store.GetIssue(storeFixtureCtx(), child)
	if err != nil {
		t.Fatalf("GetIssue child: %v", err)
	}
	// Tree picks first by created_at — should be epicA (seeded first).
	if d.ParentID == nil || *d.ParentID != epicA {
		t.Errorf("ParentID = %v; want %q (first edge by created_at)", d.ParentID, epicA)
	}

	// Both epics count the child in their rollup.
	a, _ := store.GetIssue(storeFixtureCtx(), epicA)
	b, _ := store.GetIssue(storeFixtureCtx(), epicB)
	if a.Epic == nil || a.Epic.TotalChildren != 1 {
		t.Errorf("epic A total = %v; want 1", a.Epic)
	}
	if b.Epic == nil || b.Epic.TotalChildren != 1 {
		t.Errorf("epic B total = %v; want 1", b.Epic)
	}
}

// TestEpicRollup_DeferredChildWithBlockerIsNotBlocked pins the sk-421.2
// contract change (R-11, owner answer N-1a): BlockedChildren counts only
// children that are themselves open or in progress. A DEFERRED child with
// an active blocker lands in DeferredChildren and NOT in BlockedChildren,
// so the epic card can read "blocked B" as a breakdown of "not done Y"
// (B <= Y) instead of a free-floating flag.
//
// RED BEFORE THE FIX: the previous leg was `status NOT IN
// ('closed','standing')`, which counted the deferred child too and made
// BlockedChildren = 2 here.
func TestEpicRollup_DeferredChildWithBlockerIsNotBlocked(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "epic", TypeEpic, nil)

	deferredID := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, deferredID, "deferred child", TypeTask, &epicID)
	setIssueStatus(t, pool, deferredID, "deferred")

	openID := prefix + "-1.2"
	seedTypedIssue(t, pool, pid, owner, openID, "open child", TypeTask, &epicID)

	// One blocker outside the epic, gating BOTH children.
	blockerID := prefix + "-2"
	seedTypedIssue(t, pool, pid, owner, blockerID, "external blocker", TypeTask, nil)
	seedDep(t, pool, deferredID, blockerID, "blocks", owner)
	seedDep(t, pool, openID, blockerID, "blocks", owner)

	store := NewStore(pool)
	dto, err := store.GetIssue(storeFixtureCtx(), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic == nil {
		t.Fatalf("Epic rollup nil; want populated for epic-typed issue")
	}
	got := *dto.Epic

	if got.DeferredChildren != 1 {
		t.Errorf("DeferredChildren = %d, want 1", got.DeferredChildren)
	}
	if got.BlockedChildren != 1 {
		t.Errorf("BlockedChildren = %d, want 1 — only the OPEN child; a deferred "+
			"child with a blocker must not inflate it (R-11)", got.BlockedChildren)
	}

	// INV-1: closed + (open + in_progress) + deferred + unavailable = total.
	sum := got.ClosedChildren + got.OpenChildren + got.InProgressChildren +
		got.DeferredChildren + got.UnavailableChildren
	if sum != got.TotalChildren {
		t.Errorf("identity broken: closed+open+in_progress+deferred+unavailable = %d, "+
			"TotalChildren = %d", sum, got.TotalChildren)
	}
	// INV-1: B is a subset of Y.
	if got.BlockedChildren > got.OpenChildren+got.InProgressChildren {
		t.Errorf("B > Y: BlockedChildren = %d, open+in_progress = %d",
			got.BlockedChildren, got.OpenChildren+got.InProgressChildren)
	}
}

// TestEpicRollup_OutcomeDoesNotMoveTheStatusSide — sk-489, matrix row 23:
// INV-6 and the "Out of scope" section.
//
// The subject is the BOUNDARY between two readings of one set of children. The progress
// numbers are counted by outcome, while "can the epic be closed", the warning about
// open children and the "total / not closed" counters are counted by status, and the outcome does not
// touch them. The boundary is checked on one fixture for all three at once:
// checking one surface would leave the neighbouring one unobserved, and
// an outcome leaking into any of them looks the same — an epic that suddenly
// cannot be closed because part of the work was cancelled.
func TestEpicRollup_OutcomeDoesNotMoveTheStatusSide(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := callerCtx(t, owner, "user")
	store := NewStore(pool)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "done child", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "cancelled child", TypeTask, &epicID)
	setClosedWithOutcome(t, pool, prefix+"-1.1", CloseOutcomeDone)
	setClosedWithOutcome(t, pool, prefix+"-1.2", CloseOutcomeWontDo)

	// Half 1 — all children are closed, some without work: the epic is closable.
	dto, err := store.GetIssue(ctx, epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !dto.Epic.EligibleForClose {
		t.Errorf("eligibleForClose = false with two closed children (%+v): the flag is counted "+
			"by status, and a cancelled issue is closed exactly like a done one (INV-6)",
			*dto.Epic)
	}
	if open, err := store.EpicOpenChildrenFor(ctx, epicID); err != nil {
		t.Fatalf("EpicOpenChildrenFor: %v", err)
	} else if len(open) != 0 {
		t.Errorf("the open-children warning named %v; there can be no closed-without-work issues "+
			"among the open ones", open)
	}
	counts, err := store.CountChildren(ctx, epicID, authz.Unrestricted())
	if err != nil {
		t.Fatalf("CountChildren: %v", err)
	}
	if counts.Total != 2 || counts.Unclosed != 0 {
		t.Errorf("children counters = {total:%d unclosed:%d}, want {2 0} — they are counted by "+
			"status and do not depend on the outcome (\"Out of scope\")", counts.Total, counts.Unclosed)
	}

	// Half 2 — add an OPEN child: all three surfaces behave
	// as today, and none of them "forgives" an open child
	// because a cancelled one sits next to it.
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.3", "still open", TypeTask, &epicID)

	dto, err = store.GetIssue(ctx, epicID)
	if err != nil {
		t.Fatalf("GetIssue (2): %v", err)
	}
	if dto.Epic.EligibleForClose {
		t.Errorf("eligibleForClose = true with an open child (%+v)", *dto.Epic)
	}
	open, err := store.EpicOpenChildrenFor(ctx, epicID)
	if err != nil {
		t.Fatalf("EpicOpenChildrenFor (2): %v", err)
	}
	if len(open) != 1 || open[0].ID != prefix+"-1.3" {
		t.Errorf("the warning named %v, exactly one open child %s was expected",
			open, prefix+"-1.3")
	}
	counts, err = store.CountChildren(ctx, epicID, authz.Unrestricted())
	if err != nil {
		t.Fatalf("CountChildren (2): %v", err)
	}
	if counts.Total != 3 || counts.Unclosed != 1 {
		t.Errorf("children counters = {total:%d unclosed:%d}, want {3 1}",
			counts.Total, counts.Unclosed)
	}
	// And the progress numbers next to them are by outcome, and it is the same call: the two breakdowns
	// arrive in one response and must be different.
	if dto.Epic.DoneChildren != 1 || dto.Epic.NoWorkChildren != 1 || dto.Epic.CountedChildren != 2 {
		t.Errorf("{done:%d noWork:%d counted:%d}, want {1 1 2}", dto.Epic.DoneChildren,
			dto.Epic.NoWorkChildren, dto.Epic.CountedChildren)
	}
}

package projects

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// S2 (sk-105.3) transition-reassert coverage for the alphabetic
// project/component pill surfaces (store.go GetProjectWithStats ~305-354,
// components.go ~309-419). The existing list_counts_test.go SNAPSHOTS
// these in fixed states; this file adds the seed→assert→TRANSITION
// →re-assert pattern that catches a row failing to move between buckets.
//
// All asserts pluck the single seeded project/component and never issue a
// global COUNT(*) — concurrent packages can't perturb them.

// statsFor reads the single-project stats grid for owner.
func statsFor(t *testing.T, store *Store, pid, owner uuid.UUID) ProjectStats {
	t.Helper()
	dto, err := store.GetProjectWithStats(storeFixtureCtx(), pid, owner)
	if err != nil {
		t.Fatalf("GetProjectWithStats: %v", err)
	}
	return dto.Stats
}

// TestProjectStats_GridTransitions takes one issue through
// open→in_progress→deferred→closed and re-asserts the full grid after
// each move, plus a separate blocks-edge transition for the blocked pill.
// The disjointness invariant: open/in_progress/blocked are mutually
// exclusive; closed/deferred are their own buckets; total never moves.
func TestProjectStats_GridTransitions(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspgrid")
	store := NewStore(pool)

	// Two issues: `mover` walks the lifecycle; `blk` gets a blocks edge.
	mover := prefix + "-1"
	blk := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, owner, mover, "mover", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, blk, "blockee", "open", 2)

	assert := func(label string, open, inP, blocked, def, closed, total int) {
		t.Helper()
		s := statsFor(t, store, pid, owner)
		if s.Open != open || s.InProgress != inP || s.Blocked != blocked ||
			s.Deferred != def || s.Closed != closed || s.Total != total {
			t.Fatalf("%s: grid = open %d / in_progress %d / blocked %d / deferred %d / closed %d / total %d; want %d/%d/%d/%d/%d/%d",
				label, s.Open, s.InProgress, s.Blocked, s.Deferred, s.Closed, s.Total,
				open, inP, blocked, def, closed, total)
		}
	}

	// Snapshot: both open.
	assert("snapshot", 2, 0, 0, 0, 0, 2)

	// Block `blk`: open 2→1, blocked 0→1.
	testsupport.SeedBlocker(t, pool, blk, owner)
	assert("after block blk", 1, 0, 1, 0, 0, 2)

	// mover open → in_progress: open 1→0, in_progress 0→1.
	testsupport.SetStatus(t, pool, mover, "in_progress")
	assert("mover in_progress", 0, 1, 1, 0, 0, 2)

	// mover in_progress → deferred: in_progress 1→0, deferred 0→1.
	testsupport.SetStatus(t, pool, mover, "deferred")
	assert("mover deferred", 0, 0, 1, 1, 0, 2)

	// mover deferred → closed: deferred 1→0, closed 0→1.
	testsupport.SetStatus(t, pool, mover, "closed")
	assert("mover closed", 0, 0, 1, 0, 1, 2)
}

// TestProjectStats_MyAssignedMyCreatedTransitions exercises the
// caller-scoped MyAssigned / MyCreated aggregates under re-assign and
// close transitions. MyAssigned = non-closed assigned to me; MyCreated =
// all issues I created (any status).
func TestProjectStats_MyAssignedMyCreatedTransitions(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	ownerName := testsupport.Username(t, pool, owner)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspmine")
	store := NewStore(pool)

	id := prefix + "-1"
	// Created by owner, assigned to owner, open.
	testsupport.SeedIssue(t, pool, pid, owner, id, "mine", "open", 2)
	testsupport.SetAssignee(t, pool, id, ownerName)

	s := statsFor(t, store, pid, owner)
	if s.MyAssigned != 1 || s.MyCreated != 1 {
		t.Fatalf("snapshot myAssigned/myCreated = %d/%d, want 1/1", s.MyAssigned, s.MyCreated)
	}

	// Re-assign to other: MyAssigned 1→0; MyCreated stays 1 (still my issue).
	testsupport.SetAssignee(t, pool, id, otherName)
	s = statsFor(t, store, pid, owner)
	if s.MyAssigned != 0 || s.MyCreated != 1 {
		t.Fatalf("after re-assign myAssigned/myCreated = %d/%d, want 0/1", s.MyAssigned, s.MyCreated)
	}

	// Re-assign back, then close: MyAssigned counts only non-closed → 0;
	// MyCreated still 1.
	testsupport.SetAssignee(t, pool, id, ownerName)
	if s = statsFor(t, store, pid, owner); s.MyAssigned != 1 {
		t.Fatalf("after re-assign-to-self myAssigned = %d, want 1", s.MyAssigned)
	}
	testsupport.SetStatus(t, pool, id, "closed")
	s = statsFor(t, store, pid, owner)
	if s.MyAssigned != 0 || s.MyCreated != 1 {
		t.Fatalf("after close myAssigned/myCreated = %d/%d, want 0/1 (closed excluded from MyAssigned, kept in MyCreated)",
			s.MyAssigned, s.MyCreated)
	}
}

// TestComponentPills_Transitions mirrors the project-grid transition test
// for the per-component list pills (components.go). One component with
// two linked issues; walk one through the lifecycle and add a blocks edge
// to the other, re-asserting the component row each time.
func TestComponentPills_Transitions(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tspcomp")
	store := NewStore(pool)

	comp, err := store.CreateComponent(context.Background(), pid, owner,
		CreateComponentArgs{Name: "core"}, HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("create component: %v", err)
	}

	mover := prefix + "-1"
	blk := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, owner, mover, "mover", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, blk, "blockee", "open", 2)
	link := func(issue string) {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
			issue, comp.ID); err != nil {
			t.Fatalf("link %s: %v", issue, err)
		}
	}
	link(mover)
	link(blk)

	row := func() ComponentDTO {
		t.Helper()
		list, err := store.ListComponents(storeFixtureCtx(), pid, "", uuid.Nil, "")
		if err != nil {
			t.Fatalf("ListComponents: %v", err)
		}
		for _, c := range list {
			if c.ID == comp.ID {
				return c
			}
		}
		t.Fatalf("component %s not in list", comp.ID)
		return ComponentDTO{}
	}
	assert := func(label string, open, inP, blocked int) {
		t.Helper()
		c := row()
		if c.OpenCount != open || c.InProgressCount != inP || c.BlockedCount != blocked {
			t.Fatalf("%s: component pills = open %d / in_progress %d / blocked %d, want %d/%d/%d",
				label, c.OpenCount, c.InProgressCount, c.BlockedCount, open, inP, blocked)
		}
	}

	// Snapshot: both open.
	assert("snapshot", 2, 0, 0)

	// Block blk: open 2→1, blocked 0→1.
	testsupport.SeedBlocker(t, pool, blk, owner)
	assert("after block", 1, 0, 1)

	// mover open → in_progress.
	testsupport.SetStatus(t, pool, mover, "in_progress")
	assert("mover in_progress", 0, 1, 1)

	// mover → deferred: leaves all three pills.
	testsupport.SetStatus(t, pool, mover, "deferred")
	assert("mover deferred", 0, 0, 1)

	// mover → closed (already not in pills); block target stays.
	testsupport.SetStatus(t, pool, mover, "closed")
	assert("mover closed", 0, 0, 1)
}

// TestInboxAcceptanceGate asserts the idx_issues_unaccepted gate
// (accepted_at IS NULL AND status='open', assigned by someone else): the
// inbox-needs-acceptance count flips when the issue is accepted AND when
// it transitions open→in_progress (the index predicate requires
// status='open'). Project-scoped count via testsupport.CountUnacceptedInbox.
func TestInboxAcceptanceGate(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tspinbox")

	id := prefix + "-1"
	// other → caller, open, unaccepted → in the inbox gate.
	testsupport.SeedIssue(t, pool, pid, other, id, "needs accept", "open", 2)
	testsupport.SetAssignee(t, pool, id, callerName)

	if n := testsupport.CountUnacceptedInbox(t, pool, pid, caller, callerName); n != 1 {
		t.Fatalf("inbox gate snapshot = %d, want 1", n)
	}

	// Accept → leaves the gate.
	testsupport.Accept(t, pool, id, caller, true)
	if n := testsupport.CountUnacceptedInbox(t, pool, pid, caller, callerName); n != 0 {
		t.Fatalf("inbox gate after accept = %d, want 0", n)
	}

	// Un-accept → back in the gate.
	testsupport.Accept(t, pool, id, caller, false)
	if n := testsupport.CountUnacceptedInbox(t, pool, pid, caller, callerName); n != 1 {
		t.Fatalf("inbox gate after un-accept = %d, want 1", n)
	}

	// open → in_progress while still unaccepted → leaves the gate (the
	// index predicate requires status='open').
	testsupport.SetStatus(t, pool, id, "in_progress")
	if n := testsupport.CountUnacceptedInbox(t, pool, pid, caller, callerName); n != 0 {
		t.Fatalf("inbox gate after open→in_progress = %d, want 0 (gate is status='open' only)", n)
	}
}

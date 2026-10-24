package projects

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// TestComponentStats_StandingExcludedAndBucketed asserts a standing issue linked
// to a component is excluded from the active component pills (open / in_progress
// / blocked) and reported in the component's standingCount instead (sk-118.8).
func TestComponentStats_StandingExcludedAndBucketed(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tcstand")
	store := NewStore(pool)

	comp, err := store.CreateComponent(context.Background(), pid, owner,
		CreateComponentArgs{Name: "core"}, HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("create component: %v", err)
	}

	standing := prefix + "-1"
	open := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, owner, standing, "perpetual", "standing", 2)
	testsupport.SeedIssue(t, pool, pid, owner, open, "normal", "open", 2)
	link := func(issue string) {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
			issue, comp.ID); err != nil {
			t.Fatalf("link %s: %v", issue, err)
		}
	}
	link(standing)
	link(open)

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

	c := row()
	if c.StandingCount != 1 {
		t.Fatalf("component standingCount = %d, want 1", c.StandingCount)
	}
	if c.OpenCount != 1 {
		t.Fatalf("component openCount = %d, want 1 (standing must NOT fold into open)", c.OpenCount)
	}
	if c.InProgressCount != 0 || c.BlockedCount != 0 {
		t.Fatalf("component active pills polluted by standing — in_progress=%d blocked=%d",
			c.InProgressCount, c.BlockedCount)
	}
}

// TestComponentStats_MyOutboxStandingScoped is the MAJOR gap fill for the
// component-level outbox-scoped standing pill. components.go computes
// `my_outbox_standing_count` (~line 386) with the component Delegated
// predicate (created_by = caller AND assignee <> caller), but the existing
// component standing test only covered the UNSCOPED standingCount. This seeds
// three standing issues — caller→other (delegated out), caller→self
// (self-assigned), other→other (foreign) — all linked to one component, and
// asserts the outbox pill counts ONLY the delegated-out one while the
// unscoped pill counts all three. Mirrors TestListComponents_MyOutboxCounts
// for the standing status.
func TestComponentStats_MyOutboxStandingScoped(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tcoxst")
	store := NewStore(pool)

	comp, err := store.CreateComponent(context.Background(), pid, caller,
		CreateComponentArgs{Name: "core"}, HumanActor(caller, "caller"))
	if err != nil {
		t.Fatalf("create component: %v", err)
	}

	// caller→other standing: counts in the outbox pill (delegated out).
	deleg := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, caller, deleg, "delegated standing", "standing", 2)
	testsupport.SetAssignee(t, pool, deleg, otherName)
	// caller→self standing: must NOT count (not delegated out).
	self := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, caller, self, "self standing", "standing", 2)
	testsupport.SetAssignee(t, pool, self, callerName)
	// other→other standing: must NOT count (caller did not create it).
	foreign := prefix + "-3"
	testsupport.SeedIssue(t, pool, pid, other, foreign, "foreign standing", "standing", 2)
	testsupport.SetAssignee(t, pool, foreign, otherName)

	link := func(issue string) {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
			issue, comp.ID); err != nil {
			t.Fatalf("link %s: %v", issue, err)
		}
	}
	link(deleg)
	link(self)
	link(foreign)

	// Caller-scoped list (callerID + username drive the my_outbox_* FILTERs).
	list, err := store.ListComponents(storeFixtureCtx(), pid, "", caller, callerName)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	var got *ComponentDTO
	for i := range list {
		if list[i].ID == comp.ID {
			got = &list[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("component %s not in list", comp.ID)
	}

	// Unscoped: all three standing rows.
	if got.StandingCount != 3 {
		t.Fatalf("standingCount = %d, want 3 (all three standing rows on the component)", got.StandingCount)
	}
	// Outbox-scoped: only the caller→other delegated-out standing.
	if got.MyOutboxStandingCount != 1 {
		t.Fatalf("myOutboxStandingCount = %d, want 1 (only caller-created + delegated-out standing)", got.MyOutboxStandingCount)
	}
	// Standing must not leak into the active outbox pills.
	if got.MyOutboxOpenCount != 0 || got.MyOutboxInProgressCount != 0 || got.MyOutboxBlockedCount != 0 {
		t.Fatalf("standing leaked into active outbox pills — open=%d in_progress=%d blocked=%d",
			got.MyOutboxOpenCount, got.MyOutboxInProgressCount, got.MyOutboxBlockedCount)
	}
}

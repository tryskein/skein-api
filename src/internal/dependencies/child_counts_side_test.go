package dependencies

// child_counts_side_test.go — sk-282 T013, the «ready to take» carrier's
// binding to the shared counting fragment (test-plan row 52).
//
// The cross-carrier agreement is asserted in internal/issues'
// child_counts_class_test.go, which is the only place that can import all
// three carriers. What belongs HERE is the property that is local to this
// package: this carrier does not own a query, and its rows all get an
// entry — including the ones with no children.

import (
	"context"
	"testing"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
	"skein-api/internal/testsupport"
)

func TestChildCountsForReady_CoversEveryRowIncludingChildless(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "ccready")

	withChild := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "has a child", "open", 2)
	child := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1.1", "child", "open", 2)
	testsupport.SeedDep(t, pool, child, withChild, DepParentChild, owner)
	childless := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-2", "no children", "open", 2)

	store := NewStore(pool)
	rows, err := store.ListReady(ctx, pid, ReadyFilters{}, 50)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	counts, err := store.ChildCountsForReady(ctx, rows, authz.ForUser(owner))
	if err != nil {
		t.Fatalf("ChildCountsForReady: %v", err)
	}

	for _, r := range rows {
		if _, ok := counts[r.ID]; !ok {
			t.Errorf("no counts entry for ready row %s — a missing key reads as «not counted», "+
				"and US-3 requires a zero", r.ID)
		}
	}
	if got := counts[withChild]; got != (childcount.Counts{Total: 1, Unclosed: 1}) {
		t.Errorf("counts[%s] = %+v, want 1/1", withChild, got)
	}
	if got := counts[childless]; got != (childcount.Counts{}) {
		t.Errorf("counts[%s] = %+v, want zeros — «counted, and there are none»", childless, got)
	}

	// The carrier's own row type must not have grown the counters: the
	// ready REST endpoint serialises it.
	encoded := marshalReadyRows(t, rows)
	for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
		if containsJSONKey(encoded, key) {
			t.Errorf("dependencies.IssueSummary serialises %q — the counters are a side value "+
				"precisely so the REST ready endpoint cannot receive them", key)
		}
	}
}

package issues

// child_counts_test.go — sk-282 T012, test-plan rows 45 (store side) and
// 47.
//
//	row 45  US-3: a TASK with children in open / in_progress / deferred /
//	            closed plus one standing → total 4, unclosed 3
//	row 47  R-7: on an epic the previous rollup is present IN ADDITION to
//	            the new counters, with its previous values
//
// Row 47 is the one that keeps the change honest. The two aggregates
// legitimately disagree — the rollup counts «open» children, the new
// counter counts every non-closed one — and the web UI reads the rollup.
// Asserting the rollup's values unchanged is what proves the new counters
// were added BESIDE it rather than on top of it.

import (
	"testing"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

// row 45 — the store's own answer, on a task.
func TestCountChildren_ExcludesStandingAndCountsUnclosedWidely(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := storeFixtureCtx()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "cc45")
	// A TASK, not an epic: that is the case the old rollup cannot answer
	// at all, and the reason US-3 exists.
	parent := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "wave head", "open", 2)
	for i, st := range []string{"open", "in_progress", "deferred", "closed", "standing"} {
		child := testsupport.SeedIssue(t, pool, pid, owner,
			prefix+"-1."+itoaChild(i+1), "child "+st, st, 2)
		testsupport.SeedDep(t, pool, child, parent, "parent-child", owner)
	}

	store := NewStore(pool)
	got, err := store.CountChildren(ctx, parent, authz.Unrestricted())
	if err != nil {
		t.Fatalf("CountChildren: %v", err)
	}
	if got.Total != 4 {
		t.Errorf("Total = %d, want 4 — the standing child is a perpetual obligation, not a "+
			"unit of work, and has no place in the denominator of «done / total»", got.Total)
	}
	if got.Unclosed != 3 {
		t.Errorf("Unclosed = %d, want 3 — open, in_progress and deferred all hold the wave; "+
			"only closed and standing drop out", got.Unclosed)
	}
}

// row 45, visibility half — an invisible child is not counted as work.
func TestCountChildren_InvisibleChildIsNotWork(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := storeFixtureCtx()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	viewer, _ := testsupport.SeedUser(t, pool, "user")
	visProj, visPrefix := testsupport.SeedProject(t, pool, owner, "cc45v")
	hidProj, hidPrefix := testsupport.SeedProject(t, pool, owner, "cc45h")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	parent := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1", "head", "open", 2)
	seen := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1.1", "seen", "open", 2)
	hidden := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "hidden", "open", 2)
	testsupport.SeedDep(t, pool, seen, parent, "parent-child", owner)
	testsupport.SeedDep(t, pool, hidden, parent, "parent-child", owner)

	store := NewStore(pool)
	got, err := store.CountChildren(ctx, parent, authz.ForUser(viewer))
	if err != nil {
		t.Fatalf("CountChildren: %v", err)
	}
	if got.Total != 1 || got.Unclosed != 1 {
		t.Errorf("counts = %+v, want 1/1 — the invisible child is not counted here; it is "+
			"counted by the dependency counters, in the unavailable bucket", got)
	}
	// Control: to a caller who sees both projects it IS work.
	if all, err := store.CountChildren(ctx, parent, authz.Unrestricted()); err != nil {
		t.Fatalf("CountChildren(admin): %v", err)
	} else if all.Total != 2 {
		t.Errorf("admin Total = %d, want 2 — the row was excluded, not lost", all.Total)
	}
}

// row 47 — the epic rollup keeps its own values beside the new counters.
func TestCountChildren_EpicRollupUnchangedBeside(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := storeFixtureCtx()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "cc47")
	epicID := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "the epic", "open", 2)
	if _, err := pool.Exec(ctx, `UPDATE issues SET type = 'epic' WHERE id = $1`, epicID); err != nil {
		t.Fatalf("make epic: %v", err)
	}
	for i, st := range []string{"open", "in_progress", "deferred", "closed", "standing"} {
		child := testsupport.SeedIssue(t, pool, pid, owner,
			prefix+"-1."+itoaChild(i+1), "child "+st, st, 2)
		testsupport.SeedDep(t, pool, child, epicID, "parent-child", owner)
	}

	store := NewStore(pool)
	detail, err := store.GetIssue(ctx, epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if detail.Epic == nil {
		t.Fatal("the epic rollup is gone — it is what the web UI reads, and US-3 says it stays " +
			"as-is, values and semantics both")
	}
	// Its own, NARROWER meaning: strictly open children.
	if detail.Epic.OpenChildren != 1 {
		t.Errorf("rollup OpenChildren = %d, want 1 — the rollup counts strictly «open»",
			detail.Epic.OpenChildren)
	}
	if detail.Epic.InProgressChildren != 1 || detail.Epic.ClosedChildren != 1 {
		t.Errorf("rollup in_progress/closed = %d/%d, want 1/1",
			detail.Epic.InProgressChildren, detail.Epic.ClosedChildren)
	}
	if detail.Epic.StandingChildren != 1 {
		t.Errorf("rollup StandingChildren = %d, want 1", detail.Epic.StandingChildren)
	}

	counts, err := store.CountChildren(ctx, epicID, authz.Unrestricted())
	if err != nil {
		t.Fatalf("CountChildren: %v", err)
	}
	if counts.Unclosed != 3 {
		t.Errorf("new Unclosed = %d, want 3", counts.Unclosed)
	}
	// The disagreement is the point, and INV-10 requires it to be a
	// KNOWN one: the rollup's open-children is narrower than the new
	// unclosed count by exactly the in_progress and deferred children.
	if detail.Epic.OpenChildren >= counts.Unclosed {
		t.Errorf("the epic rollup's OpenChildren (%d) is not narrower than unclosedChildrenCount "+
			"(%d) — if they ever agree on this fixture, one of them stopped meaning what it "+
			"is documented to mean, and the two-field coexistence loses its justification",
			detail.Epic.OpenChildren, counts.Unclosed)
	}
}

// itoaChild renders a small positive child index. Local to keep this file
// independent of the other test files' helpers.
func itoaChild(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

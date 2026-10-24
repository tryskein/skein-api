package issues_test

// child_counts_class_test.go — sk-282 T013, test-plan rows 52 and 53.
//
//	row 52  D-2 / FP-11: the counters arrive as a SIDE VALUE from all three
//	            carriers (ListIssues, ListAllIssues, ListReady), counted by
//	            ONE fragment; the REST path does not switch it on
//	row 53  INV-10 on each of the three SQL paths: the identity holds,
//	            invisible and standing children included
//
// This is an EXTERNAL test package on purpose. The class spans
// internal/issues and internal/dependencies, and only an external package
// can import both plus internal/childcount without an import cycle — which
// is also the only way to state the class property that matters: the three
// carriers agree WITH EACH OTHER. A per-package test can prove each
// carrier self-consistent and still let the three drift apart.

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// classFixture is one parent whose children exercise every rule at once:
// the four countable statuses, a standing child (excluded from the
// counters, present in the dependency counters' standing bucket) and an
// invisible child (excluded from the counters, present in the unavailable
// bucket).
type classFixture struct {
	owner      uuid.UUID
	viewer     uuid.UUID
	visProject uuid.UUID
	parent     string
}

func seedClassFixture(t *testing.T, pool *pgxpool.Pool, hint string) classFixture {
	t.Helper()
	owner, _ := testsupport.SeedUser(t, pool, "user")
	viewer, viewerName := testsupport.SeedUser(t, pool, "user")
	visProject, visPrefix := testsupport.SeedProject(t, pool, owner, hint)
	hidProject, hidPrefix := testsupport.SeedProject(t, pool, owner, hint+"h")
	testsupport.AddProjectMember(t, pool, visProject, viewer, "member")

	// The parent is OPEN and assigned to the viewer, so it shows up in
	// every one of the three carriers' default result sets: the project
	// list, the cross-project list, and «ready to take».
	parent := testsupport.SeedIssue(t, pool, visProject, owner, visPrefix+"-1", "wave head", "open", 2)
	testsupport.SetAssignee(t, pool, parent, viewerName)

	for i, st := range []string{"open", "in_progress", "deferred", "closed", "standing"} {
		child := testsupport.SeedIssue(t, pool, visProject, owner,
			fmt.Sprintf("%s-1.%d", visPrefix, i+1), "child "+st, st, 2)
		testsupport.SeedDep(t, pool, child, parent, dependencies.DepParentChild, owner)
	}
	hidden := testsupport.SeedIssue(t, pool, hidProject, owner, hidPrefix+"-1", "unreachable", "open", 2)
	testsupport.SeedDep(t, pool, hidden, parent, dependencies.DepParentChild, owner)

	return classFixture{owner: owner, viewer: viewer, visProject: visProject, parent: parent}
}

// wantCounts is what every carrier must report for the fixture's parent:
// five visible children of which one is standing → four countable, of
// which one is closed → three unclosed. The invisible child is not work
// for this caller.
var wantCounts = childcount.Counts{Total: 4, Unclosed: 3}

// row 52 — the side value, from all three carriers, counted the same way.
func TestChildCountsClass_AllThreeCarriersAgree(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	fx := seedClassFixture(t, pool, "ccc52")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)

	got := map[string]childcount.Counts{}

	t.Run("ListIssues (project mode)", func(t *testing.T) {
		res, err := issuesStore.ListIssues(ctx, issues.ListIssuesArgs{ProjectID: fx.visProject})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		counts, err := issuesStore.ChildCountsForRows(ctx, res.Items, authz.ForUser(fx.viewer))
		if err != nil {
			t.Fatalf("ChildCountsForRows: %v", err)
		}
		assertCarrier(t, "ListIssues", res.Items, counts, fx.parent)
		got["ListIssues"] = counts[fx.parent]
	})

	t.Run("ListAllIssues (cross-project mode)", func(t *testing.T) {
		res, err := issuesStore.ListAllIssues(ctx, issues.ListAllIssuesArgs{
			Scope:  authz.ForUser(fx.viewer),
			Caller: fx.viewer,
			Limit:  100,
		})
		if err != nil {
			t.Fatalf("ListAllIssues: %v", err)
		}
		counts, err := issuesStore.ChildCountsForRows(ctx, res.Items, authz.ForUser(fx.viewer))
		if err != nil {
			t.Fatalf("ChildCountsForRows: %v", err)
		}
		assertCarrier(t, "ListAllIssues", res.Items, counts, fx.parent)
		got["ListAllIssues"] = counts[fx.parent]
	})

	t.Run("ListReady (prime's ready-to-take)", func(t *testing.T) {
		rows, err := depsStore.ListReady(ctx, fx.visProject, dependencies.ReadyFilters{}, 50)
		if err != nil {
			t.Fatalf("ListReady: %v", err)
		}
		counts, err := depsStore.ChildCountsForReady(ctx, rows, authz.ForUser(fx.viewer))
		if err != nil {
			t.Fatalf("ChildCountsForReady: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.ID == fx.parent {
				found = true
			}
		}
		if !found {
			t.Fatalf("the parent is not in the ready set — the fixture does not exercise this "+
				"carrier at all (rows: %d)", len(rows))
		}
		if counts[fx.parent] != wantCounts {
			t.Errorf("ListReady counts = %+v, want %+v", counts[fx.parent], wantCounts)
		}
		got["ListReady"] = counts[fx.parent]
	})

	// The class property: not «each is right» but «the three agree». A
	// per-carrier assertion cannot catch two of them drifting together.
	if len(got) != 3 {
		t.Fatalf("only %d carriers were exercised, want 3", len(got))
	}
	for name, counts := range got {
		if counts != wantCounts {
			t.Errorf("carrier %s reports %+v, the others report %+v — «how many children» now "+
				"has more than one answer, and INV-10's identity stops holding BETWEEN "+
				"surfaces while every per-surface test stays green", name, counts, wantCounts)
		}
	}
}

// assertCarrier checks that a list carrier's side value covers every row
// it returned — including rows with no children, which must read as 0 and
// not as an absent key.
func assertCarrier(t *testing.T, name string, rows []issues.IssueSummary, counts map[string]childcount.Counts, parent string) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("%s returned no rows — the fixture does not exercise this carrier", name)
	}
	for _, r := range rows {
		if _, ok := counts[r.ID]; !ok {
			t.Errorf("%s: no counts entry for row %s — a missing key reads as «not counted», "+
				"and US-3 requires 0", name, r.ID)
		}
	}
	if counts[parent] != wantCounts {
		t.Errorf("%s counts for the parent = %+v, want %+v", name, counts[parent], wantCounts)
	}
}

// row 52, second half — the counters are a SIDE value: nothing was added
// to the shared row or result types, which is what keeps them off REST.
func TestChildCountsClass_NotOnTheSharedRowTypes(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	fx := seedClassFixture(t, pool, "ccc52b")

	res, err := issues.NewStore(pool).ListIssues(ctx, issues.ListIssuesArgs{ProjectID: fx.visProject})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	encoded := marshalJSON(t, res)
	for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
		if containsKey(encoded, key) {
			t.Errorf("the shared list result serialises %q — D-2 and INV-2 keep the counters "+
				"OFF IssueSummary and ListResult, because those are exactly what the REST "+
				"list endpoints return, and INV-2 forbids even an additive key there.\n%s",
				key, encoded)
		}
	}
}

// row 53 — INV-10's identity, checked on each carrier's own numbers
// against the dependency counters computed for the same caller.
func TestChildCountsClass_INV10IdentityOnEveryCarrier(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	fx := seedClassFixture(t, pool, "ccc53")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)

	depCounts, err := depsStore.CountDependencies(ctx, fx.parent, authz.ForUser(fx.viewer))
	if err != nil {
		t.Fatalf("CountDependencies: %v", err)
	}
	cell := depCounts.Down[dependencies.DepParentChild]
	// Non-vacuity: with either subtraction at zero the identity would hold
	// trivially and this row would prove nothing.
	if cell.Standing == 0 || cell.Unavailable == 0 {
		t.Fatalf("fixture has standing=%d unavailable=%d — both must be non-zero",
			cell.Standing, cell.Unavailable)
	}
	wantTotal := cell.Total() - cell.Standing - cell.Unavailable

	carriers := map[string]childcount.Counts{}

	res, err := issuesStore.ListIssues(ctx, issues.ListIssuesArgs{ProjectID: fx.visProject})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	c, err := issuesStore.ChildCountsForRows(ctx, res.Items, authz.ForUser(fx.viewer))
	if err != nil {
		t.Fatalf("ChildCountsForRows: %v", err)
	}
	carriers["ListIssues"] = c[fx.parent]

	all, err := issuesStore.ListAllIssues(ctx, issues.ListAllIssuesArgs{
		Scope:  authz.ForUser(fx.viewer),
		Caller: fx.viewer,
	})
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
	c, err = issuesStore.ChildCountsForRows(ctx, all.Items, authz.ForUser(fx.viewer))
	if err != nil {
		t.Fatalf("ChildCountsForRows(all): %v", err)
	}
	carriers["ListAllIssues"] = c[fx.parent]

	ready, err := depsStore.ListReady(ctx, fx.visProject, dependencies.ReadyFilters{}, 50)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	rc, err := depsStore.ChildCountsForReady(ctx, ready, authz.ForUser(fx.viewer))
	if err != nil {
		t.Fatalf("ChildCountsForReady: %v", err)
	}
	carriers["ListReady"] = rc[fx.parent]

	for name, counts := range carriers {
		if counts.Total != wantTotal {
			t.Errorf("INV-10 broken on %s: childrenCount = %d, but down/parent-child "+
				"total(%d) − standing(%d) − unavailable(%d) = %d",
				name, counts.Total, cell.Total(), cell.Standing, cell.Unavailable, wantTotal)
		}
	}
}

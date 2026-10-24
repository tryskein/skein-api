package dependencies

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect %s: %v", testDatabaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", testDatabaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "tu" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "deptest_" + suffix
	prefix := "dpt" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner)
	if err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed project counter: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, prefix
}

func seedComponent(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', '[]'::jsonb, NULL, $4, now(), now())`,
		id, projectID, name, creator)
	if err != nil {
		t.Fatalf("seed component: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM components WHERE id = $1`, id)
	})
	return id
}

// seedOpenIssue inserts an open, unblocked issue (status=open, no
// outgoing blocks/waits-for edge) so it qualifies for the Ready set.
func seedOpenIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

func pinComponent(t *testing.T, pool *pgxpool.Pool, issueID string, componentID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
		issueID, componentID)
	if err != nil {
		t.Fatalf("pin component: %v", err)
	}
}

// TestListReady_ComponentFilter_HappyPath verifies the new ComponentID
// filter returns only ready issues pinned to the requested component.
func TestListReady_ComponentFilter_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	compID := seedComponent(t, pool, pid, owner, "alpha")

	// 4 unrelated ready issues that do not have the component pinned.
	for i := 1; i <= 4; i++ {
		seedOpenIssue(t, pool, pid, owner, prefix+"-"+itoa(i), "noise "+itoa(i))
	}
	// 2 ready issues pinned to the component.
	for i := 5; i <= 6; i++ {
		seedOpenIssue(t, pool, pid, owner, prefix+"-"+itoa(i), "target "+itoa(i))
		pinComponent(t, pool, prefix+"-"+itoa(i), compID)
	}

	store := NewStore(pool)
	res, err := store.ListReady(context.Background(), pid, ReadyFilters{
		ComponentID: &compID,
	}, 10)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 component-pinned ready issues, got %d (ids: %v)", len(res), idsOf(res))
	}
	got := map[string]bool{}
	for _, it := range res {
		got[it.ID] = true
	}
	for i := 5; i <= 6; i++ {
		want := prefix + "-" + itoa(i)
		if !got[want] {
			t.Errorf("missing pinned ready issue %s in result %v", want, idsOf(res))
		}
	}
}

// TestListReady_ComponentFilter_ZeroResults verifies that a component
// with no ready issues attached returns an empty slice.
func TestListReady_ComponentFilter_ZeroResults(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	compID := seedComponent(t, pool, pid, owner, "lonely")

	for i := 1; i <= 4; i++ {
		seedOpenIssue(t, pool, pid, owner, prefix+"-"+itoa(i), "no-component "+itoa(i))
	}

	store := NewStore(pool)
	res, err := store.ListReady(context.Background(), pid, ReadyFilters{
		ComponentID: &compID,
	}, 10)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected 0 results, got %d", len(res))
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func idsOf(items []IssueSummary) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// ---------------------------------------------------------------------
// sk-282 T008 — dependency counters (test-plan rows 15–19).
//
//	row 15  INV-9: the direction × type × status cut; a node that has BOTH
//	            a parent and children keeps «up» separate from «down»
//	row 16  visibility: an unreachable link is its own non-zero category
//	            INSIDE its direction × type cell, and the invisible side's
//	            status is disclosed by no bucket
//	row 17  US-1: an invisible parent AND an invisible child at the same
//	            time land in two separate cells, not one bucket per type
//	row 18  edge: an issue with no dependencies at all reads as zero, not
//	            as «not counted»
//	row 19  the T008 gate: the sum over cells equals the number of edges
//	            the SAME caller gets from an explicit request
//
// Row 50 (an invisible standing child lands only in «unavailable») is
// owned by T012's test task and is not here.
//
// Everything seeds its own data through internal/testsupport, so a
// narrowed run works and nothing depends on another package's rows.
// ---------------------------------------------------------------------

// countsFixture is the node INV-9 is written about: one parent above,
// several children below, and a second edge type so the type axis is not
// degenerate.
type countsFixture struct {
	owner   uuid.UUID
	project uuid.UUID
	node    string
	parent  string
	// children are in the visible project: 2 closed, 1 open, 1 in_progress.
	children []string
	blocker  string
}

func seedCountsFixture(t *testing.T, pool *pgxpool.Pool) countsFixture {
	t.Helper()
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "depcnt")

	fx := countsFixture{owner: owner, project: pid}
	fx.node = testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "wave node", "open", 2)
	fx.parent = testsupport.SeedIssue(t, pool, pid, owner, prefix+"-2", "container", "standing", 2)
	// parent-child is stored from = child, to = parent, so the node's
	// edge TO its parent is outgoing → direction «up».
	testsupport.SeedDep(t, pool, fx.node, fx.parent, DepParentChild, owner)

	statuses := []string{"closed", "closed", "open", "in_progress"}
	for i, st := range statuses {
		childID := testsupport.SeedIssue(t, pool, pid, owner,
			prefix+"-1."+itoa(i+1), "child", st, 2)
		testsupport.SeedDep(t, pool, childID, fx.node, DepParentChild, owner)
		fx.children = append(fx.children, childID)
	}

	// A second type, pointing the other way: the node is blocked by an
	// open issue, so `blocks` appears under «up» only.
	fx.blocker = testsupport.SeedIssue(t, pool, pid, owner, prefix+"-3", "blocker", "open", 2)
	testsupport.SeedDep(t, pool, fx.node, fx.blocker, DepBlocks, owner)

	return fx
}

// row 15 — the cut itself, and the mixing INV-9 forbids.
func TestCountDependencies_DirectionTypeStatusCut(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	fx := seedCountsFixture(t, pool)
	store := NewStore(pool)

	counts, err := store.CountDependencies(context.Background(), fx.node, authz.ForUser(fx.owner))
	if err != nil {
		t.Fatalf("CountDependencies: %v", err)
	}

	// Down = children. The status axis must be resolved, not summed.
	down, ok := counts.Down[DepParentChild]
	if !ok {
		t.Fatalf("no down/parent-child cell; got down=%+v", counts.Down)
	}
	want := DepStatusCounts{Open: 1, InProgress: 1, Closed: 2}
	if down != want {
		t.Errorf("down/parent-child = %+v, want %+v", down, want)
	}

	// Up = the parent. One standing edge, and — this is the INV-9 point —
	// it must NOT have been folded in with the four children.
	up, ok := counts.Up[DepParentChild]
	if !ok {
		t.Fatalf("no up/parent-child cell; got up=%+v", counts.Up)
	}
	if up != (DepStatusCounts{Standing: 1}) {
		t.Errorf("up/parent-child = %+v, want exactly one standing edge", up)
	}
	if got := up.Total() + down.Total(); got != 5 {
		t.Fatalf("parent-child edges total = %d, want 5", got)
	}
	if up.Total() == 5 || down.Total() == 5 {
		t.Errorf("one direction absorbed the whole type — \"57 children and one parent\" " +
			"folded into 58 is the exact defect INV-9 names")
	}

	// The type axis: `blocks` exists upward only, and a type the issue has
	// no edges of is absent ENTIRELY rather than present with zeros.
	if blocks, ok := counts.Up[DepBlocks]; !ok || blocks != (DepStatusCounts{Open: 1}) {
		t.Errorf("up/blocks = %+v (present=%v), want exactly one open edge", blocks, ok)
	}
	if _, ok := counts.Down[DepBlocks]; ok {
		t.Errorf("down carries a blocks cell although the node has no incoming blocks edge — "+
			"\"types the issue has no edges of are absent from the response entirely\"; down=%+v",
			counts.Down)
	}
	for _, unusedType := range []string{DepRelated, DepDuplicates, DepSupersedes, DepWaitsFor, DepDiscoveredFrom} {
		if _, ok := counts.Up[unusedType]; ok {
			t.Errorf("up carries a %q cell the issue has no edges of", unusedType)
		}
		if _, ok := counts.Down[unusedType]; ok {
			t.Errorf("down carries a %q cell the issue has no edges of", unusedType)
		}
	}
}

// row 16 — the unavailable category, and the status it must not leak.
func TestCountDependencies_UnavailableIsItsOwnBucketAndHidesStatus(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	viewer, _ := testsupport.SeedUser(t, pool, "user")
	visProj, visPrefix := testsupport.SeedProject(t, pool, owner, "depvis")
	hidProj, hidPrefix := testsupport.SeedProject(t, pool, owner, "dephid")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	node := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1", "node", "open", 2)
	// One visible closed child …
	visChild := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1.1", "seen", "closed", 2)
	testsupport.SeedDep(t, pool, visChild, node, DepParentChild, owner)
	// … and two invisible children whose statuses are DIFFERENT from the
	// visible one, so a leak would be recognisable rather than masked by
	// coincidence.
	hidOpen := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "hidden open", "open", 2)
	hidDeferred := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-2", "hidden deferred", "deferred", 2)
	testsupport.SeedDep(t, pool, hidOpen, node, DepParentChild, owner)
	testsupport.SeedDep(t, pool, hidDeferred, node, DepParentChild, owner)

	store := NewStore(pool)
	counts, err := store.CountDependencies(ctx, node, authz.ForUser(viewer))
	if err != nil {
		t.Fatalf("CountDependencies: %v", err)
	}

	down := counts.Down[DepParentChild]
	if down.Unavailable != 2 {
		t.Errorf("down/parent-child unavailable = %d, want 2 — an edge into an invisible "+
			"project must be COUNTED, not dropped: \"count only the visible\" is the "+
			"rejected variant that makes a cross-project tail vanish", down.Unavailable)
	}
	if down.Closed != 1 {
		t.Errorf("down/parent-child closed = %d, want 1 (the visible child)", down.Closed)
	}
	// The negative vector: neither invisible child's status may appear.
	if down.Open != 0 {
		t.Errorf("down/parent-child open = %d, want 0 — the invisible child's «open» leaked "+
			"into a status bucket", down.Open)
	}
	if down.Deferred != 0 {
		t.Errorf("down/parent-child deferred = %d, want 0 — the invisible child's «deferred» "+
			"leaked into a status bucket", down.Deferred)
	}
	if down.InProgress != 0 || down.Standing != 0 {
		t.Errorf("down/parent-child = %+v: a status bucket other than the visible child's is "+
			"non-zero, so something about the invisible side was disclosed", down)
	}

	// Control: the owner sees both projects, so the same edges resolve to
	// real statuses and nothing is unavailable. Without this the test
	// would also pass against an implementation that lost the rows.
	ownerCounts, err := store.CountDependencies(ctx, node, authz.ForUser(owner))
	if err != nil {
		t.Fatalf("CountDependencies owner: %v", err)
	}
	ownerDown := ownerCounts.Down[DepParentChild]
	wantOwner := DepStatusCounts{Open: 1, Deferred: 1, Closed: 1}
	if ownerDown != wantOwner {
		t.Errorf("owner's down/parent-child = %+v, want %+v — the same three edges, now all "+
			"visible", ownerDown, wantOwner)
	}
}

// row 17 — an invisible parent and an invisible child at the same time.
//
// This is the case that separates a correct implementation from one with a
// single «unavailable» bucket per type: that one passes every other row
// here and still leaves the orchestrator unable to tell «my container is
// out of reach» from «one of my children is».
func TestCountDependencies_InvisibleParentAndChildStaySeparate(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	viewer, _ := testsupport.SeedUser(t, pool, "user")
	visProj, visPrefix := testsupport.SeedProject(t, pool, owner, "depsep")
	hidProj, hidPrefix := testsupport.SeedProject(t, pool, owner, "depsephid")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	node := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1", "node", "open", 2)
	hiddenParent := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "hidden parent", "open", 2)
	hiddenChild := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-2", "hidden child", "open", 2)
	// node → hiddenParent is outgoing (up); hiddenChild → node is incoming (down).
	testsupport.SeedDep(t, pool, node, hiddenParent, DepParentChild, owner)
	testsupport.SeedDep(t, pool, hiddenChild, node, DepParentChild, owner)

	store := NewStore(pool)
	counts, err := store.CountDependencies(ctx, node, authz.ForUser(viewer))
	if err != nil {
		t.Fatalf("CountDependencies: %v", err)
	}

	up := counts.Up[DepParentChild]
	down := counts.Down[DepParentChild]
	if up.Unavailable != 1 {
		t.Errorf("up/parent-child unavailable = %d, want 1 (the invisible parent)", up.Unavailable)
	}
	if down.Unavailable != 1 {
		t.Errorf("down/parent-child unavailable = %d, want 1 (the invisible child)", down.Unavailable)
	}
	if up.Unavailable+down.Unavailable == 2 && (up.Unavailable == 0 || down.Unavailable == 0) {
		t.Errorf("both unreachable edges landed in ONE cell (up=%d, down=%d) — \"unavailable: 2\" "+
			"without a direction cannot tell an invisible parent from two invisible children",
			up.Unavailable, down.Unavailable)
	}
	// And nothing else appeared: no status bucket may be non-zero when
	// every edge is unreachable.
	if up != (DepStatusCounts{Unavailable: 1}) {
		t.Errorf("up/parent-child = %+v, want only the unavailable bucket", up)
	}
	if down != (DepStatusCounts{Unavailable: 1}) {
		t.Errorf("down/parent-child = %+v, want only the unavailable bucket", down)
	}
}

// row 18 — no dependencies at all.
func TestCountDependencies_NoDependenciesReadsAsZero(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "depnone")
	lonely := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "no links", "open", 2)

	store := NewStore(pool)
	counts, err := store.CountDependencies(ctx, lonely, authz.ForUser(owner))
	if err != nil {
		t.Fatalf("CountDependencies: %v", err)
	}
	if counts == nil {
		t.Fatal("counts are nil — \"no dependencies\" must be a value, not an absence")
	}
	// Both direction keys must EXIST. A missing key is indistinguishable
	// from «this direction was not computed», which is exactly the
	// confusion INV-1 forbids.
	if counts.Up == nil {
		t.Error("Up is nil — «no parent» must read as an empty set, not as «not counted»")
	}
	if counts.Down == nil {
		t.Error("Down is nil — «no children» must read as an empty set, not as «not counted»")
	}
	if len(counts.Up) != 0 || len(counts.Down) != 0 {
		t.Errorf("an issue with no edges produced cells: up=%+v down=%+v", counts.Up, counts.Down)
	}
}

// row 19 — the T008 gate.
//
// The comparison is against List, called by the SAME caller with the SAME
// visibility pair: the point is not that the numbers are plausible but
// that the two paths cannot disagree about what this caller may see. Run
// on a fixture that has both directions, two types, and — through the
// second sub-case — unreachable edges, because a sum that only matches on
// fully-visible data proves nothing about the unavailable bucket.
func TestCountDependencies_SumMatchesExplicitEdgeRequest(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	assertSumMatchesList := func(t *testing.T, caller uuid.UUID, issueID string) {
		t.Helper()
		counts, err := store.CountDependencies(ctx, issueID, authz.ForUser(caller))
		if err != nil {
			t.Fatalf("CountDependencies: %v", err)
		}
		list, err := store.List(ctx, issueID, authz.ForUser(caller))
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		// Per direction AND per type, not just in total: a bug that moved
		// an edge from one cell to another would keep a global sum right.
		wantUp := map[string]int{}
		for _, e := range list.Outgoing {
			wantUp[e.Type]++
		}
		wantDown := map[string]int{}
		for _, e := range list.Incoming {
			wantDown[e.Type]++
		}
		for depType, want := range wantUp {
			if got := counts.Up[depType].Total(); got != want {
				t.Errorf("up/%s: counters say %d, an explicit edge request returns %d",
					depType, got, want)
			}
		}
		for depType, want := range wantDown {
			if got := counts.Down[depType].Total(); got != want {
				t.Errorf("down/%s: counters say %d, an explicit edge request returns %d",
					depType, got, want)
			}
		}
		// …and no cell exists that the edge request does not back.
		for depType, cell := range counts.Up {
			if wantUp[depType] != cell.Total() {
				t.Errorf("up/%s: counters report %d edges the explicit request does not return",
					depType, cell.Total())
			}
		}
		for depType, cell := range counts.Down {
			if wantDown[depType] != cell.Total() {
				t.Errorf("down/%s: counters report %d edges the explicit request does not return",
					depType, cell.Total())
			}
		}
		if len(list.Outgoing)+len(list.Incoming) == 0 {
			t.Fatal("the fixture has no edges at all — the gate would hold vacuously")
		}
	}

	t.Run("everything visible", func(t *testing.T) {
		fx := seedCountsFixture(t, pool)
		assertSumMatchesList(t, fx.owner, fx.node)
	})

	t.Run("with unreachable edges on both sides", func(t *testing.T) {
		owner, _ := testsupport.SeedUser(t, pool, "user")
		viewer, _ := testsupport.SeedUser(t, pool, "user")
		visProj, visPrefix := testsupport.SeedProject(t, pool, owner, "depgate")
		hidProj, hidPrefix := testsupport.SeedProject(t, pool, owner, "depgatehid")
		testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

		node := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1", "node", "open", 2)
		seen := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1.1", "seen", "open", 2)
		hiddenParent := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "hp", "open", 2)
		hiddenChild := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-2", "hc", "closed", 2)
		testsupport.SeedDep(t, pool, seen, node, DepParentChild, owner)
		testsupport.SeedDep(t, pool, node, hiddenParent, DepParentChild, owner)
		testsupport.SeedDep(t, pool, hiddenChild, node, DepParentChild, owner)

		assertSumMatchesList(t, viewer, node)
	})
}

// TestDepCountStatuses_MatchIssueStatusCheck binds the status buckets to
// the constraint the database actually enforces.
//
// CountDependencies refuses to count a status it has no bucket for, and
// that refusal is only safe while the bucket set IS the schema's set: a
// migration adding a sixth status would otherwise turn every skein_show on
// a node with such a child into an error. Reading the CHECK back is the
// only way to state that as a check rather than as a comment — and it is
// also what makes the hard failure the right call: the drift is caught
// here, in a test, before it can reach a read.
func TestDepCountStatuses_MatchIssueStatusCheck(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	var clause string
	if err := pool.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(oid)
  FROM pg_constraint
 WHERE conrelid = 'issues'::regclass AND conname = 'issues_status_check'`).Scan(&clause); err != nil {
		t.Fatalf("read issues_status_check: %v", err)
	}

	for _, status := range DepCountStatuses() {
		if !strings.Contains(clause, "'"+status+"'") {
			t.Errorf("DepCountStatuses names %q, which the database's issues_status_check does "+
				"not allow: %s", status, clause)
		}
	}
	// The other direction is the one that matters: a status the schema
	// allows and the counters cannot bucket makes CountDependencies fail
	// on any issue that has such a neighbour.
	for _, candidate := range []string{"open", "in_progress", "deferred", "closed", "standing", "blocked", "archived", "paused"} {
		allowedByDB := strings.Contains(clause, "'"+candidate+"'")
		_, hasBucket := depCountStatusBuckets[candidate]
		if allowedByDB && !hasBucket {
			t.Errorf("the database allows status %q but the counters have no bucket for it — "+
				"CountDependencies would fail on every issue with such a neighbour. Add the "+
				"bucket (and decide whether it counts as work) in the same change as the "+
				"migration. Constraint: %s", candidate, clause)
		}
	}
}

package migrations

// migration_000050_progress_test.go — sk-489, matrix row 22 (SC-002).
//
// The deploy promise: "on a data snapshot from before the rollout, every ring, branch
// progress and chip after the migration give the same percentages and the same "X/Y" numbers".
// A user who opens the same epic in the morning must see the same figures —
// otherwise the feature has not "added a leg" but rewritten the past.
//
// WHY THE SNAPSHOT IS TAKEN BY A RAW QUERY AND NOT BY THE STORE. Before the roll-forward
// the column `close_outcome` does not exist, and the store of the NEW version at version
// 49 simply will not run — it physically cannot take the "as it was" snapshot. But even
// if it could, that would be the wrong way: comparing a query with itself
// is a tautology. The snapshot is written here with its own SQL, the horizon rule in it
// is written by hand, and it is compared with the STORE's answer after the roll-forward. Two
// independent counts that agreed on the same data.
//
// WHY THIS LIVES IN THE migrations PACKAGE AND NOT IN issues. The subject is the TRANSITION
// between schema versions, and the migrator exists only here. The precedent of importing
// internal/issues from this package is migration_000044_test.go and
// migration_000049_test.go.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
)

// seedPlainIssue inserts an issue with the COLUMNS OF VERSION 49 — without `close_outcome`,
// which does not exist at this version yet. With the same set with which today's
// production would insert it, knowing nothing about the outcome.
func seedPlainIssue(t *testing.T, pool *pgxpool.Pool, id string,
	projectID, creator uuid.UUID, issueType, status string,
) {
	t.Helper()
	var closedAt any
	var closedReason *string
	if status == "closed" {
		closedAt = time.Now().UTC()
		empty := ""
		closedReason = &empty
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, source_context, assignee, design, acceptance_criteria,
                    notes, estimated_minutes, due_at, defer_until, started_at, closed_at,
                    closed_reason, external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', $4, 2, $5, false, 'human', NULL, NULL, '', '', '',
        NULL, NULL, NULL, NULL, $6::timestamptz, $7,
        NULL, $8, now(), now(), '{}'::jsonb)`,
		id, projectID, "seed "+id, issueType, status, closedAt, closedReason, creator); err != nil {
		t.Fatalf("seed issue %s (%s/%s): %v", id, issueType, status, err)
	}
}

func seedParentEdge(t *testing.T, pool *pgxpool.Pool, childID, parentID string, creator uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`,
		childID, parentID, creator); err != nil {
		t.Fatalf("seed parent-child %s -> %s: %v", childID, parentID, err)
	}
}

// pairSnapshot — an independent raw count of "closed / total" over the direct children
// of one parent, as seen by the reader `caller`.
//
// The horizon rule is rewritten here BY HAND and not taken from
// authz.ProjectVisibleExpr: the point of the snapshot is that it is counted by not the same
// code that is being checked. The rule is the same — inaccessible ones stay in
// 'total', visible permanent ones drop out of it, only visible closed ones go into
// 'closed' — but it is written independently.
type pairSnapshot struct {
	closed int
	total  int
}

func snapshotPair(t *testing.T, pool *pgxpool.Pool, parentID string, caller uuid.UUID) pairSnapshot {
	t.Helper()
	var got pairSnapshot
	if err := pool.QueryRow(context.Background(), `
SELECT
  COUNT(*) FILTER (
    WHERE EXISTS (SELECT 1 FROM project_members m
                   WHERE m.project_id = c.project_id AND m.user_id = $2)
      AND c.status = 'closed') AS closed,
  COUNT(*) FILTER (
    WHERE NOT EXISTS (SELECT 1 FROM project_members m
                       WHERE m.project_id = c.project_id AND m.user_id = $2)
       OR c.status <> 'standing') AS total
  FROM dependencies d
  JOIN issues c ON c.id = d.from_id
 WHERE d.to_id = $1 AND d.type = 'parent-child'`, parentID, caller).
		Scan(&got.closed, &got.total); err != nil {
		t.Fatalf("snapshot pair of %s: %v", parentID, err)
	}
	return got
}

// TestMigration000050_ProgressNumbersSurviveTheUpgrade — row 22.
func TestMigration000050_ProgressNumbersSurviveTheUpgrade(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := migrator000050(t, url)

	// Down first: the fixture must be seeded at VERSION 49, otherwise the
	// 000050 trigger will set outcomes on insert and the snapshot will be taken already
	// after the feature — that is, it would prove nothing.
	if err := m.Migrate(migration000050Below); err != nil {
		t.Fatalf("Migrate(%d): %v", migration000050Below, err)
	}

	// An active instance admin, as an upgrading installation has one (the
	// api refuses to migrate without, config/startup_checks.go#PreMigration):
	// on the way up the transition to spaces (000071) puts the projects into
	// the common space he owns, so the reader keeps seeing them (INV-19).
	// Without him the projects would stay without a space and out of every
	// member's reach (INV-1 b) — the test would depend on admins left in the
	// shared database by other packages.
	admin := seedUser(t, pool, "m50a")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, admin); err != nil {
		t.Fatalf("make the admin: %v", err)
	}

	caller := seedUser(t, pool, "m50p")
	pid, prefix := seedProject(t, pool, caller, "m50p")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)`, pid, caller); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_members WHERE project_id = $1`, pid)
	})

	outsider := seedUser(t, pool, "m50h")
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider, "m50h")
	// Runs before the seeds' own cleanups: the spaces the transition made
	// for these users go first (spaces.owner_id is RESTRICT).
	t.Cleanup(func() {
		migDropTransitionSpaces(t, pool, []uuid.UUID{admin, caller, outsider}, []uuid.UUID{pid, hiddenPID})
	})

	// Ring: an epic with children in ALL statuses, plus a permanent and an inaccessible one.
	epicID := prefix + "-1"
	seedPlainIssue(t, pool, epicID, pid, caller, "epic", "open")
	for i, st := range []string{"open", "in_progress", "deferred", "closed", "standing"} {
		id := prefix + "-1." + itoaProgress(i+1)
		seedPlainIssue(t, pool, id, pid, caller, "task", st)
		seedParentEdge(t, pool, id, epicID, caller)
	}
	hiddenKid := hiddenPrefix + "-9"
	seedPlainIssue(t, pool, hiddenKid, hiddenPID, outsider, "task", "closed")
	seedParentEdge(t, pool, hiddenKid, epicID, outsider)

	// Branch: a parent ISSUE with two children. Its numbers are counted by its OWN reader,
	// and the snapshot must cover him too.
	branchID := prefix + "-2"
	seedPlainIssue(t, pool, branchID, pid, caller, "task", "open")
	for i, st := range []string{"closed", "open"} {
		id := prefix + "-2." + itoaProgress(i+1)
		seedPlainIssue(t, pool, id, pid, caller, "task", st)
		seedParentEdge(t, pool, id, branchID, caller)
	}

	before := map[string]pairSnapshot{
		epicID:   snapshotPair(t, pool, epicID, caller),
		branchID: snapshotPair(t, pool, branchID, caller),
	}
	// Precondition: the snapshot is non-empty and meaningful. The pair {0,0} would pass
	// any comparison and prove nothing.
	if before[epicID] != (pairSnapshot{closed: 1, total: 5}) {
		t.Fatalf("ring snapshot = %+v, want {closed:1 total:5} — the fixture is not the one "+
			"described (the permanent one is outside the \"total\", the inaccessible one is inside it)", before[epicID])
	}
	if before[branchID] != (pairSnapshot{closed: 1, total: 2}) {
		t.Fatalf("branch snapshot = %+v, want {closed:1 total:2}", before[branchID])
	}

	// Roll-forward.
	if err := m.Up(); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Precondition of the second half: the backfill ACTUALLY set the outcome. Without
	// it `noWork = 0` and `done = closed` would pass on an empty column too —
	// NULL does not equal 'done' by the leg predicate, but is not counted as one either — so
	// the check would hold on a coincidence of two voids.
	wantOutcome(t, pool, prefix+"-1.4", "done",
		"the 000050 backfill must set the outcome on every previously closed issue (US-8)")
	wantOutcome(t, pool, prefix+"-2.1", "done",
		"the same for the closed child of the branch")
	wantOutcome(t, pool, prefix+"-1.1", "",
		"and an UNclosed one must have no outcome (INV-1)")

	ctx := auth.WithUserRole(auth.WithUserID(context.Background(), caller), "user")
	store := issues.NewStore(pool)

	// Card ring.
	dto, err := store.GetIssue(ctx, epicID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", epicID, err)
	}
	if dto.Epic == nil {
		t.Fatalf("the epic has no summary")
	}
	ring := *dto.Epic
	if ring.ClosedChildren != before[epicID].closed || ring.TotalChildren != before[epicID].total {
		t.Errorf("ring after the roll-forward: closed/total = %d/%d, before it was %d/%d — "+
			"SC-002 promises THE SAME \"X of Y\" numbers",
			ring.ClosedChildren, ring.TotalChildren, before[epicID].closed, before[epicID].total)
	}
	assertSeededProgress(t, "ring", ring.DoneChildren, ring.NoWorkChildren,
		ring.CountedChildren, ring.ClosedChildren, ring.TotalChildren)

	// List row.
	list, err := store.ListIssues(ctx, issues.ListIssuesArgs{ProjectID: pid, Caller: &caller, Limit: 100})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	var row *issues.EpicProgress
	for _, it := range list.Items {
		if it.ID == epicID {
			row = it.EpicProgress
		}
	}
	if row == nil {
		t.Fatalf("the epic row came without epicProgress")
	}
	if row.Closed != before[epicID].closed || row.Total != before[epicID].total {
		t.Errorf("row chip after the roll-forward: %d/%d, before it %d/%d",
			row.Closed, row.Total, before[epicID].closed, before[epicID].total)
	}
	assertSeededProgress(t, "row chip", row.Done, row.NoWork, row.Counted, row.Closed, row.Total)

	// Tree branch.
	tree, err := store.IssueTree(ctx, issues.IssueTreeArgs{
		Scope:  authz.ForUser(caller),
		Caller: caller,
		RootID: &branchID, Types: []string{"parent-child"},
	})
	if err != nil {
		t.Fatalf("IssueTree(%s): %v", branchID, err)
	}
	var branch *issues.EpicProgress
	for _, node := range tree.Items {
		if node.Item.ID == branchID {
			branch = node.Item.EpicProgress
		}
	}
	if branch == nil {
		t.Fatalf("branch row %s came without epicProgress", branchID)
	}
	if branch.Closed != before[branchID].closed || branch.Total != before[branchID].total {
		t.Errorf("branch progress after the roll-forward: %d/%d, before it %d/%d",
			branch.Closed, branch.Total, before[branchID].closed, before[branchID].total)
	}
	assertSeededProgress(t, "branch", branch.Done, branch.NoWork, branch.Counted,
		branch.Closed, branch.Total)
}

// assertSeededProgress — the second half of SC-002: on the seeded data the new
// numbers are DERIVED from the old ones. The backfill sets "done" on all previously closed,
// so none dropped out, the numerator equals "closed", and the denominator equals
// "total". Any other combination means the backfill did not go through all
// rows or the "no work" leg counts the wrong thing.
func assertSeededProgress(t *testing.T, surface string, done, noWork, counted, closed, total int) {
	t.Helper()
	if noWork != 0 {
		t.Errorf("%s: noWork = %d, want 0 — the 000050 backfill sets \"done\" on ALL previously "+
			"closed, so none can drop out after the roll-forward", surface, noWork)
	}
	if done != closed {
		t.Errorf("%s: done = %d, closed = %d — on the seeded data these are the same number",
			surface, done, closed)
	}
	if counted != total {
		t.Errorf("%s: counted = %d, total = %d — none dropped out, the denominator equals the total",
			surface, counted, total)
	}
}

// itoaProgress formats a single-digit identifier suffix. Local,
// because the neighbouring files of this package do not introduce such a helper.
func itoaProgress(n int) string {
	return string(rune('0' + n))
}

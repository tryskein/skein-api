package projects

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedTypedIssueWithParent inserts an issues row with explicit type and
// optional parent_id. Used by sk-32 slice 3 tests to fabricate epic +
// children fixtures without going through CreateIssue (which expects a
// counter and applies validation we don't need here).
//
// sk-35: when parentID is non-nil we ALSO write the canonical
// parent-child dep edge — the epicsEligibleCount reader now sources
// children from the dep graph rather than issues.parent_id.
func seedTypedIssueWithParent(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID,
	idStr, status, issueType string, parentID *string) {
	t.Helper()
	closedAt := "NULL"
	if status == "closed" {
		closedAt = "now()"
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1::varchar, $2, $3, 1, $1::varchar, '', $4, 2, $5, false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL, NULL, `+closedAt+`, NULL,
        NULL, $6, now(), now(), '{}'::jsonb)`,
		idStr, projectID, parentID, issueType, status, creator); err != nil {
		t.Fatalf("seed typed issue %s: %v", idStr, err)
	}
	if parentID != nil && *parentID != "" {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())
ON CONFLICT (from_id, to_id, type) DO NOTHING`,
			idStr, *parentID, creator); err != nil {
			t.Fatalf("seed parent-child dep for %s: %v", idStr, err)
		}
	}
}

// TestListProjects_EpicsEligibleCount verifies the per-row
// epicsEligibleCount surfaces 2 when a project has 5 epics — 2 with
// every child closed, 2 with at least one open child, and 1 empty
// (skip, no children = ineligible).
func TestListProjects_EpicsEligibleCount(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "ec", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "ec-" + pid.String()[:8]

	// Epic 1 — 2 children, both closed → eligible.
	epic1 := prefix + "-1"
	seedTypedIssueWithParent(t, pool, pid, owner, epic1, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.1", "closed", "task", &epic1)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.2", "closed", "task", &epic1)

	// Epic 2 — 1 child, closed → eligible.
	epic2 := prefix + "-2"
	seedTypedIssueWithParent(t, pool, pid, owner, epic2, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-2.1", "closed", "task", &epic2)

	// Epic 3 — 2 children, 1 open → NOT eligible.
	epic3 := prefix + "-3"
	seedTypedIssueWithParent(t, pool, pid, owner, epic3, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-3.1", "closed", "task", &epic3)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-3.2", "open", "task", &epic3)

	// Epic 4 — 1 in-progress child → NOT eligible.
	epic4 := prefix + "-4"
	seedTypedIssueWithParent(t, pool, pid, owner, epic4, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-4.1", "in_progress", "task", &epic4)

	// Epic 5 — empty → NOT eligible (the second EXISTS prunes empties).
	epic5 := prefix + "-5"
	seedTypedIssueWithParent(t, pool, pid, owner, epic5, "open", "epic", nil)

	// Non-epic noise — should not affect the counter.
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-99", "open", "task", nil)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *ProjectListItem
	for i := range res.Items {
		if res.Items[i].ID == pid {
			got = &res.Items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("project %s not found in list", pid)
	}
	if got.EpicsEligibleCount != 2 {
		t.Errorf("EpicsEligibleCount = %d, want 2 (epic1+epic2)", got.EpicsEligibleCount)
	}
}

// TestListProjects_EpicsEligibleCount_EmptyProject confirms a project
// with zero epics returns EpicsEligibleCount = 0.
func TestListProjects_EpicsEligibleCount_EmptyProject(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "ecz", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range res.Items {
		if item.ID == pid && item.EpicsEligibleCount != 0 {
			t.Errorf("EpicsEligibleCount = %d on empty project; want 0",
				item.EpicsEligibleCount)
		}
	}
}

// (sk-33) TestListProjects_EpicsEligibleCount_DepWiredChildren was
// removed: the parent-child dep edge no longer exists; children come
// from issues.parent_id only. The hierarchical-children path is fully
// covered by TestListProjects_EpicsEligibleCount above.

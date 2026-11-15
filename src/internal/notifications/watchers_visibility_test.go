package notifications

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The caller's subscription lists (ListAllWatchers — /me/watchers and
// skein_watchers list — and its paged form ListWatchersPage) show a
// subscription only while the caller sees its project. The reference is the
// defect report: both read only the key's scope, so a subscription row that
// outlived the caller's access (access lost on a path that does not delete
// subscriptions) kept returning the issue title and the project name.
// DB-backed, skip without TEST_DATABASE_URL (in-package newTestPool).
func TestWatcherLists_HideProjectsNoLongerVisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedTestUser(t, pool, "user")
	member := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, member, owner)
	issueID := "wvis-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_watchers WHERE issue_id = $1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_watchers WHERE project_id = $1`, projectID)
	})

	store := NewStore(pool)
	addScoped(t, pool, issueID, member, ReasonManual, ScopeIssue)
	if err := store.AddProjectWatch(ctx, projectID, member); err != nil {
		t.Fatalf("AddProjectWatch: %v", err)
	}

	// listed reports whether each section of both lists holds the seeded
	// subscription.
	listed := func(t *testing.T) (allIssue, allProject, pageIssue, pageProject bool) {
		t.Helper()
		all, err := store.ListAllWatchers(ctx, member)
		if err != nil {
			t.Fatalf("ListAllWatchers: %v", err)
		}
		page, err := store.ListWatchersPage(ctx, WatchersPageArgs{UserID: member, Limit: 50})
		if err != nil {
			t.Fatalf("ListWatchersPage: %v", err)
		}
		for _, w := range all.Issues {
			allIssue = allIssue || w.IssueID == issueID
		}
		for _, w := range all.Projects {
			allProject = allProject || w.ProjectID == projectID
		}
		for _, w := range page.Issues {
			pageIssue = pageIssue || w.IssueID == issueID
		}
		for _, w := range page.Projects {
			pageProject = pageProject || w.ProjectID == projectID
		}
		return
	}

	// Control: while the member sees the project, both lists show both rows.
	if ai, ap, pi, pp := listed(t); !ai || !ap || !pi || !pp {
		t.Fatalf("visible project: ListAllWatchers issue=%v project=%v, ListWatchersPage issue=%v project=%v; want all true",
			ai, ap, pi, pp)
	}

	// Access lost without the revoke path: the membership rows go, the
	// subscription rows stay.
	if _, err := pool.Exec(ctx,
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, member); err != nil {
		t.Fatalf("delete project membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
DELETE FROM space_members
 WHERE user_id = $1
   AND space_id = (SELECT space_id FROM projects WHERE id = $2)`, member, projectID); err != nil {
		t.Fatalf("delete space membership: %v", err)
	}
	var visible bool
	if err := pool.QueryRow(ctx, `SELECT skein_project_visible($1, $2)`, member, projectID).Scan(&visible); err != nil {
		t.Fatalf("skein_project_visible: %v", err)
	}
	if visible {
		t.Fatal("precondition: the member still sees the project after losing both memberships")
	}
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $3)
     + (SELECT count(*) FROM project_watchers WHERE project_id = $2 AND user_id = $3)`,
		issueID, projectID, member).Scan(&rows); err != nil {
		t.Fatalf("count subscription rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("precondition: %d subscription rows left, want 2", rows)
	}

	if ai, ap, pi, pp := listed(t); ai || ap || pi || pp {
		t.Errorf("project no longer visible: ListAllWatchers issue=%v project=%v, ListWatchersPage issue=%v project=%v; want all false",
			ai, ap, pi, pp)
	}
}

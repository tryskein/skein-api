package issues

// activity_plan_test.go — sk-674.18.
// The activity feed binds the caller's project array (the HOT ZONE reach
// or the visible set) like the dashboard reads of
// dashboard.TestReachReads_LeaveNoNamedPreparedStatement, and for the same
// reason runs it planned with that array every time. Reference — the same
// contract, observed on the server: after repeated reads of the feed, the
// one connection they ran on holds no named prepared statement over a
// project array (uuid[]) that a generic plan could take over.
//
// sk-1007 (review of T011/T012): the page names a task and a project of
// another owner's project (a dependency.created with a foreign toProjectId,
// an issue.created with a foreign parent), so every read also runs the
// reference redaction — authz.VisibleIssueIDs and authz.VisibleAmong, both
// binding the visible set as a project array — under the same contract.

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/redact"
	"skein-api/internal/testsupport"
)

func TestListUserActivity_LeavesNoNamedPreparedStatement(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx := context.Background()
	shared := newTestPool(t)
	owner := seedUser(t, shared)
	pid, pPre := seedProject(t, shared, owner)
	stranger := seedUser(t, shared)
	foreign, fPre := seedProject(t, shared, stranger)
	from := testsupport.SeedIssue(t, shared, pid, owner, pPre+"-1", "source", "open", 2)
	to := testsupport.SeedIssue(t, shared, foreign, stranger, fPre+"-1", "foreign", "open", 2)
	ownerName := usernameOf(t, shared, owner)
	link := seedRefEvent(t, shared, owner, ownerName, linkEvent(owner.String(), from, to, pid, &foreign))
	seedRefEvent(t, shared, owner, ownerName, refEvent{
		typ: "issue.created", entityType: "issue", entityID: from, projectID: pid,
		newData: map[string]any{"id": from, "projectId": pid.String(), "parentId": to},
	})
	readerCtx := func() context.Context {
		return authz.WithRequestCache(auth.WithUserRole(auth.WithUserID(ctx, owner), "user"))
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns, cfg.MinConns = 1, 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	named := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE NOT from_sql AND 'uuid[]'::regtype = ANY(parameter_types)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	store := NewStore(pool)
	set := map[uuid.UUID]struct{}{pid: {}}
	for name, reach := range map[string]map[uuid.UUID]struct{}{"visible set (/me/activity)": nil, "HOT ZONE reach (dashboard feed)": set} {
		t.Run(name, func(t *testing.T) {
			before := named(t)
			for range 7 { // past the five custom executions after which a generic plan may take over
				items, _, err := store.ListUserActivity(readerCtx(), owner, set, false, nil, nil, 20, reach, redact.None)
				if err != nil {
					t.Fatalf("ListUserActivity: %v", err)
				}
				redacted := false
				for _, e := range items {
					if e.ID == link && e.Entity.ID == from+"::"+hiddenMarker+"::blocks" {
						redacted = true
					}
				}
				if len(items) != 2 || !redacted {
					t.Fatalf("the page (%d rows) does not carry the redacted link — the redaction reads did not run", len(items))
				}
			}
			if after := named(t); after != before {
				t.Errorf("the feed left %d named prepared statements over a project array on the connection", after-before)
			}
		})
	}
}

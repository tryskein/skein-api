package issues

// activity_plan_test.go — sk-674.18.
// The activity feed binds the caller's project array (the HOT ZONE reach
// or the visible set) like the dashboard reads of
// dashboard.TestReachReads_LeaveNoNamedPreparedStatement, and for the same
// reason runs it planned with that array every time. Reference — the same
// contract, observed on the server: after repeated reads of the feed, the
// one connection they ran on holds no named prepared statement over a
// project array (uuid[]) that a generic plan could take over.

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/redact"
)

func TestListUserActivity_LeavesNoNamedPreparedStatement(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx := context.Background()
	shared := newTestPool(t)
	owner := seedUser(t, shared)
	pid, _ := seedProject(t, shared, owner)

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
				if _, _, err := store.ListUserActivity(ctx, owner, set, false, nil, nil, 20, reach, redact.None); err != nil {
					t.Fatalf("ListUserActivity: %v", err)
				}
			}
			if after := named(t); after != before {
				t.Errorf("the feed left %d named prepared statements over a project array on the connection", after-before)
			}
		})
	}
}

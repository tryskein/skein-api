package dashboard

// reach_plan_test.go — sk-674.18 EXPLAIN gate (dashboard-scoring).
// Reference — the gate's finding, written before the fix: the HOT ZONE
// reads bind the reach as an array of project ids, and a named prepared
// statement over it switches to a generic plan after five executions —
// blind to the array, ~40× slower on the 10k corpus for every later call
// on that connection. The contract is therefore observable on the server:
// a reach-bound read leaves no named prepared statement behind (it is
// planned with its array every time), however often it runs. Statements
// with no project array (a plain MIN over events) are not concerned: their
// generic plan is their custom plan.
//
// One connection, so pg_prepared_statements is the state of the very
// session the reads ran on. Counterexample: the default exec mode leaves one
// stmtcache_* statement per distinct query, its parameter types listing
// uuid[].

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/redact"
)

func TestReachReads_LeaveNoNamedPreparedStatement(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx := context.Background()
	shared := newTestPool(t)
	owner, ownerUname := seedUser(t, shared)
	project := seedProject(t, shared, owner)
	pre := uniqueIDPrefix(t, "plan")
	id := pre + "-1"
	seedIssueRow(t, shared, project, id, "human", owner, nil)
	seedEventForIssue(t, shared, "issue.created", id, project, ownerUname, owner, "human", time.Now().UTC().Add(-time.Hour))

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
	store := New(pool)
	set := map[uuid.UUID]struct{}{project: {}}
	reads := map[string]func() error{
		"Scoring": func() error {
			_, err := store.Scoring(ctx, Window24h, set, false, nil, nil, set)
			return err
		},
		"Race": func() error {
			_, err := store.Race(ctx, RaceArgs{Metric: RaceMetricClosed, Window: RaceWindow7d, VisibleProjects: set, Reach: set})
			return err
		},
		"InProgress": func() error {
			_, err := store.InProgress(ctx, set, false, nil, 50, set, redact.None)
			return err
		},
		"Agents": func() error {
			_, err := store.Agents(ctx, set, false)
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			before := named(t)
			for range 7 { // past the five custom executions after which a generic plan may take over
				if err := read(); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			if after := named(t); after != before {
				t.Errorf("%s left %d named prepared statements over a project array on the connection — a generic plan can take them over", name, after-before)
			}
		})
	}
}

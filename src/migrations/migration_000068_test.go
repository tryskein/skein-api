package migrations

// migration_000068_test.go — sk-674.18, T010: the planner cost of the
// per-row boolean form of the access relation. Reference — the migration
// header, written before it: up declares skein_project_visible at COST 1000
// (the least value at which the index path of the fan-out's subtree arm wins
// on test No. 15's corpus), down returns it to the default 100. Observed on the
// catalogue the planner reads (pg_proc.procost).

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func m68Cost(t *testing.T, pool *pgxpool.Pool, stage string) float64 {
	t.Helper()
	var cost float64
	if err := pool.QueryRow(context.Background(),
		`SELECT procost FROM pg_proc WHERE oid = 'skein_project_visible(uuid, uuid)'::regprocedure`).Scan(&cost); err != nil {
		t.Fatalf("%s: read procost: %v", stage, err)
	}
	return cost
}

// TestMigration000068_VisiblePointCost — see the file header.
func TestMigration000068_VisiblePointCost(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")

	if err := m.Migrate(67); err != nil {
		t.Fatalf("Migrate(67): %v", err)
	}
	if got := m68Cost(t, pool, "down"); got != 100 {
		t.Errorf("down: procost = %v, want 100", got)
	}
	if err := m.Migrate(68); err != nil {
		t.Fatalf("Migrate(68): %v", err)
	}
	if got := m68Cost(t, pool, "up"); got != 1000 {
		t.Errorf("up: procost = %v, want 1000", got)
	}
}

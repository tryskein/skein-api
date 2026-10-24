package migrations

// migration_000067_test.go — sk-674.18, T009: the per-row boolean form of
// the access relation. Reference — the migration header, written before it:
//
//   - up/down: down removes the function, up brings it back;
//   - skein_project_visible(caller, project) is true exactly when the
//     relation skein_project_access_set(caller) has a row for the project,
//     for every actor × project of the T003 corpus, a missing project and an
//     unknown caller included (one rule, no copy);
//   - the planner keeps it an opaque call — the plan of a row filter names
//     the function — which is what keeps the big queries on their correlated
//     index-probe plans (SC-013).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

func m67Present(t *testing.T, pool *pgxpool.Pool, stage string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT to_regprocedure('skein_project_visible(uuid, uuid)') IS NOT NULL`).Scan(&ok); err != nil {
		t.Fatalf("%s: read function: %v", stage, err)
	}
	return ok
}

// TestMigration000067_VisiblePoint — see the file header.
func TestMigration000067_VisiblePoint(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	ctx := context.Background()

	if err := m.Migrate(66); err != nil {
		t.Fatalf("Migrate(66): %v", err)
	}
	if m67Present(t, pool, "down") {
		t.Fatalf("down: skein_project_visible still exists")
	}
	if err := m.Migrate(67); err != nil {
		t.Fatalf("Migrate(67): %v", err)
	}
	if !m67Present(t, pool, "up") {
		t.Fatalf("up: skein_project_visible missing")
	}
	m59Up(t, m, "head after round trip")

	f := testsupport.SeedTenancy(t, pool)
	missing := uuid.New()
	callers := []uuid.UUID{uuid.New()} // an unknown caller
	for _, a := range testsupport.TenancyActors {
		callers = append(callers, f.User(t, a).ID)
	}
	projectIDs := []uuid.UUID{missing}
	for _, p := range f.Projects() {
		projectIDs = append(projectIDs, p.ID)
	}
	for _, c := range callers {
		for _, p := range projectIDs {
			var fn, rel bool
			if err := pool.QueryRow(ctx, `
SELECT skein_project_visible($1, $2),
       EXISTS (SELECT 1 FROM skein_project_access_set($1) va WHERE va.project_id = $2)`,
				c, p).Scan(&fn, &rel); err != nil {
				t.Fatalf("caller %s, project %s: %v", c, p, err)
			}
			if fn != rel {
				t.Errorf("caller %s, project %s: skein_project_visible %v, relation row %v", c, p, fn, rel)
			}
		}
	}

	var plan strings.Builder
	rows, err := pool.Query(ctx,
		`EXPLAIN SELECT i.id FROM issues i WHERE skein_project_visible($1, i.project_id)`, f.User(t, testsupport.ActorOnlyA).ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "skein_project_visible(") {
		t.Errorf("the row filter does not call skein_project_visible — the planner inlined it, and big "+
			"queries lose their correlated plans:\n%s", plan.String())
	}
}

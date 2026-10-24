package migrations

// migration_000046_test.go — sk-303.14, tasks T012 and T014.
//
// The migration introduces a PARTIAL index under the unreviewed set.
// Checking "the index exists" by name is useless: the name says nothing
// about the WHERE body or column order, and those are exactly where a
// mistake can be made — e.g. forgetting `reviewed_at IS NULL` in the body
// (the index bloats and loses selectivity) or swapping the pagination
// pair (the query falls back to sorting the result). So here the index
// DEFINITION is compared, as pg_get_indexdef returns it.
//
// Applicability of the index by real queries is not here: that is the
// matrix-row-61 gate, and it belongs to class T014, where there is a
// representative data volume and query plans. Here is only that the
// migration created exactly what that class rests on, and that rollback
// drops it whole. The split turned out not to be formal: that class also
// cut the migration's index set from two to one, showing the planner does
// not pick the second.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// indexDef returns the index definition, or an empty string if there is no index.
func indexDef(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var def *string
	if err := testPool(t).QueryRow(ctx, `
SELECT pg_get_indexdef(c.oid)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relname = $1 AND n.nspname = 'public' AND c.relkind = 'i'`, name).Scan(&def); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return ""
		}
		t.Fatalf("reading index %s: %v", name, err)
	}
	if def == nil {
		return ""
	}
	return *def
}

// unreviewedIndexes — index name and definition fragments, each of which
// must be present. Fragments are chosen so each catches its own breakage,
// not "something changed".
//
// A map, not a single entry, is deliberate: the migration started with two
// indexes, and the plan check (T014, row 61) reduced them to one. The table
// shape is left so bringing a second index back is a data edit, not a
// rewrite of assertions.
var unreviewedIndexes = map[string][]string{
	"idx_issues_unreviewed_creator": {
		// Leading column is created_by: without it the filter on both paths
		// and the second-number counter lose their entry into the index.
		"(created_by,",
		// Pagination pair in the same order as the lists' ORDER BY.
		"updated_at DESC",
		"id DESC",
		// Partial body in full: each condition cuts the table, and losing
		// any one bloats the index while leaving it "working".
		"status = 'closed'",
		"review_required",
		"reviewed_at IS NULL",
	},
}

// TestMigration000046_CreatesPartialUnreviewedIndexes — the migration's
// indexes are created, and created as partial and in this column order.
func TestMigration000046_CreatesPartialUnreviewedIndexes(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()

	for name, wantParts := range unreviewedIndexes {
		def := indexDef(t, ctx, name)
		if def == "" {
			t.Errorf("index %s is missing", name)
			continue
		}
		if !strings.Contains(def, "WHERE") {
			t.Errorf("index %s is not partial — WHERE body is lost, the index covers the whole table: %s",
				name, def)
		}
		for _, part := range wantParts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s definition has no %q: %s", name, part, def)
			}
		}
	}
}

// TestMigration000046_DownRemovesIndexes — rollback drops the migration's
// indexes, and a re-apply restores them.
//
// Indexes are pure performance, so rollback has no caveats about data
// preservation. Walk the whole set, not one name: the migration's set
// already changed once, and the assertion must follow it.
func TestMigration000046_DownRemovesIndexes(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Return to head is registered BEFORE the down step: a failing
	// assertion below would otherwise leave the shared database at
	// version 45 and break fixtures of later packages in a sequential run.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	if err := m.Migrate(45); err != nil {
		t.Fatalf("Migrate(45) (000046 down): %v", err)
	}
	for name := range unreviewedIndexes {
		if def := indexDef(t, ctx, name); def != "" {
			t.Errorf("after rollback index %s remained: %s", name, def)
		}
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	for name := range unreviewedIndexes {
		if def := indexDef(t, ctx, name); def == "" {
			t.Errorf("after re-apply index %s did not return", name)
		}
	}
}

package migrations

// migration_000056_test.go — sk-551.8: the personal left-panel layout mode
// in user_ui_prefs. Reference — spec sk-551.8, section "Data / entities /
// migrations" and the contract "user_ui_prefs profile" (D3, D5): a mode field with
// the vocabulary columns / sections, default columns, no NULL; the migration is
// additive — existing rows get columns; the rollback drops only
// this field.
//
// Beyond the 000055 pattern the test holds two cases that it does not see:
//   - upgrade of an existing row: the row is created on the 000055 schema (with
//     density and issue_modal_views), after up to 000056 it reads as
//     columns, and the neighbouring fields are intact — the pattern's INSERT goes on the already applied
//     schema and does not check the backfill of old rows;
//   - the rollback keeper carries both density and issue_modal_views: the 000056 rollback
//     must not drop any of the neighbouring fields.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func assertNavLayoutUp(t *testing.T, pool *pgxpool.Pool, stage string) {
	t.Helper()
	if !columnExists(t, pool, "user_ui_prefs", "nav_layout") {
		t.Fatalf("%s: user_ui_prefs.nav_layout is missing", stage)
	}
	ctx := context.Background()
	user := seedUser(t, pool, "m56"+stage[:3])
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_ui_prefs WHERE user_id = $1`, user)
	})

	// A row that does not name the layout gets "Columns side by side".
	if _, err := pool.Exec(ctx, `INSERT INTO user_ui_prefs (user_id) VALUES ($1)`, user); err != nil {
		t.Fatalf("%s: insert bare row: %v", stage, err)
	}
	if got := readNavLayout(t, pool, user); got != "columns" {
		t.Errorf("%s: a new row has nav_layout = %q, want columns", stage, got)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE user_ui_prefs SET nav_layout = 'sections' WHERE user_id = $1`, user); err != nil {
		t.Errorf("%s: sections was refused: %v", stage, err)
	} else if got := readNavLayout(t, pool, user); got != "sections" {
		t.Errorf("%s: after writing sections nav_layout = %q", stage, got)
	}

	// A word outside the vocabulary and NULL are refused, not stored.
	for _, bad := range []struct {
		name, sql, code string
	}{
		{"unknown word", `UPDATE user_ui_prefs SET nav_layout = 'tiles' WHERE user_id = $1`, "23514"},
		{"case matters", `UPDATE user_ui_prefs SET nav_layout = 'Sections' WHERE user_id = $1`, "23514"},
		{"NULL", `UPDATE user_ui_prefs SET nav_layout = NULL WHERE user_id = $1`, "23502"},
	} {
		_, err := pool.Exec(ctx, bad.sql, user)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != bad.code {
			t.Errorf("%s: %s: want SQLSTATE %s, got %v", stage, bad.name, bad.code, err)
		}
	}
}

func readNavLayout(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		`SELECT nav_layout FROM user_ui_prefs WHERE user_id = $1`, user).Scan(&v); err != nil {
		t.Fatalf("read nav_layout: %v", err)
	}
	return v
}

// assertNeighboursIntact checks the fields of 000054 and 000055 on a row
// that lives through a 000056 rollback or re-apply.
func assertNeighboursIntact(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, stage string,
	wantViews []string, wantDensity string) {
	t.Helper()
	var (
		views   []string
		density string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT issue_modal_views, density FROM user_ui_prefs WHERE user_id = $1`, user).
		Scan(&views, &density); err != nil {
		t.Fatalf("%s: row is unreadable: %v", stage, err)
	}
	if !reflect.DeepEqual(views, wantViews) {
		t.Errorf("%s: issue_modal_views = %v, want %v untouched", stage, views, wantViews)
	}
	if density != wantDensity {
		t.Errorf("%s: density = %q, want %q untouched", stage, density, wantDensity)
	}
}

func TestMigration000056_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	assertNavLayoutUp(t, pool, "before rollback")

	// A row that outlives the rollback: the settings of 000054 and 000055
	// must survive the nav_layout column going away.
	ctx := context.Background()
	keeper := seedUser(t, pool, "m56keep")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_ui_prefs WHERE user_id = $1`, keeper)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_ui_prefs (user_id, issue_modal_views, density, nav_layout)
		 VALUES ($1, ARRAY['kanban'], 'compact', 'sections')`,
		keeper); err != nil {
		t.Fatalf("insert keeper row: %v", err)
	}

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// down: the column goes, the neighbours stay.
	if err := m.Migrate(55); err != nil {
		t.Fatalf("Migrate(55) (000056 down): %v", err)
	}
	if columnExists(t, pool, "user_ui_prefs", "nav_layout") {
		t.Error("after rollback the column user_ui_prefs.nav_layout remained")
	}
	assertNeighboursIntact(t, pool, keeper, "after rollback", []string{"kanban"}, "compact")

	// A row created on the 000055 schema, before the layout column exists.
	legacy := seedUser(t, pool, "m56old")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_ui_prefs WHERE user_id = $1`, legacy)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_ui_prefs (user_id, issue_modal_views, density)
		 VALUES ($1, ARRAY['tree'], 'compact')`,
		legacy); err != nil {
		t.Fatalf("insert row on the 000055 schema: %v", err)
	}

	// up: the column returns; existing rows read as columns, neighbours intact.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	if got := readNavLayout(t, pool, legacy); got != "columns" {
		t.Errorf("upgrade: a row created on 000055 reads nav_layout = %q, want columns", got)
	}
	assertNeighboursIntact(t, pool, legacy, "upgrade", []string{"tree"}, "compact")
	if got := readNavLayout(t, pool, keeper); got != "columns" {
		t.Errorf("re-apply: the keeper row reads nav_layout = %q, want columns", got)
	}
	assertNeighboursIntact(t, pool, keeper, "re-apply", []string{"kanban"}, "compact")
	assertNavLayoutUp(t, pool, "after re-apply")

	// down again: the column goes once more, the neighbours still stay.
	if err := m.Migrate(55); err != nil {
		t.Fatalf("second Migrate(55) (000056 down): %v", err)
	}
	if columnExists(t, pool, "user_ui_prefs", "nav_layout") {
		t.Error("after the second rollback the column user_ui_prefs.nav_layout remained")
	}
	assertNeighboursIntact(t, pool, keeper, "after second rollback", []string{"kanban"}, "compact")
	assertNeighboursIntact(t, pool, legacy, "after second rollback", []string{"tree"}, "compact")
}

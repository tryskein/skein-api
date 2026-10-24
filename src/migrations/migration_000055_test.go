package migrations

// migration_000055_test.go — sk-551.7: the personal interface density in
// user_ui_prefs. Reference — the maintainers' decision (sk-551.7, 2026-10-05):
// "Density: Normal / Compact", the default mode is "Normal";
// storage is a profile field in user_ui_prefs. Up adds the column; a row
// written without naming the density gets normal; the CHECK refuses a
// word outside the vocabulary; down removes the column and keeps the
// window setting of 000054; up again restores the column. Same shape as
// the 000053 / 000054 tests.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func assertDensityUp(t *testing.T, pool *pgxpool.Pool, stage string) {
	t.Helper()
	if !columnExists(t, pool, "user_ui_prefs", "density") {
		t.Fatalf("%s: user_ui_prefs.density is missing", stage)
	}
	ctx := context.Background()
	user := seedUser(t, pool, "m55"+stage[:3])
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_ui_prefs WHERE user_id = $1`, user)
	})

	// A row that does not name the density gets "Normal".
	if _, err := pool.Exec(ctx, `INSERT INTO user_ui_prefs (user_id) VALUES ($1)`, user); err != nil {
		t.Fatalf("%s: insert bare row: %v", stage, err)
	}
	var density string
	if err := pool.QueryRow(ctx,
		`SELECT density FROM user_ui_prefs WHERE user_id = $1`, user).Scan(&density); err != nil {
		t.Fatalf("%s: read density: %v", stage, err)
	}
	if density != "normal" {
		t.Errorf("%s: a new row has density = %q, want normal", stage, density)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE user_ui_prefs SET density = 'compact' WHERE user_id = $1`, user); err != nil {
		t.Errorf("%s: compact was refused: %v", stage, err)
	}

	// A word outside the vocabulary and NULL are refused, not stored.
	for _, bad := range []struct {
		name, sql, code string
	}{
		{"unknown word", `UPDATE user_ui_prefs SET density = 'tiny' WHERE user_id = $1`, "23514"},
		{"case matters", `UPDATE user_ui_prefs SET density = 'Compact' WHERE user_id = $1`, "23514"},
		{"NULL", `UPDATE user_ui_prefs SET density = NULL WHERE user_id = $1`, "23502"},
	} {
		_, err := pool.Exec(ctx, bad.sql, user)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != bad.code {
			t.Errorf("%s: %s: want SQLSTATE %s, got %v", stage, bad.name, bad.code, err)
		}
	}
}

func TestMigration000055_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	assertDensityUp(t, pool, "before rollback")

	// A row that outlives the rollback: the window setting of 000054 must
	// survive the density column going away.
	ctx := context.Background()
	keeper := seedUser(t, pool, "m55keep")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_ui_prefs WHERE user_id = $1`, keeper)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_ui_prefs (user_id, issue_modal_views, density) VALUES ($1, ARRAY['kanban'], 'compact')`,
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

	if err := m.Migrate(54); err != nil {
		t.Fatalf("Migrate(54) (000055 down): %v", err)
	}
	if columnExists(t, pool, "user_ui_prefs", "density") {
		t.Error("after rollback the column user_ui_prefs.density remained")
	}
	var views []string
	if err := pool.QueryRow(ctx,
		`SELECT issue_modal_views FROM user_ui_prefs WHERE user_id = $1`, keeper).Scan(&views); err != nil {
		t.Fatalf("after rollback the keeper row is unreadable: %v", err)
	}
	if !reflect.DeepEqual(views, []string{"kanban"}) {
		t.Errorf("after rollback issue_modal_views = %v, want [kanban] untouched", views)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	assertDensityUp(t, pool, "after re-apply")
}

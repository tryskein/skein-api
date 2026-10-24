package migrations

// migration_000054_test.go — sk-551.5: user_ui_prefs, the per-user set of
// task views that open an issue as a pop-up window. Reference — the owner's
// decision (sk-551.5, 2026-10-05): "all off by default", the set of
// views is only Cards / Board / Tree. Up creates the table; a row
// written without naming the set gets the empty set; the CHECK refuses a
// name outside the vocabulary and a NULL element; down removes the table;
// up again restores it. Same shape as the 000053 test.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func assertUIPrefsUp(t *testing.T, pool *pgxpool.Pool, stage string) {
	t.Helper()
	if !tableExists(t, pool, "user_ui_prefs") {
		t.Fatalf("%s: table user_ui_prefs is missing", stage)
	}
	ctx := context.Background()
	user := seedUser(t, pool, "m54"+stage[:3])
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_ui_prefs WHERE user_id = $1`, user)
	})

	// A row that does not name the set gets it EMPTY — "all off
	// by default".
	if _, err := pool.Exec(ctx, `INSERT INTO user_ui_prefs (user_id) VALUES ($1)`, user); err != nil {
		t.Fatalf("%s: insert bare row: %v", stage, err)
	}
	var views []string
	if err := pool.QueryRow(ctx,
		`SELECT issue_modal_views FROM user_ui_prefs WHERE user_id = $1`, user).Scan(&views); err != nil {
		t.Fatalf("%s: read set: %v", stage, err)
	}
	if views == nil || len(views) != 0 {
		t.Errorf("%s: a new row has issue_modal_views = %#v, want the empty set", stage, views)
	}

	// The three known names are accepted together.
	if _, err := pool.Exec(ctx,
		`UPDATE user_ui_prefs SET issue_modal_views = ARRAY['cards','kanban','tree'] WHERE user_id = $1`,
		user); err != nil {
		t.Errorf("%s: the full vocabulary was refused: %v", stage, err)
	}

	// A name outside the vocabulary and a NULL element are refused by the
	// CHECK (SQLSTATE 23514), not stored.
	for _, bad := range []struct {
		name string
		sql  string
	}{
		{"unknown view", `UPDATE user_ui_prefs SET issue_modal_views = ARRAY['board'] WHERE user_id = $1`},
		{"NULL element", `UPDATE user_ui_prefs SET issue_modal_views = ARRAY[NULL::text] WHERE user_id = $1`},
	} {
		_, err := pool.Exec(ctx, bad.sql, user)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: %s: want check_violation 23514, got %v", stage, bad.name, err)
		}
	}
}

func TestMigration000054_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	assertUIPrefsUp(t, pool, "before rollback")

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

	if err := m.Migrate(53); err != nil {
		t.Fatalf("Migrate(53) (000054 down): %v", err)
	}
	if tableExists(t, pool, "user_ui_prefs") {
		t.Error("after rollback the table user_ui_prefs remained")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	assertUIPrefsUp(t, pool, "after re-apply")
}

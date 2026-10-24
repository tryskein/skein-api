package migrations

// migration_000053_test.go — sk-551.10, item 5: the personal "unwatch on
// close» flag. Up adds the column, off by default; down removes it; up
// again restores it. Same shape as the 000052 test: absolute versions and
// the return to head registered before the first step down.
//
// sk-622.21 (test matrix row 27): the default is checked ON VERSION 53, not
// after Up to head — 000059 later turns the default on (US-2), so reading the
// head's default would test that migration instead of this one.

import (
	"context"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func assertUnwatchOnCloseUp(t *testing.T, pool *pgxpool.Pool, stage string) {
	t.Helper()
	if !columnExists(t, pool, "user_notification_prefs", "unwatch_on_close") {
		t.Fatalf("%s: user_notification_prefs.unwatch_on_close is missing", stage)
	}
	// A prefs row written without naming the flag gets it OFF — the
	// acceptance's "off by default".
	user := seedUser(t, pool, "m53"+stage[:3])
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, user)
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, default_inbox_scope, suppress_agent_actions, updated_at)
VALUES ($1, 'all', true, now())`, user); err != nil {
		t.Fatalf("%s: insert prefs row: %v", stage, err)
	}
	var on bool
	if err := pool.QueryRow(ctx,
		`SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1`, user).Scan(&on); err != nil {
		t.Fatalf("%s: read flag: %v", stage, err)
	}
	if on {
		t.Errorf("%s: a new prefs row has unwatch_on_close = true, want false", stage)
	}
}

func TestMigration000053_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)

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
		t.Fatalf("Migrate(53): %v", err)
	}
	assertUnwatchOnCloseUp(t, pool, "before rollback")

	if err := m.Migrate(52); err != nil {
		t.Fatalf("Migrate(52) (000053 down): %v", err)
	}
	if columnExists(t, pool, "user_notification_prefs", "unwatch_on_close") {
		t.Error("after rollback user_notification_prefs.unwatch_on_close remained")
	}

	if err := m.Migrate(53); err != nil {
		t.Fatalf("Migrate(53) after down (000053 up): %v", err)
	}
	assertUnwatchOnCloseUp(t, pool, "after re-apply")
}

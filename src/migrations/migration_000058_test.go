package migrations

// migration_000058_test.go — sk-565, T027, test matrix rows 1–2 (D3, INV-8
// on the schema side). Reference — spec sk-565, "Data / entities /
// migrations": a personal flag "assign agent tasks to me", NOT NULL, default
// true; existing rows get true; the rollback drops
// only it.
//
// Beyond the 000053 sample (whose rollback is pinned to 52 because 000053
// was the head then): the rollback here is pinned to 57, a row is seeded on
// the 57 schema BEFORE up so the backfill of existing rows is observed, and
// the neighbouring columns are seeded with NON-factory values so a down that
// recreates the table or rewrites rows is visible — on factory values it
// would not be.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const m58Column = "assign_agent_issues_to_me"

// m58Migrator opens a migrator on the test database and registers the
// return to head. Registered before the first step down, so the suite is
// left at head whatever the test does.
func m58Migrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Errorf("restore Up after down: %v", err)
		}
		_, _ = m.Close()
	})
	return m
}

// insertBarePrefsRow writes a prefs row naming no column but the key — the
// form a row created by someone else's upsert takes for the columns it does
// not name.
func insertBarePrefsRow(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, stage string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, user); err != nil {
		t.Fatalf("%s: insert prefs row: %v", stage, err)
	}
}

func readAssignFlag(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, stage string) bool {
	t.Helper()
	var on bool
	if err := pool.QueryRow(context.Background(),
		`SELECT `+m58Column+` FROM user_notification_prefs WHERE user_id = $1`, user).Scan(&on); err != nil {
		t.Fatalf("%s: read %s: %v", stage, m58Column, err)
	}
	return on
}

// m58Neighbours are the personal settings that share the row with the new
// flag.
type m58Neighbours struct {
	reviewRequiredDefault, suppressAgentActions, unwatchOnClose bool
}

func readNeighbours(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, stage string) m58Neighbours {
	t.Helper()
	var n m58Neighbours
	if err := pool.QueryRow(context.Background(), `
SELECT review_required_default, suppress_agent_actions, unwatch_on_close
  FROM user_notification_prefs WHERE user_id = $1`, user).
		Scan(&n.reviewRequiredDefault, &n.suppressAgentActions, &n.unwatchOnClose); err != nil {
		t.Fatalf("%s: read neighbours: %v", stage, err)
	}
	return n
}

// TestMigration000058_UpBackfillsExistingRowsAndDefaultsTrue — row 1.
func TestMigration000058_UpBackfillsExistingRowsAndDefaultsTrue(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()
	m := m58Migrator(t)

	if err := m.Migrate(57); err != nil {
		t.Fatalf("Migrate(57) (000058 down): %v", err)
	}
	// A row that exists before the column does.
	existing := seedUser(t, pool, "m58e")
	insertBarePrefsRow(t, pool, existing, "on 57")

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Up (000058 up): %v", err)
	}
	if !columnExists(t, pool, "user_notification_prefs", m58Column) {
		t.Fatalf("after up user_notification_prefs.%s is missing", m58Column)
	}
	if !readAssignFlag(t, pool, existing, "existing row") {
		t.Errorf("a row that existed before 000058 reads %s = false, want true", m58Column)
	}

	// A row written after up without naming the flag gets it on.
	fresh := seedUser(t, pool, "m58f")
	insertBarePrefsRow(t, pool, fresh, "after up")
	if !readAssignFlag(t, pool, fresh, "new row") {
		t.Errorf("a new row written without the column reads %s = false, want true", m58Column)
	}

	// The flag has no third state: an explicit NULL is refused (not_null_violation).
	nulled := seedUser(t, pool, "m58n")
	_, err := pool.Exec(ctx,
		`INSERT INTO user_notification_prefs (user_id, `+m58Column+`) VALUES ($1, NULL)`, nulled)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Errorf("explicit NULL in %s: want SQLSTATE 23502, got %v", m58Column, err)
	}
}

// TestMigration000058_DownDropsOnlyTheColumn — row 2.
func TestMigration000058_DownDropsOnlyTheColumn(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()
	m := m58Migrator(t)

	// Neighbours at NON-factory values (factory: true, true, false), the
	// flag itself switched off.
	user := seedUser(t, pool, "m58d")
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs
    (user_id, review_required_default, suppress_agent_actions, unwatch_on_close, `+m58Column+`)
VALUES ($1, false, false, true, false)`, user); err != nil {
		t.Fatalf("seed prefs row: %v", err)
	}
	want := m58Neighbours{reviewRequiredDefault: false, suppressAgentActions: false, unwatchOnClose: true}
	if got := readNeighbours(t, pool, user, "before down"); got != want {
		t.Fatalf("seeded neighbours read %+v, want %+v", got, want)
	}

	if err := m.Migrate(57); err != nil {
		t.Fatalf("Migrate(57) (000058 down): %v", err)
	}
	_, err := pool.Exec(ctx,
		`SELECT `+m58Column+` FROM user_notification_prefs WHERE user_id = $1`, user)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42703" {
		t.Errorf("after down selecting %s: want SQLSTATE 42703 (undefined_column), got %v", m58Column, err)
	}
	if got := readNeighbours(t, pool, user, "after down"); got != want {
		t.Errorf("after down neighbours read %+v, want %+v (unchanged)", got, want)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Up after down: %v", err)
	}
	if !readAssignFlag(t, pool, user, "after re-apply") {
		t.Errorf("after re-apply %s = false, want true (the column comes back with its default)", m58Column)
	}
	if got := readNeighbours(t, pool, user, "after re-apply"); got != want {
		t.Errorf("after re-apply neighbours read %+v, want %+v (unchanged)", got, want)
	}
}

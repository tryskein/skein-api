package migrations

// migration_000062_test.go — sk-622.21, T025, test matrix row No. 52 (INV-1 in
// R2: a tombstone is impossible; the R2 → R1 rollback is allowed). The row
// names the file migration_000061_test.go; the migration of T025 is 000062
// (000061 was taken by the R1 reconciliation indexes — lead's note on
// sk-637.4 / sk-637.21).
//
// Reference — plan T025 and the spec, written before the migration:
//
//   - after up an issue_watchers row whose reasons contain 'unwatched' is
//     refused by the CHECK, and the constraint is validated (not left
//     NOT VALID);
//   - down drops the CHECK (a tombstone is accepted again — what R1 runs
//     on) and restores the R1 reconciliation indexes;
//   - a repeated up passes over such a leftover row: it becomes what the
//     new version writes — an opt-out, no subscription row, the user's
//     Watching card of that issue cleared — and the CHECK is back.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func m62ConstraintValidated(t *testing.T, pool *pgxpool.Pool) (exists, validated bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
SELECT true, convalidated FROM pg_constraint
 WHERE conrelid = 'issue_watchers'::regclass AND conname = 'issue_watchers_no_tombstone'`).
		Scan(&exists, &validated)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read constraint: %v", err)
	}
	return exists, validated
}

func m62IndexExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, name).Scan(&ok); err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return ok
}

// TestMigration000062_TombstoneImpossibleAndRollbackAllowed — No. 52.
func TestMigration000062_TombstoneImpossibleAndRollbackAllowed(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	ctx := context.Background()

	owner := seedUser(t, pool, "m62o")
	user := seedUser(t, pool, "m62u")
	pid, prefix := seedProject(t, pool, owner, "m62")
	issue := prefix + "-1"
	seedIssue(t, pool, issue, pid, owner, "")

	// R2: a tombstone is refused, by the named, validated constraint.
	if exists, validated := m62ConstraintValidated(t, pool); !exists || !validated {
		t.Fatalf("after up: constraint exists=%v validated=%v, want both true", exists, validated)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{unwatched}')`, issue, user)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "issue_watchers_no_tombstone" {
		t.Fatalf("tombstone insert after up: err = %v, want check_violation (23514) of issue_watchers_no_tombstone", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{creator,unwatched}')`, issue, user); err == nil {
		t.Fatalf("a mixed row carrying 'unwatched' was accepted after up")
	}
	for _, idx := range []string{"idx_issue_watchers_tombstone", "idx_notif_card_unresolved"} {
		if m62IndexExists(t, pool, idx) {
			t.Errorf("after up the R1 reconciliation index %s is still there", idx)
		}
	}

	// R2 → R1: down drops the CHECK, restores the indexes, accepts a
	// tombstone again.
	if err := m.Migrate(61); err != nil {
		t.Fatalf("Migrate(61) (000062 down): %v", err)
	}
	if exists, _ := m62ConstraintValidated(t, pool); exists {
		t.Fatalf("after down the constraint is still there")
	}
	for _, idx := range []string{"idx_issue_watchers_tombstone", "idx_notif_card_unresolved"} {
		if !m62IndexExists(t, pool, idx) {
			t.Errorf("after down the R1 reconciliation index %s is missing", idx)
		}
	}
	m59Exec(t, pool, "tombstone after down",
		`INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{unwatched}')`, issue, user)
	card := m59SeedFeedRow(t, pool, pid, issue, user)
	m59Exec(t, pool, "card the row", `UPDATE notifications SET card_issue_id = $2 WHERE id = $1`, card, issue)

	// Up again: the leftover tombstone becomes the new version's shape.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("repeated Up over a leftover tombstone: %v", err)
	}
	if n := m59Count(t, pool, "rows after re-up",
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issue, user); n != 0 {
		t.Errorf("after re-up %d subscription rows of the tombstone pair remain, want 0", n)
	}
	if n := m59Count(t, pool, "opt-out after re-up",
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, issue, user); n != 1 {
		t.Errorf("after re-up opt-outs of the pair = %d, want 1", n)
	}
	if n := m59Count(t, pool, "card after re-up",
		`SELECT count(*) FROM notifications WHERE id = $1 AND card_issue_id IS NOT NULL`, card); n != 0 {
		t.Errorf("after re-up the user's notification still carries the card of the unwatched issue")
	}
	if exists, validated := m62ConstraintValidated(t, pool); !exists || !validated {
		t.Errorf("after re-up: constraint exists=%v validated=%v, want both true", exists, validated)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_watch_optouts WHERE issue_id = $1`, issue)
	})
}

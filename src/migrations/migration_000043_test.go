package migrations

// migration_000043_test.go — sk-343, second half of the task.
//
// ⚠ The migration pins a REQUIREMENT CHANGE, it does not fix a defect:
// before it the false default was the right answer to the previous
// behaviour — the setting silenced even what is addressed to the
// recipient personally. Turning it on for everyone became possible only
// after the first half of sk-343, which lifted suppression from the
// personal addressee.
//
// What is proved here and why this way:
//
//  1. REWRITING EXISTING ROWS. A row that stood at false stands at true
//     after the migration — including the one a person turned off
//     themselves. The column has no third state, so "never touched" and
//     "deliberately turned off" are indistinguishable, and the product
//     owner accepted this side effect explicitly. A row already at true
//     must not be harmed — otherwise a migration that "rewrites
//     everything in sight" would pass the test proving nothing.
//
//  2. COLUMN DEFAULT. Checked by inserting a row WITHOUT this column,
//     not by reading information_schema: what is observed is the value
//     such an insert actually gets.
//
//  3. COUNTER. The number of rewritten rows is taken by an INDEPENDENT
//     select before the migration and checked against what the
//     migration wrote into migration_counters. Comparing the counter
//     with itself would prove nothing.
//
//  4. ROLLBACK. down returns the column default to false and does NOT
//     restore previous row values — that is a declared property of
//     rollback, and it is checked so the next reader does not take it
//     for an omission.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertPrefsRowDefaulted inserts a user_notification_prefs row WITHOUT naming
// suppress_agent_actions, so the value it ends up with is the column's DEFAULT
// — the only way to observe that default as behaviour rather than as metadata.
func insertPrefsRowDefaulted(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) bool {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, updated_at) VALUES ($1, now())
ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		t.Fatalf("insert defaulted prefs row: %v", err)
	}
	var got bool
	if err := pool.QueryRow(ctx,
		`SELECT suppress_agent_actions FROM user_notification_prefs WHERE user_id = $1`,
		userID).Scan(&got); err != nil {
		t.Fatalf("read defaulted prefs row: %v", err)
	}
	return got
}

func readSuppress(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) bool {
	t.Helper()
	var got bool
	if err := pool.QueryRow(context.Background(),
		`SELECT suppress_agent_actions FROM user_notification_prefs WHERE user_id = $1`,
		userID).Scan(&got); err != nil {
		t.Fatalf("read suppress_agent_actions: %v", err)
	}
	return got
}

func TestMigration000043_SuppressAgentActionsDefaultOn(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	m, captured := capturingMigrator(t, url)

	// State BEFORE this migration.
	if err := m.Migrate(42); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(42): %v", err)
	}
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	// At version 42 the column default must be off — otherwise the
	// "default change" below changes nothing and the test is green by
	// coincidence.
	defaulted := seedUser(t, pool, "w43a")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, defaulted)
	})
	if got := insertPrefsRowDefaulted(t, pool, defaulted); got {
		t.Fatal("at version 42 the column default is already true — there is nothing to compare before and after")
	}

	// Three fixtures: turned off deliberately, turned off by default (the
	// row above), and already on. The third is the control "the migration
	// does not touch extra".
	offByChoice := seedUser(t, pool, "w43b")
	alreadyOn := seedUser(t, pool, "w43c")
	noRow := seedUser(t, pool, "w43d")
	for _, u := range []uuid.UUID{offByChoice, alreadyOn, noRow} {
		uu := u
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM user_notification_prefs WHERE user_id = $1`, uu)
		})
	}
	for _, tc := range []struct {
		user uuid.UUID
		on   bool
	}{{offByChoice, false}, {alreadyOn, true}} {
		if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET suppress_agent_actions = EXCLUDED.suppress_agent_actions`,
			tc.user, tc.on); err != nil {
			t.Fatalf("seed prefs row: %v", err)
		}
	}

	// The control number is taken BEFORE the migration and over the WHOLE
	// table: the migration counter is global, so checking it against one
	// fixture's number would be comparing different quantities.
	var wantRewritten, wantNoRow int64
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM user_notification_prefs WHERE NOT suppress_agent_actions),
       (SELECT count(*) FROM users u
         WHERE NOT EXISTS (SELECT 1 FROM user_notification_prefs p WHERE p.user_id = u.id))`).
		Scan(&wantRewritten, &wantNoRow); err != nil {
		t.Fatalf("preflight counts: %v", err)
	}
	if wantRewritten < 2 {
		t.Fatalf("fixture did not accumulate off rows (%d) — there is nothing to rewrite", wantRewritten)
	}

	if err := m.Migrate(43); err != nil {
		t.Fatalf("Migrate(43): %v", err)
	}

	// 1. Rewriting existing rows, and only the off ones.
	for name, u := range map[string]uuid.UUID{
		"turned off deliberately": offByChoice,
		"turned off by default":   defaulted,
		"already on":              alreadyOn,
	} {
		if !readSuppress(t, pool, u) {
			t.Errorf("%s: suppress_agent_actions = false after the migration, want true", name)
		}
	}

	// 2. New column default — observable, by an insert without the column.
	fresh := seedUser(t, pool, "w43e")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, fresh)
	})
	if got := insertPrefsRowDefaulted(t, pool, fresh); !got {
		t.Error("insert without the column gave false — the column default was not switched")
	}

	// 3. Counters: check against independently taken numbers and the migration print.
	for _, tc := range []struct {
		metric string
		want   int64
	}{
		{"accounts_rewritten", wantRewritten},
		{"accounts_left_off", 0},
		{"accounts_without_prefs_row", wantNoRow},
	} {
		var got int64
		if err := pool.QueryRow(ctx, `
SELECT value FROM migration_counters
 WHERE migration = '000043_suppress_agent_actions_default_on' AND metric = $1
 ORDER BY id DESC LIMIT 1`, tc.metric).Scan(&got); err != nil {
			t.Fatalf("read counter %s: %v", tc.metric, err)
		}
		if got != tc.want {
			t.Errorf("counter %s = %d, want %d (taken by an independent select before the migration)",
				tc.metric, got, tc.want)
		}
	}
	line := findOutputLine(captured(), "000043_suppress_agent_actions_default_on")
	if line == "" {
		t.Fatal("migration 000043 printed nothing: counters must be named by name in the output")
	}
	for _, metric := range []string{"accounts_rewritten", "accounts_left_off", "accounts_without_prefs_row"} {
		if !strings.Contains(line, metric+"=") {
			t.Errorf("migration output does not name counter %s: %q", metric, line)
		}
	}
	if want := "accounts_rewritten=" + strconv.FormatInt(wantRewritten, 10); !strings.Contains(line, want) {
		t.Errorf("migration output = %q, want containing %q", line, want)
	}

	// 4. Rollback: the default is returned, row values are not (declared property).
	if err := m.Migrate(42); err != nil {
		t.Fatalf("Migrate(42) — rollback: %v", err)
	}
	back := seedUser(t, pool, "w43f")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, back)
	})
	if got := insertPrefsRowDefaulted(t, pool, back); got {
		t.Error("after rollback an insert without the column gave true — down did not restore the default")
	}
	if !readSuppress(t, pool, offByChoice) {
		t.Error("rollback restored the row's previous value: it cannot and does not promise that — " +
			"the check is here so the next reader does not take the property for a bug")
	}

	// Re-apply: idempotent. The `back` row is dropped before it — it was
	// created ALREADY AFTER rollback, i.e. with the returned default false,
	// and the re-apply must rewrite it. Leaving it would mean counting the
	// counter over a population that changed between applies, and demanding
	// zero of a quantity that is one by construction.
	if _, err := pool.Exec(ctx,
		`DELETE FROM user_notification_prefs WHERE user_id = $1`, back); err != nil {
		t.Fatalf("drop post-rollback row: %v", err)
	}
	if err := m.Migrate(43); err != nil {
		t.Fatalf("Migrate(43) — re-apply: %v", err)
	}
	var again int64
	if err := pool.QueryRow(ctx, `
SELECT value FROM migration_counters
 WHERE migration = '000043_suppress_agent_actions_default_on' AND metric = 'accounts_rewritten'
 ORDER BY id DESC LIMIT 1`).Scan(&again); err != nil {
		t.Fatalf("read counter after re-up: %v", err)
	}
	if again != 0 {
		t.Errorf("re-apply rewrote %d rows, want 0 — all are already on", again)
	}
	if got := insertPrefsRowDefaulted(t, pool, back); !got {
		t.Errorf("after re-apply the default is again not true (user %s)", back)
	}
}

package migrations

// migration_000059_test.go — sk-622.21, T001, test matrix row 6 (US-1, US-2,
// INV-1 on the schema side, edge 6). Reference — spec sk-622.21 "Data /
// entities / migrations" and plan K3 / K4 / K6, written before the migration:
//
//   - after up no issue_watchers row carries 'unwatched' (tombstones are
//     deleted, not carried over to opt-outs), opt-outs start empty;
//   - unwatch_on_close is true for every existing row and by default;
//   - deleting the issue or the user deletes the opt-out; deleting the issue
//     of a card nulls notifications.card_issue_id and KEEPS the feed row;
//   - rows in the shape the previous code version writes are still accepted
//     (old version over new schema during the R1 rollout);
//   - down reverts the schema; deleted tombstones are NOT restored — the
//     owner's accepted data loss, asserted here as expected behaviour.
//
// The "old version" is exercised by inserting its row shapes, not by running
// an old binary (residual INV-16, tests-rationale No. 6).

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// m59Migrator opens a migrator and registers the return to head before the
// first step down, so the suite is left at head whatever the test does.
func m59Migrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	m, err := migrate.New("file://.", pgxMigrateURL(os.Getenv(testDatabaseURL)))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Errorf("restore Up: %v", err)
		}
		_, _ = m.Close()
	})
	return m
}

func m59Up(t *testing.T, m *migrate.Migrate, stage string) {
	t.Helper()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("%s: Up: %v", stage, err)
	}
}

func m59Exec(t *testing.T, pool *pgxpool.Pool, stage, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
}

func m59Count(t *testing.T, pool *pgxpool.Pool, stage, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	return n
}

// m59SeedFeedRow writes an event and a notification for user in the shape the
// previous code version writes — no card columns named — and returns the
// notification id.
func m59SeedFeedRow(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, issueID string, user uuid.UUID) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type)
VALUES ($1, 'issue', $2, 'issue.updated', 'tester', 'human') RETURNING id`,
		projectID, issueID).Scan(&eventID); err != nil {
		t.Fatalf("seed event for %s: %v", issueID, err)
	}
	var notifID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id) VALUES ($1, $2, 'issue.updated', $3)
RETURNING id`, user, eventID, projectID).Scan(&notifID); err != nil {
		t.Fatalf("seed notification for %s: %v", issueID, err)
	}
	return notifID
}

// TestMigration000059_UpOnSeededDatabase — tombstones, opt-outs and the
// unwatch_on_close default across up on a database that already holds data.
func TestMigration000059_UpOnSeededDatabase(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t)

	if err := m.Migrate(58); err != nil {
		t.Fatalf("Migrate(58) (000059 down): %v", err)
	}
	owner := seedUser(t, pool, "m59o")
	tomb := seedUser(t, pool, "m59t")
	live := seedUser(t, pool, "m59l")
	pid, prefix := seedProject(t, pool, owner, "m59")
	issue := prefix + "-1"
	seedIssue(t, pool, issue, pid, owner, "")

	m59Exec(t, pool, "seed tombstone on 58", `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{unwatched}')`, issue, tomb)
	m59Exec(t, pool, "seed live subscription on 58", `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')`, issue, live)
	m59Exec(t, pool, "seed prefs off on 58", `
INSERT INTO user_notification_prefs (user_id, unwatch_on_close) VALUES ($1, false)`, tomb)

	m59Up(t, m, "000059 up")

	if n := m59Count(t, pool, "tombstones after up", `
SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND 'unwatched' = ANY(reasons)`, issue); n != 0 {
		t.Errorf("after up %d tombstone rows survived, want 0", n)
	}
	if n := m59Count(t, pool, "live after up", `
SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issue, live); n != 1 {
		t.Errorf("after up the live subscription rows = %d, want 1 (only tombstones are deleted)", n)
	}
	if n := m59Count(t, pool, "opt-outs after up", `
SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1`, issue); n != 0 {
		t.Errorf("after up %d opt-outs exist for the seeded issue, want 0 (tombstones are not carried over)", n)
	}
	var on bool
	if err := pool.QueryRow(context.Background(),
		`SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1`, tomb).Scan(&on); err != nil {
		t.Fatalf("read unwatch_on_close: %v", err)
	}
	if !on {
		t.Errorf("an existing row with unwatch_on_close=false reads false after up, want true")
	}
	fresh := seedUser(t, pool, "m59f")
	m59Exec(t, pool, "bare prefs row after up", `INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, fresh)
	if err := pool.QueryRow(context.Background(),
		`SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1`, fresh).Scan(&on); err != nil {
		t.Fatalf("read unwatch_on_close of a new row: %v", err)
	}
	if !on {
		t.Errorf("a new prefs row written without the column reads unwatch_on_close=false, want true")
	}
}

// TestMigration000059_ForeignKeysOfOptOutsAndCards — edge 6: an opt-out goes
// with its issue and with its user; the card goes with its issue while the
// feed row stays.
func TestMigration000059_ForeignKeysOfOptOutsAndCards(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner := seedUser(t, pool, "m59k")
	gone := seedUser(t, pool, "m59g")
	reader := seedUser(t, pool, "m59r")
	pid, prefix := seedProject(t, pool, owner, "m59")
	kept := prefix + "-1"
	doomed := prefix + "-2"
	seedIssue(t, pool, kept, pid, owner, "")
	seedIssue(t, pool, doomed, pid, owner, "")

	m59Exec(t, pool, "opt-outs", `
INSERT INTO issue_watch_optouts (issue_id, user_id) VALUES ($1, $3), ($2, $3), ($1, $4)`,
		kept, doomed, reader, gone)
	notif := m59SeedFeedRow(t, pool, pid, doomed, reader)
	m59Exec(t, pool, "set card", `UPDATE notifications SET card_issue_id = $2, card_resolved = true WHERE id = $1`,
		notif, doomed)

	m59Exec(t, pool, "delete issue", `DELETE FROM issues WHERE id = $1`, doomed)
	if n := m59Count(t, pool, "opt-out of deleted issue", `
SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1`, doomed); n != 0 {
		t.Errorf("opt-outs of a deleted issue = %d, want 0 (cascade)", n)
	}
	var card *string
	if err := pool.QueryRow(context.Background(),
		`SELECT card_issue_id FROM notifications WHERE id = $1`, notif).Scan(&card); err != nil {
		t.Fatalf("the feed row of a deleted card issue is gone (want it kept): %v", err)
	}
	if card != nil {
		t.Errorf("card_issue_id of a deleted issue = %q, want NULL", *card)
	}

	m59Exec(t, pool, "delete user", `DELETE FROM users WHERE id = $1`, gone)
	if n := m59Count(t, pool, "opt-out of deleted user", `
SELECT count(*) FROM issue_watch_optouts WHERE user_id = $1`, gone); n != 0 {
		t.Errorf("opt-outs of a deleted user = %d, want 0 (cascade)", n)
	}
	if n := m59Count(t, pool, "unrelated opt-out", `
SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, kept, reader); n != 1 {
		t.Errorf("an unrelated opt-out disappeared: %d rows, want 1", n)
	}
	m59Exec(t, pool, "cleanup opt-outs", `DELETE FROM issue_watch_optouts WHERE issue_id = $1`, kept)
}

// TestMigration000059_OldVersionRowShapesAccepted — the row shapes the
// previous code version writes are still accepted by the R1 schema, the one
// both versions run on during the R1 rollout. Run at version 61 (the last
// R1 migration): R2's 000062 forbids the tombstone shape by design (INV-1),
// and R2 is rolled out only after the old version is gone.
func TestMigration000059_OldVersionRowShapesAccepted(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	if err := m.Migrate(61); err != nil {
		t.Fatalf("Migrate(61) (the R1 schema): %v", err)
	}
	owner := seedUser(t, pool, "m59v")
	other := seedUser(t, pool, "m59w")
	pid, prefix := seedProject(t, pool, owner, "m59")
	issue := prefix + "-1"
	seedIssue(t, pool, issue, pid, owner, "")

	notif := m59SeedFeedRow(t, pool, pid, issue, owner)
	var card *string
	var resolved bool
	if err := pool.QueryRow(context.Background(),
		`SELECT card_issue_id, card_resolved FROM notifications WHERE id = $1`, notif).Scan(&card, &resolved); err != nil {
		t.Fatalf("read card columns: %v", err)
	}
	if card != nil || resolved {
		t.Errorf("an old-shape notification reads card_issue_id=%v card_resolved=%v, want NULL / false", card, resolved)
	}
	m59Exec(t, pool, "old-shape tombstone", `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{unwatched}')`, issue, other)
	m59Exec(t, pool, "old-shape prefs row", `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions) VALUES ($1, false)`, other)
	m59Exec(t, pool, "cleanup tombstone", `DELETE FROM issue_watchers WHERE issue_id = $1`, issue)
}

// TestMigration000059_DownRevertsSchemaTombstonesStayGone — down removes the
// table, the columns and restores the false default; a tombstone deleted by up
// is not restored (expected, maintainers' decision); up applies again.
func TestMigration000059_DownRevertsSchemaTombstonesStayGone(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t)

	if err := m.Migrate(58); err != nil {
		t.Fatalf("Migrate(58): %v", err)
	}
	owner := seedUser(t, pool, "m59d")
	tomb := seedUser(t, pool, "m59u")
	pid, prefix := seedProject(t, pool, owner, "m59")
	issue := prefix + "-1"
	seedIssue(t, pool, issue, pid, owner, "")
	m59Exec(t, pool, "seed tombstone on 58", `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{unwatched}')`, issue, tomb)

	m59Up(t, m, "up")
	m59Exec(t, pool, "opt-out on head", `INSERT INTO issue_watch_optouts (issue_id, user_id) VALUES ($1, $2)`, issue, tomb)
	m59SeedFeedRow(t, pool, pid, issue, owner)

	if err := m.Migrate(58); err != nil {
		t.Fatalf("Migrate(58) with data: %v", err)
	}
	if tableExists(t, pool, "issue_watch_optouts") {
		t.Errorf("after down issue_watch_optouts still exists")
	}
	for _, col := range []string{"card_issue_id", "card_resolved"} {
		if columnExists(t, pool, "notifications", col) {
			t.Errorf("after down notifications.%s still exists", col)
		}
	}
	fresh := seedUser(t, pool, "m59z")
	m59Exec(t, pool, "bare prefs row on 58", `INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, fresh)
	var on bool
	if err := pool.QueryRow(context.Background(),
		`SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1`, fresh).Scan(&on); err != nil {
		t.Fatalf("read unwatch_on_close on 58: %v", err)
	}
	if on {
		t.Errorf("after down a new prefs row reads unwatch_on_close=true, want the 000053 default false")
	}
	if n := m59Count(t, pool, "tombstone after down", `
SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issue, tomb); n != 0 {
		t.Errorf("after down a deleted tombstone came back (%d rows); down does not restore tombstones", n)
	}

	m59Up(t, m, "re-apply")
	if !tableExists(t, pool, "issue_watch_optouts") || !columnExists(t, pool, "notifications", "card_issue_id") {
		t.Errorf("re-apply did not restore the 000059 schema")
	}
}

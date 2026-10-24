package migrations

// migration_000052_test.go — sk-539 (review of sk-340, quiet hours).
//
// Migration 000052 carries three steps of different nature, and every one of
// them has to round-trip:
//
//  1. users.timezone + users.timezone_set_manually and the name CHECK;
//  2. user_notification_prefs.quiet_hours_start / _end with the pairing and
//     non-degeneracy CHECKs;
//  3. the eighth terminal push outcome `quiet_hours` in the closed set.
//
// The schema is asserted by BEHAVIOUR wherever the database can show it —
// a probe write in a rolled-back transaction — and by the catalog only for
// column presence. A probe says what the database DOES; reading the
// constraint text would only say what it SAYS.
//
// The second test pins the one deliberate property of the down file: with a
// push already finished as `quiet_hours`, the rollback STOPS instead of
// rewriting that row into an outcome that never happened (same decision as
// 000044 for `superseded`).
//
// Modelled on migration_000049..000051 tests: absolute versions (not Steps),
// and the return to head registered BEFORE the first step down, so a failing
// assertion cannot leave the shared database behind for the next package.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// quietFixture is one user with one notification row, the minimum every
// probe of this file needs.
type quietFixture struct {
	user           uuid.UUID
	notificationID int64
}

func seedQuietFixture(t *testing.T, pool *pgxpool.Pool, tag string) quietFixture {
	t.Helper()
	ctx := context.Background()
	user := seedUser(t, pool, tag)
	pid, prefix := seedProject(t, pool, user, tag)

	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type)
VALUES ($1, 'issue', $2, 'issue.created', 'seed', 'human') RETURNING id`,
		pid, prefix+"-1").Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var nid int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id)
VALUES ($1, $2, 'issue.created', $3) RETURNING id`, user, eventID, pid).Scan(&nid); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	// Registered last, so it runs first: the notification and the prefs row
	// must go before the user and the project they point at.
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM notifications WHERE id = $1`, nid)
		_, _ = pool.Exec(bg, `DELETE FROM user_notification_prefs WHERE user_id = $1`, user)
	})
	return quietFixture{user: user, notificationID: nid}
}

// probe runs stmt in a transaction that is always rolled back and reports
// whether the database accepted it. A rejection must come from the named
// constraint; any other failure is a broken probe, not an answer.
func probe(t *testing.T, pool *pgxpool.Pool, constraint, stmt string, args ...any) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, stmt, args...)
	if err == nil {
		return true
	}
	if !strings.Contains(err.Error(), constraint) {
		t.Fatalf("probe %q failed for a reason other than %s: %v", stmt, constraint, err)
	}
	return false
}

// pushOutcomeAccepted reports whether the closed set of push outcomes lets
// the value in, by finishing the fixture's row with it.
func pushOutcomeAccepted(t *testing.T, pool *pgxpool.Pool, f quietFixture, outcome string) bool {
	t.Helper()
	return probe(t, pool, "notifications_push_outcome_chk", `
UPDATE notifications SET push_state = 'final', push_outcome = $2 WHERE id = $1`,
		f.notificationID, outcome)
}

// quietWindowAccepted reports whether a prefs row with these boundaries is
// accepted (nil = SQL NULL).
func quietWindowAccepted(t *testing.T, pool *pgxpool.Pool, f quietFixture, constraint string, start, end any) bool {
	t.Helper()
	return probe(t, pool, constraint, `
INSERT INTO user_notification_prefs (
    user_id, default_inbox_scope, suppress_agent_actions,
    quiet_hours_start, quiet_hours_end, updated_at)
VALUES ($1, 'all', true, $2::time, $3::time, now())`, f.user, start, end)
}

var quietColumns = []struct{ table, column string }{
	{"users", "timezone"},
	{"users", "timezone_set_manually"},
	{"user_notification_prefs", "quiet_hours_start"},
	{"user_notification_prefs", "quiet_hours_end"},
}

// assertQuietUpState — everything 000052 introduces is in place and behaves
// as the migration header promises.
func assertQuietUpState(t *testing.T, pool *pgxpool.Pool, f quietFixture, stage string) {
	t.Helper()
	for _, c := range quietColumns {
		if !columnExists(t, pool, c.table, c.column) {
			t.Fatalf("%s: %s.%s is missing", stage, c.table, c.column)
		}
	}

	// 1. Time zone: NULL by default ("unknown", not UTC), not hand-set by
	//    default, and a padded name is refused.
	var tz *string
	var manual bool
	if err := pool.QueryRow(context.Background(),
		`SELECT timezone, timezone_set_manually FROM users WHERE id = $1`, f.user).
		Scan(&tz, &manual); err != nil {
		t.Fatalf("%s: read timezone: %v", stage, err)
	}
	if tz != nil || manual {
		t.Errorf("%s: fresh user has timezone=%v manual=%v, want NULL/false", stage, tz, manual)
	}
	if !probe(t, pool, "users_timezone_chk",
		`UPDATE users SET timezone = 'Europe/Moscow' WHERE id = $1`, f.user) {
		t.Errorf("%s: a well-formed zone name is refused — the probe itself is broken", stage)
	}
	if probe(t, pool, "users_timezone_chk",
		`UPDATE users SET timezone = ' Europe/Moscow' WHERE id = $1`, f.user) {
		t.Errorf("%s: a zone name with surrounding blanks is accepted", stage)
	}
	if probe(t, pool, "users_timezone_chk",
		`UPDATE users SET timezone = '' WHERE id = $1`, f.user) {
		t.Errorf("%s: an empty zone name is accepted", stage)
	}

	// 2. Quiet window: a midnight-crossing window is a normal value; half a
	//    window and a degenerate one are refused.
	if !quietWindowAccepted(t, pool, f, "user_prefs_quiet_hours", "22:00", "07:00") {
		t.Errorf("%s: the window 22:00–07:00 is refused", stage)
	}
	if quietWindowAccepted(t, pool, f, "user_prefs_quiet_hours_pair_chk", "22:00", nil) {
		t.Errorf("%s: a window with only a start is accepted", stage)
	}
	if quietWindowAccepted(t, pool, f, "user_prefs_quiet_hours_nondegenerate_chk", "22:00", "22:00") {
		t.Errorf("%s: a window with start = end is accepted", stage)
	}

	// 3. The outcome is in the closed set; the probe control is an outcome
	//    that predates this migration.
	if !pushOutcomeAccepted(t, pool, f, "superseded") {
		t.Errorf("%s: control outcome superseded is refused — the probe itself is broken", stage)
	}
	if !pushOutcomeAccepted(t, pool, f, "quiet_hours") {
		t.Errorf("%s: push outcome quiet_hours is refused", stage)
	}
}

// TestMigration000052_UpDownUpRoundTrip — up/down/up, each state asserted.
func TestMigration000052_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	f := seedQuietFixture(t, pool, "m52")

	assertQuietUpState(t, pool, f, "before rollback")

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

	// Absolute version, not Steps(-1): a later migration would turn a step
	// back into a rollback of somebody else's migration.
	if err := m.Migrate(51); err != nil {
		t.Fatalf("Migrate(51) (000052 down): %v", err)
	}
	for _, c := range quietColumns {
		if columnExists(t, pool, c.table, c.column) {
			t.Errorf("after rollback %s.%s remained", c.table, c.column)
		}
	}
	if !pushOutcomeAccepted(t, pool, f, "superseded") {
		t.Error("after rollback control outcome superseded is refused — the rollback dropped the whole set")
	}
	if pushOutcomeAccepted(t, pool, f, "quiet_hours") {
		t.Error("after rollback push outcome quiet_hours is still accepted")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	assertQuietUpState(t, pool, f, "after re-apply")
}

// TestMigration000052_DownStopsOnAQuietHoursRow — the down file's own
// promise: a push already finished as `quiet_hours` makes the rollback fail
// as a whole, leaving the row and the schema as they were, rather than
// quietly rewriting the row into an outcome that never happened.
func TestMigration000052_DownStopsOnAQuietHoursRow(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()
	pool := testPool(t)
	f := seedQuietFixture(t, pool, "m52d")

	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_state = 'final', push_outcome = 'quiet_hours' WHERE id = $1`,
		f.notificationID); err != nil {
		t.Fatalf("finish the row as quiet_hours: %v", err)
	}

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Restoring the shared database depends on how the rollback ended, and
	// the defer is registered before the attempt so a failing assertion
	// cannot strand it either way:
	//   - the down file failed (the expected case): golang-migrate marked the
	//     TARGET version dirty before running it, so "51, dirty" is recorded
	//     although the file's single implicit transaction rolled back and the
	//     schema is still at 52. Forcing 52 is the operator's recovery step;
	//   - the down file committed (the defect this test catches): the
	//     database is cleanly at 51 on the down schema. Forcing 52 there
	//     would record 52 without applying 000052 and turn the Up below into
	//     ErrNoChange, so only Up runs and re-applies 000052.
	var downErr error
	defer func() {
		if downErr != nil {
			if err := m.Force(52); err != nil {
				t.Fatalf("Force(52): %v", err)
			}
		}
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	if downErr = m.Migrate(51); downErr == nil {
		t.Fatal("rollback of 000052 succeeded with a quiet_hours row present — " +
			"the row was dropped from the closed set or rewritten")
	}

	var outcome *string
	if err := pool.QueryRow(ctx,
		`SELECT push_outcome FROM notifications WHERE id = $1`, f.notificationID).Scan(&outcome); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcome == nil || *outcome != "quiet_hours" {
		t.Errorf("row outcome after the refused rollback = %v, want quiet_hours", outcome)
	}
	// The failed file must not have half-run. Its FIRST statement drops the
	// closed set of outcomes and its second (the one that fails) re-adds it,
	// so a statement-at-a-time run would leave the table with no outcome
	// CHECK at all: an unknown outcome being accepted is that half-run.
	if pushOutcomeAccepted(t, pool, f, "no_such_outcome") {
		t.Error("after the refused rollback an unknown push outcome is accepted — " +
			"the down file half-ran and dropped the closed set")
	}
	for _, c := range quietColumns {
		if !columnExists(t, pool, c.table, c.column) {
			t.Errorf("after the refused rollback %s.%s is gone — the down file half-ran", c.table, c.column)
		}
	}
}

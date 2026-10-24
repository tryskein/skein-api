package migrations

// migration_000044_test.go — integration class for migration 000044
// (sk-303.14, T004). Owns coverage-matrix rows 7–14:
//
//	7   forward and reverse migration run is clean (up/down/up)
//	8   issues created before release do not enter the set (SC-015)
//	9   CHECK bounds reject forbidden schedule values (SC-027)
//	10  schedule base is stamped when creating an issue WITH an assignee immediately (SC-019)
//	11  schedule base is stamped on assignee change by patch and by claim
//	12  negative: a title-only patch does not move the schedule base
//	13  negative: a claim without an assignee change does not move the base
//	14  negative: the same assignee name in a different case is not a change
//
// Row 15 (migration counters) lives in migration_counters_test.go — the
// independent check of the whole wave already sits there, and a second
// instance here would mean two sources of truth.
//
// WHY THE REAL STORE, NOT A COPY OF ITS SQL. The trigger requirement is
// "the stamp is set only on an ACTUAL assignee change", and the danger is
// that real write paths always name the assignee column in the assignment
// (`assignee = COALESCE($n, assignee)`). A test that wrote this SQL by
// hand would check a copy that drifts separately from the original. So
// rows 10–14 drive issues.Store — CreateIssue, PatchIssue and Claim —
// i.e. exactly the three paths that exist today.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/review"
)

const migration000044 = "000044_review_and_nudges"

// ---------------------------------------------------------------------------
// Schema probes
// ---------------------------------------------------------------------------

func columnExists(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
     WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&ok); err != nil {
		t.Fatalf("probe column %s.%s: %v", table, column, err)
	}
	return ok
}

func tableExists(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&ok); err != nil {
		t.Fatalf("probe table %s: %v", table, err)
	}
	return ok
}

func triggerExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = $1 AND NOT tgisinternal)`,
		name).Scan(&ok); err != nil {
		t.Fatalf("probe trigger %s: %v", name, err)
	}
	return ok
}

func functionExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = $1)`, name).Scan(&ok); err != nil {
		t.Fatalf("probe function %s: %v", name, err)
	}
	return ok
}

func constraintDef(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`,
		name).Scan(&def); err != nil {
		t.Fatalf("read constraint %s: %v", name, err)
	}
	return def
}

// ---------------------------------------------------------------------------
// Row 7 — forward and reverse run is clean
// ---------------------------------------------------------------------------

func TestMigration000044_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)

	// State AT HEAD: everything the migration introduces is in place.
	assertUpState := func(stage string) {
		t.Helper()
		for _, col := range []string{"review_required", "reviewed_at", "reviewed_by", "assignee_set_at"} {
			if !columnExists(t, pool, "issues", col) {
				t.Errorf("%s: issues.%s is missing", stage, col)
			}
		}
		for _, col := range []string{
			"review_mechanic_enabled", "review_required_default",
			"reminder_burst_count", "unaccepted_interval_minutes", "unreviewed_interval_minutes",
		} {
			if !columnExists(t, pool, "user_notification_prefs", col) {
				t.Errorf("%s: user_notification_prefs.%s is missing", stage, col)
			}
		}
		if !tableExists(t, pool, "reminder_rate_limit") {
			t.Errorf("%s: table reminder_rate_limit is missing", stage)
		}
		for _, col := range []string{"last_formed_at", "next_due_at"} {
			if !columnExists(t, pool, "reminder_rate_limit", col) {
				t.Errorf("%s: reminder_rate_limit.%s is missing", stage, col)
			}
		}
		if !triggerExists(t, pool, "issues_stamp_assignee_set_at") {
			t.Errorf("%s: trigger issues_stamp_assignee_set_at is missing", stage)
		}
		if !functionExists(t, pool, "skein_stamp_assignee_set_at") {
			t.Errorf("%s: function skein_stamp_assignee_set_at is missing", stage)
		}
		if def := constraintDef(t, pool, "notifications_push_outcome_chk"); !strings.Contains(def, "superseded") {
			t.Errorf("%s: supersession outcome is not in the closed set: %s", stage, def)
		}
		if c := readMigrationCounters(t, pool, migration000044); len(c) != 3 {
			t.Errorf("%s: migration counters %d, want 3: %v", stage, len(c), c)
		}
	}

	assertUpState("before rollback")

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Return to head is registered BEFORE the down step: otherwise a
	// failing assertion below would leave the shared database at version
	// 43 and break fixtures of every later package in a sequential run.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// Absolute version, not Steps(-1): the arrival of 000045 would make
	// a step-back roll back SOMEONE ELSE'S migration.
	if err := m.Migrate(43); err != nil {
		t.Fatalf("Migrate(43) (000044 down): %v", err)
	}

	for _, col := range []string{"review_required", "reviewed_at", "reviewed_by", "assignee_set_at"} {
		if columnExists(t, pool, "issues", col) {
			t.Errorf("after rollback issues.%s remained", col)
		}
	}
	for _, col := range []string{
		"review_mechanic_enabled", "review_required_default",
		"reminder_burst_count", "unaccepted_interval_minutes", "unreviewed_interval_minutes",
	} {
		if columnExists(t, pool, "user_notification_prefs", col) {
			t.Errorf("after rollback user_notification_prefs.%s remained", col)
		}
	}
	if tableExists(t, pool, "reminder_rate_limit") {
		t.Error("after rollback table reminder_rate_limit remained")
	}
	if triggerExists(t, pool, "issues_stamp_assignee_set_at") {
		t.Error("after rollback trigger issues_stamp_assignee_set_at remained")
	}
	if functionExists(t, pool, "skein_stamp_assignee_set_at") {
		t.Error("after rollback function skein_stamp_assignee_set_at remained")
	}
	if def := constraintDef(t, pool, "notifications_push_outcome_chk"); strings.Contains(def, "superseded") {
		t.Errorf("after rollback the supersession outcome remained in the set: %s", def)
	}
	if c := readMigrationCounters(t, pool, migration000044); len(c) != 0 {
		t.Errorf("after rollback migration counters remained: %v", c)
	}

	// Re-apply: the chain is intact and the head state is reproduced.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	assertUpState("after re-apply")
}

// ---------------------------------------------------------------------------
// Row 8 — issues created before release do not enter the set (SC-015)
// ---------------------------------------------------------------------------

func TestMigration000044_PreReleaseIssuesStayOutOfTheSet(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool, "m44a")
	pid, prefix := seedProject(t, pool, owner, "m44a")

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	// Rollback to 43 = the world BEFORE the review workflow release. Issues seeded
	// here ARE the "historical archive" SC-015 exists for.
	if err := m.Migrate(43); err != nil {
		t.Fatalf("Migrate(43): %v", err)
	}

	var ownerName string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerName); err != nil {
		t.Fatalf("read username: %v", err)
	}
	historical := prefix + "-hist"
	historicalAssigned := prefix + "-histasg"
	seedIssue(t, pool, historical, pid, owner, "")
	seedIssue(t, pool, historicalAssigned, pid, owner, ownerName)
	if _, err := pool.Exec(ctx, `
UPDATE issues SET status = 'closed', closed_at = now() WHERE id = $1`, historical); err != nil {
		t.Fatalf("close historical issue: %v", err)
	}

	// Release.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up: %v", err)
	}

	// No historical issue carries the flag — otherwise on release day
	// the whole archive of closed delegated issues would flood counters
	// and reminders at once.
	var flagged int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM issues WHERE id = ANY($1) AND review_required`,
		[]string{historical, historicalAssigned}).Scan(&flagged); err != nil {
		t.Fatalf("count flagged: %v", err)
	}
	if flagged != 0 {
		t.Errorf("%d historical issues got the \"review required\" flag (SC-015)", flagged)
	}

	// Broader: after release there is not a single issue in the whole
	// database with the flag on. The assertion is stronger than its two
	// fixtures and catches any backfill that accidentally turned the flag
	// on for someone else.
	var flaggedAnywhere int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issues WHERE review_required`).
		Scan(&flaggedAnywhere); err != nil {
		t.Fatalf("count flagged anywhere: %v", err)
	}
	if flaggedAnywhere != 0 {
		t.Errorf("after release %d issues carry the \"review required\" flag", flaggedAnywhere)
	}

	// And they have no review mark: the flag is off, not "reviewed".
	var reviewed int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM issues WHERE id = ANY($1) AND (reviewed_at IS NOT NULL OR reviewed_by IS NOT NULL)`,
		[]string{historical, historicalAssigned}).Scan(&reviewed); err != nil {
		t.Fatalf("count reviewed: %v", err)
	}
	if reviewed != 0 {
		t.Errorf("the migration stamped a review mark on %d historical issues", reviewed)
	}

	// The schedule base of a historical ASSIGNED issue stays empty:
	// the assignment moment was not observed, and substituting created_at
	// or deploy time would record a fabricated fact from which the
	// schedule would compute real marks.
	var hasBase bool
	if err := pool.QueryRow(ctx, `
SELECT assignee_set_at IS NOT NULL FROM issues WHERE id = $1`, historicalAssigned).Scan(&hasBase); err != nil {
		t.Fatalf("read base: %v", err)
	}
	if hasBase {
		t.Error("the migration invented a schedule base for an issue assigned before release")
	}
}

// ---------------------------------------------------------------------------
// Row 9 — CHECK bounds reject forbidden values (SC-027)
// ---------------------------------------------------------------------------

func TestMigration000044_ScheduleBoundsRejectForbiddenValues(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	user := seedUser(t, pool, "m44b")
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, user); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}

	// Factory state of the row: the review workflow is on, the flag default is
	// "required" (US-3), the schedule is empty (factory is taken from the
	// contract, not the schema — otherwise the values would exist in two
	// places).
	var mechanic, defaultRequired bool
	var burst, unacc, unrev *int
	if err := pool.QueryRow(ctx, `
SELECT review_mechanic_enabled, review_required_default,
       reminder_burst_count, unaccepted_interval_minutes, unreviewed_interval_minutes
  FROM user_notification_prefs WHERE user_id = $1`, user).
		Scan(&mechanic, &defaultRequired, &burst, &unacc, &unrev); err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	if !mechanic {
		t.Error("the mechanic is off by default — turning it off must be a human action (INV-6)")
	}
	if !defaultRequired {
		t.Error("personal flag default is off; the spec names factory \"required\" (US-3)")
	}
	if burst != nil || unacc != nil || unrev != nil {
		t.Errorf("schedule is not empty on a fresh row: %v %v %v", burst, unacc, unrev)
	}

	// Bounds are taken from the schedule contract, not rewritten here:
	// the schema carries them as a second instance, and the instances
	// must not diverge.
	set := func(column string, value int) error {
		_, err := pool.Exec(ctx,
			`UPDATE user_notification_prefs SET `+column+` = $2 WHERE user_id = $1`, user, value)
		return err
	}
	clearAll := func() {
		if _, err := pool.Exec(ctx, `
UPDATE user_notification_prefs
   SET reminder_burst_count = NULL,
       unaccepted_interval_minutes = NULL,
       unreviewed_interval_minutes = NULL
 WHERE user_id = $1`, user); err != nil {
			t.Fatalf("clear prefs: %v", err)
		}
	}

	rejected := []struct {
		column string
		value  int
	}{
		{"reminder_burst_count", review.MaxBurstCount + 1},
		{"reminder_burst_count", review.MinBurstCount - 1},
		{"unaccepted_interval_minutes", review.MinIntervalMinutes - 1},
		{"unaccepted_interval_minutes", 0},
		{"unaccepted_interval_minutes", review.MaxIntervalMinutes + 1},
		{"unreviewed_interval_minutes", review.MinIntervalMinutes - 1},
		{"unreviewed_interval_minutes", -1},
		{"unreviewed_interval_minutes", review.MaxIntervalMinutes + 1},
	}
	for _, tc := range rejected {
		if err := set(tc.column, tc.value); err == nil {
			t.Errorf("%s = %d accepted by the schema; a value outside bounds must be rejected, not rewritten (SC-022, SC-027)",
				tc.column, tc.value)
			clearAll()
		}
	}

	accepted := []struct {
		column string
		value  int
	}{
		{"reminder_burst_count", review.MinBurstCount},
		{"reminder_burst_count", review.MaxBurstCount},
		{"reminder_burst_count", review.DefaultBurstCount},
		{"unaccepted_interval_minutes", review.MinIntervalMinutes},
		{"unaccepted_interval_minutes", review.MaxIntervalMinutes},
		{"unaccepted_interval_minutes", review.DefaultUnacceptedIntervalMinutes},
		{"unreviewed_interval_minutes", review.MinIntervalMinutes},
		{"unreviewed_interval_minutes", review.MaxIntervalMinutes},
		{"unreviewed_interval_minutes", review.DefaultUnreviewedIntervalMinutes},
	}
	for _, tc := range accepted {
		if err := set(tc.column, tc.value); err != nil {
			t.Errorf("%s = %d rejected by the schema, though the schedule contract allows it: %v",
				tc.column, tc.value, err)
		}
	}

	// The frozen next-reminder due time is REQUIRED and has no schema
	// default: it must be computed from the prefs that were in force at
	// window open, and now() would be a fabricated due time, tied to no
	// prefs. A row without it is a row on which the rule "a prefs change
	// takes effect from the next window" cannot be fulfilled.
	if _, err := pool.Exec(ctx, `
INSERT INTO reminder_rate_limit (user_id, category, last_formed_at)
VALUES ($1, 'unaccepted', now())`, user); err == nil {
		t.Error("the schema accepted a window state without a frozen next-reminder due time")
		if _, err := pool.Exec(ctx, `DELETE FROM reminder_rate_limit WHERE user_id = $1`, user); err != nil {
			t.Fatalf("cleanup rate limit: %v", err)
		}
	}

	// The rate-limit category set is closed and matches the contract.
	for _, cat := range review.Categories() {
		if _, err := pool.Exec(ctx, `
INSERT INTO reminder_rate_limit (user_id, category, last_formed_at, next_due_at)
VALUES ($1, $2, now(), now() + interval '3 hours')
ON CONFLICT (user_id, category) DO UPDATE
   SET last_formed_at = EXCLUDED.last_formed_at,
       next_due_at    = EXCLUDED.next_due_at`,
			user, string(cat)); err != nil {
			t.Errorf("category %q rejected by the schema: %v", cat, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reminder_rate_limit (user_id, category, last_formed_at, next_due_at)
VALUES ($1, 'digest', now(), now() + interval '3 hours')`,
		user); err == nil {
		t.Error("the schema accepted a category outside the closed set")
	}

	// The same due time cannot be nulled on a live row either: the rule
	// rests on its presence, not on its presence only at insert time.
	if _, err := pool.Exec(ctx, `
UPDATE reminder_rate_limit SET next_due_at = NULL WHERE user_id = $1`, user); err == nil {
		t.Error("the schema allowed nulling the frozen due time on an existing row")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM reminder_rate_limit WHERE user_id = $1`, user)
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, user)
	})
}

// ---------------------------------------------------------------------------
// Rows 10–14 — schedule base on REAL write paths
// ---------------------------------------------------------------------------

// storeFixture — a project ready to accept issues.Store writes: a member
// and an id-counter row that a bare seedProject does not create.
func storeFixture(t *testing.T, pool *pgxpool.Pool, tag string) (uuid.UUID, string, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()

	owner := seedUser(t, pool, tag)
	var ownerName string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerName); err != nil {
		t.Fatalf("read username: %v", err)
	}
	pid, prefix := seedProject(t, pool, owner, tag)

	// Registered AFTER seedProject, so it runs BEFORE its cleanup
	// (t.Cleanup is a stack): members and the counter go before the project.
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM project_counters WHERE project_id = $1`, pid)
		_, _ = pool.Exec(bg, `DELETE FROM project_members WHERE project_id = $1`, pid)
	})

	if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)`, pid, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter) VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, pid); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	return owner, ownerName, pid, prefix
}

func readBase(t *testing.T, pool *pgxpool.Pool, id string) *time.Time {
	t.Helper()
	var base *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee_set_at FROM issues WHERE id = $1`, id).Scan(&base); err != nil {
		t.Fatalf("read assignee_set_at of %s: %v", id, err)
	}
	return base
}

// dbNow — a moment on the DATABASE clock. The schedule-base stamp is set
// by the trigger via now(), so the window the stamp must fall into is
// taken from the same clock: the test-process clock and Postgres are
// different sources, and comparing the stamp to time.Now() would mix
// their divergence into the assertion.
func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&ts); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return ts
}

// Row 10 — the schedule base is stamped on CREATE of an issue with an
// assignee immediately. This is the main reminder-scenario path, and a
// trigger only on update would not see it (SC-019).
func TestMigration000044_BaseStampedOnCreateWithAssignee(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner, ownerName, pid, prefix := storeFixture(t, pool, "m44c")
	store := issues.NewStore(pool)

	before := time.Now().UTC().Add(-time.Second)

	assignee := ownerName
	withAssignee, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		issues.CreateIssueInput{Title: "created with assignee", Type: "task", Priority: 2, Assignee: &assignee},
		owner, issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue with assignee: %v", err)
	}
	base := readBase(t, pool, withAssignee.ID)
	if base == nil {
		t.Fatal("create with an assignee immediately did not stamp the schedule base (SC-019)")
	}
	if base.Before(before) {
		t.Errorf("schedule base %v is before create time %v", *base, before)
	}

	// And this is NOT issue creation time in general, but specifically the
	// assignment moment: an issue without an assignee has no base at all.
	withoutAssignee, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		issues.CreateIssueInput{Title: "created without assignee", Type: "task", Priority: 2},
		owner, issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue without assignee: %v", err)
	}
	if b := readBase(t, pool, withoutAssignee.ID); b != nil {
		t.Errorf("issue without an assignee got schedule base %v — the base was stamped on create, not assignment", *b)
	}
}

// Row 11 — the schedule base is stamped on assignee change by PATCH and
// by CLAIM: the two remaining real write paths.
func TestMigration000044_BaseStampedOnAssigneeChange(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner, ownerName, pid, prefix := storeFixture(t, pool, "m44d")
	other := seedUser(t, pool, "m44e")
	var otherName string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherName); err != nil {
		t.Fatalf("read other username: %v", err)
	}
	store := issues.NewStore(pool)

	// --- "patch" path ---
	byPatch, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		issues.CreateIssueInput{Title: "reassigned by patch", Type: "task", Priority: 2},
		owner, issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if b := readBase(t, pool, byPatch.ID); b != nil {
		t.Fatalf("precondition: an issue without an assignee must have no base, got %v", *b)
	}

	firstAssign := otherName
	patched, err := store.PatchIssue(t.Context(), byPatch.ID,
		issues.PatchIssueInput{ExpectedVersion: byPatch.Version, Assignee: &firstAssign},
		issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("PatchIssue (first assign): %v", err)
	}
	firstBase := readBase(t, pool, byPatch.ID)
	if firstBase == nil {
		t.Fatal("first assign by patch did not stamp the schedule base")
	}

	// Reassignment — a new base: the scale restarts for the new person,
	// it does not continue someone else's (R-5).
	//
	// THE PRECONDITION IS MADE DETERMINISTIC, not waited for. Comparing
	// two consecutive stamps for "strictly later" is not allowed: both
	// are set by now(), and two independent calls are entitled to
	// indistinguishable values at the environment clock precision — a
	// correct assignee change would give a false failure, and what the
	// test is trying to prove is a re-stamp, not elapsed physical time.
	// So the base is shifted into a known past: replacing this marker IS
	// the fact under test. (A write of this column alone does not change
	// the assignee, so the trigger does not touch it — rows 12–14 assert
	// exactly that.)
	sentinel := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET assignee_set_at = $2 WHERE id = $1`, byPatch.ID, sentinel); err != nil {
		t.Fatalf("seed sentinel base: %v", err)
	}
	if seeded := readBase(t, pool, byPatch.ID); seeded == nil || !seeded.Equal(sentinel) {
		t.Fatalf("precondition: the marker did not land in the column, got %v", seeded)
	}

	secondAssign := ownerName
	beforeReassign := dbNow(t, pool)
	if _, err := store.PatchIssue(t.Context(), byPatch.ID,
		issues.PatchIssueInput{ExpectedVersion: patched.Version, Assignee: &secondAssign},
		issues.HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("PatchIssue (reassign): %v", err)
	}
	afterReassign := dbNow(t, pool)

	secondBase := readBase(t, pool, byPatch.ID)
	if secondBase == nil {
		t.Fatal("reassign by patch nulled the schedule base")
	}
	if secondBase.Equal(sentinel) {
		t.Fatalf("reassign did not stamp the base again: marker %v stayed in place", sentinel)
	}
	if secondBase.Before(beforeReassign) || secondBase.After(afterReassign) {
		t.Errorf("schedule base %v is outside the reassign interval [%v, %v]",
			*secondBase, beforeReassign, afterReassign)
	}

	// --- "claim" path ---
	// An issue without an assignee: claim assigns it to the claimant, and
	// that is an actual assignee change.
	byClaim, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		issues.CreateIssueInput{Title: "assigned by claim", Type: "task", Priority: 2},
		owner, issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if b := readBase(t, pool, byClaim.ID); b != nil {
		t.Fatalf("precondition: there must be no base, got %v", *b)
	}
	if _, err := store.Claim(t.Context(), issues.ClaimInput{
		ID: byClaim.ID, SessionID: "sess-m44-claim", Actor: issues.HumanActor(other, otherName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if b := readBase(t, pool, byClaim.ID); b == nil {
		t.Error("claim assigned an assignee but did not stamp the schedule base")
	}
}

// Rows 12–14 — the NEGATIVE class. Existing write paths always name the
// assignee column in the assignment, so a trigger "on mentioning the
// column" would pass the positive rows above and fail these: the burst
// would restart on a flat surface — on every title patch and every claim.
func TestMigration000044_BaseUnmovedByNonAssignmentWrites(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner, ownerName, pid, prefix := storeFixture(t, pool, "m44f")
	store := issues.NewStore(pool)

	assignee := ownerName
	issue, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		issues.CreateIssueInput{Title: "stable base", Type: "task", Priority: 2, Assignee: &assignee},
		owner, issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	original := readBase(t, pool, issue.ID)
	if original == nil {
		t.Fatal("precondition: schedule base was not stamped on create")
	}
	version := issue.Version

	// Row 12: a patch of ONE title.
	newTitle := "renamed, nothing else"
	patched, err := store.PatchIssue(t.Context(), issue.ID,
		issues.PatchIssueInput{ExpectedVersion: version, Title: &newTitle},
		issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("PatchIssue (title only): %v", err)
	}
	version = patched.Version
	if b := readBase(t, pool, issue.ID); b == nil || !b.Equal(*original) {
		t.Errorf("title patch moved the schedule base: was %v, became %v", *original, b)
	}

	// Row 14: the same assignee name in a DIFFERENT CASE. The domain
	// compares assignees case-insensitively, so this is not a change.
	// The check sits before claim on purpose: it leaves a different-case
	// string in the column, and the next step also proves that a claim
	// on such a row does not move the base either.
	upper := strings.ToUpper(ownerName)
	if upper == ownerName {
		t.Fatalf("fixture is unfit: name %q has no second case", ownerName)
	}
	if _, err = store.PatchIssue(t.Context(), issue.ID,
		issues.PatchIssueInput{ExpectedVersion: version, Assignee: &upper},
		issues.HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("PatchIssue (case-only assignee): %v", err)
	}
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee FROM issues WHERE id = $1`, issue.ID).Scan(&stored); err != nil {
		t.Fatalf("read assignee: %v", err)
	}
	if stored != upper {
		t.Fatalf("precondition is unfit: column has %q, want %q — the case patch did not reach the row, and the trigger is not checked",
			stored, upper)
	}
	if b := readBase(t, pool, issue.ID); b == nil || !b.Equal(*original) {
		t.Errorf("a case-only name change moved the schedule base: was %v, became %v", *original, b)
	}

	// Row 13: a claim WITHOUT an assignee change — the issue is already
	// on the claimant.
	if _, err := store.Claim(t.Context(), issues.ClaimInput{
		ID: issue.ID, SessionID: "sess-m44-noop", Actor: issues.HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("Claim (no assignee change): %v", err)
	}
	if b := readBase(t, pool, issue.ID); b == nil || !b.Equal(*original) {
		t.Errorf("claim without an assignee change moved the schedule base: was %v, became %v", *original, b)
	}
}

// ---------------------------------------------------------------------------
// Source-level invariant of the rollback (needs no database)
// ---------------------------------------------------------------------------

// outcomeLiteral extracts the outcome values named inside an ADD CONSTRAINT.
var outcomeLiteral = regexp.MustCompile(`'([a-z_]+)'`)

// TestMigration000044_RollbackRefusesInsteadOfRewritingOutcomes pins the
// construction of the rollback, not its effect on a live database.
//
// The up migration adds a seventh value — 'superseded' — to the closed set of
// delivery outcomes. The rollback restores the previous set WITHOUT it, so
// `ADD CONSTRAINT` cannot validate while a single row still carries the new
// value, and the rollback fails. That failure is the point: rewriting such
// rows into some other outcome would write into the database a fact that did
// not happen, and supersession is distinguishable from a configuration
// refusal precisely because the value exists. The rollback must stop and
// demand a human decision rather than choose one.
//
// The neighbouring up/down/up round trip observes the restored constraint on
// a real database, but it only runs with TEST_DATABASE_URL set and its
// fixture carries no superseded rows. This check reads the migration files
// themselves, so it holds in the unit pass as well — and it is the only place
// that forbids the rewrite outright.
func TestMigration000044_RollbackRefusesInsteadOfRewritingOutcomes(t *testing.T) {
	t.Parallel()

	up, err := os.ReadFile(migration000044 + ".up.sql")
	if err != nil {
		t.Fatalf("reading %s up: %v", migration000044, err)
	}
	down, err := os.ReadFile(migration000044 + ".down.sql")
	if err != nil {
		t.Fatalf("reading %s down: %v", migration000044, err)
	}

	if !strings.Contains(string(up), "'superseded'") {
		t.Fatal("the up migration no longer adds outcome 'superseded' — " +
			"the subject of this invariant has vanished and the check has become tautological")
	}

	addIdx := strings.Index(string(down), "ADD CONSTRAINT")
	if addIdx < 0 {
		t.Fatal("the rollback has no ADD CONSTRAINT — the parser is broken, not the migration")
	}
	restoredStmt := string(down)[addIdx:]
	if end := strings.Index(restoredStmt, ";"); end > 0 {
		restoredStmt = restoredStmt[:end]
	}
	restored := map[string]bool{}
	for _, m := range outcomeLiteral.FindAllStringSubmatch(restoredStmt, -1) {
		restored[m[1]] = true
	}
	if len(restored) == 0 {
		t.Fatalf("no outcome value found in the restored constraint: %q — the parser is broken", restoredStmt)
	}
	if restored["superseded"] {
		t.Error("the rollback restores the set TOGETHER with 'superseded' — then it no longer refuses, " +
			"and a rollback run against rows that recorded a supersession would silently succeed")
	}
	if strings.Contains(string(down), "UPDATE notifications") {
		t.Error("the rollback rewrites notifications rows — forbidden: rewriting a superseded row " +
			"into another outcome writes into the database a fact that did not happen")
	}
}

// Package migrations holds the SQL files applied by golang-migrate at
// startup. There's no Go code in normal builds — this file lives in
// `_test.go` so it only compiles under `go test`, where we exercise
// the more interesting migrations (currently just sk-35's 000022) by
// rewinding the test DB and replaying them against synthetic
// fixtures.
package migrations

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"

	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

// testPool returns a pool against TEST_DATABASE_URL, skipping if unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run migration tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgxMigrateURL rewrites a postgres:// URL to the pgx5:// scheme the
// golang-migrate driver expects, preserving the host / credentials /
// query params verbatim.
func pgxMigrateURL(in string) string {
	if strings.HasPrefix(in, "postgres://") {
		return "pgx5://" + strings.TrimPrefix(in, "postgres://")
	}
	if strings.HasPrefix(in, "postgresql://") {
		return "pgx5://" + strings.TrimPrefix(in, "postgresql://")
	}
	return in
}

// TestMigration000022_RestoreParentChildDep verifies the migration's
// backfill semantics: every issue with a non-NULL parent_id gets a
// matching parent-child dep edge with the audit-trail metadata note,
// pre-existing 'related' edges are untouched, and child_counters
// reflects the highest existing suffix.
//
// We don't run the migration directly here (it has already been
// applied to the test DB at fixture time, every time). Instead we
// seed a fresh fixture, manually run the same logic the migration
// runs in step 2 and step 3 against THIS fixture, and assert the
// outcome — that's what the production migration does for the
// schema-wide state on first apply.
func TestMigration000022_RestoreParentChildDep(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Fixture: a user, a project, and four issues laid out as
	//   <prefix>-42        (epic)
	//   <prefix>-42.1      (child of 42, parent_id = 42)
	//   <prefix>-42.1.3    (grandchild of 42 via 42.1)
	//   <prefix>-99        (separate top-level, parent_id stays NULL)
	uid := uuid.Must(uuid.NewRandom())
	uname := "mig" + strings.ReplaceAll(uid.String()[:18], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', now(), '{}'::jsonb)`,
		uid, uname+"@t.l", uname, uname); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})

	pid := uuid.Must(uuid.NewRandom())
	prefix := "mg" + strings.ReplaceAll(pid.String()[:6], "-", "")
	if err := seedProjectRow(ctx, t, pool, pid, "mig_"+prefix, prefix, uid); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
	})

	id42 := prefix + "-42"
	id42_1 := prefix + "-42.1"
	id42_1_3 := prefix + "-42.1.3"
	id99 := prefix + "-99"

	insertIssue := func(idStr string, parentID *string) {
		if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, source_context, assignee, design, acceptance_criteria,
                    notes, estimated_minutes, due_at, defer_until, started_at, closed_at,
                    closed_reason, external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, $3, 1, $4, '', 'task', 2, 'open', false, 'human', NULL, NULL, '', '', '',
        NULL, NULL, NULL, NULL, NULL, NULL, NULL, $5, now(), now(), '{}'::jsonb)`,
			idStr, pid, parentID, idStr, uid); err != nil {
			t.Fatalf("seed issue %s: %v", idStr, err)
		}
	}
	insertIssue(id42, nil)
	insertIssue(id42_1, &id42)
	insertIssue(id42_1_3, &id42_1)
	insertIssue(id99, nil)

	// Pre-existing dep row that simulates a sk-33 step-1 conversion
	// (was parent-child, became 'related' with a migration_note). The
	// 000022 backfill must NOT touch this.
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'related',
        '{"migration_note":"converted from parent-child by 000020"}'::jsonb,
        1, $3, now())`, id42, id99, uid); err != nil {
		t.Fatalf("seed sk-33 leftover: %v", err)
	}

	// To exercise the migration logic against THIS fixture (the
	// migration already applied at DB-init time backfilled what was
	// there at that moment — our fresh rows are new), run the same
	// INSERT…ON CONFLICT statement here. Idempotent vs the global apply.
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (
  from_id, to_id, type, metadata, version, created_by, created_at
)
SELECT
  i.id, i.parent_id, 'parent-child',
  jsonb_build_object('migration_note','backfilled from issues.parent_id by 000022'),
  1,
  COALESCE(i.created_by, '00000000-0000-0000-0000-000000000000'::uuid),
  COALESCE(i.created_at, now())
FROM issues i
WHERE i.parent_id IS NOT NULL
  AND i.project_id = $1
ON CONFLICT (from_id, to_id, type) DO NOTHING`, pid); err != nil {
		t.Fatalf("backfill step: %v", err)
	}

	// Assert: every parent_id-bearing issue in this fixture got a
	// parent-child dep row.
	expected := map[string]string{
		id42_1:   id42,
		id42_1_3: id42_1,
	}
	for child, parent := range expected {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'parent-child'`, child, parent).Scan(&n); err != nil {
			t.Fatalf("query %s→%s: %v", child, parent, err)
		}
		if n != 1 {
			t.Errorf("expected 1 parent-child edge %s→%s, got %d", child, parent, n)
		}
	}

	// The pre-existing 'related' row must survive — never converted.
	var stillRelated int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'related'
   AND metadata->>'migration_note' = 'converted from parent-child by 000020'`,
		id42, id99).Scan(&stillRelated); err != nil {
		t.Fatalf("checking pre-existing related row: %v", err)
	}
	if stillRelated != 1 {
		t.Errorf("pre-existing related row gone (expected 1, got %d)", stillRelated)
	}

	// Step 3: child_counters update — last_child must reach the
	// highest seen suffix per parent. Apply the same UPSERT the
	// migration uses, scoped to our fixture's hierarchical ids.
	if _, err := pool.Exec(ctx, `
INSERT INTO child_counters (parent_id, last_child)
SELECT
  regexp_replace(i.id, '\.[0-9]+$', '')                AS parent,
  MAX((regexp_match(i.id, '\.([0-9]+)$'))[1]::int)     AS last_child
FROM issues i
WHERE i.id ~ '\.[0-9]+$'
  AND i.project_id = $1
GROUP BY 1
ON CONFLICT (parent_id) DO UPDATE
  SET last_child = GREATEST(child_counters.last_child, EXCLUDED.last_child)`, pid); err != nil {
		t.Fatalf("counter upsert: %v", err)
	}

	var lc int
	if err := pool.QueryRow(ctx, `SELECT last_child FROM child_counters WHERE parent_id = $1`, id42).Scan(&lc); err != nil {
		t.Fatalf("counter for %s: %v", id42, err)
	}
	if lc < 1 {
		t.Errorf("expected child_counters[%s].last_child >= 1, got %d", id42, lc)
	}
	if err := pool.QueryRow(ctx, `SELECT last_child FROM child_counters WHERE parent_id = $1`, id42_1).Scan(&lc); err != nil {
		t.Fatalf("counter for %s: %v", id42_1, err)
	}
	if lc < 3 {
		t.Errorf("expected child_counters[%s].last_child >= 3, got %d", id42_1, lc)
	}
}

// TestMigration000029_UpDownRoundTrip exercises sk-118 (F2a): the up
// migration must make status='standing' a storable value, and the
// DATA-GUARDED down migration must FIRST demote any standing row to 'open'
// and THEN re-add the 4-value CHECK (without the guard the down would abort
// on a live standing row).
//
// The test DB starts at head (000029 already applied by the harness), so
// the up side is asserted by inserting a standing row directly. We then
// seed a standing fixture, step the migration DOWN by one (runs 000029
// down), assert the seeded row flipped to 'open' AND that a fresh standing
// insert is now rejected by the restored 4-value CHECK, and finally migrate
// back UP to leave the DB at head for the rest of the suite (-p 1 runs
// sequentially, so a half-rewound schema would corrupt later tests).
func TestMigration000029_UpDownRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	// --- fixture: user + project (cleaned up at the end) ---
	uid := uuid.Must(uuid.NewRandom())
	uname := "st" + strings.ReplaceAll(uid.String()[:18], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', now(), '{}'::jsonb)`,
		uid, uname+"@t.l", uname, uname); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})

	pid := uuid.Must(uuid.NewRandom())
	prefix := "st" + strings.ReplaceAll(pid.String()[:6], "-", "")
	if err := seedProjectRow(ctx, t, pool, pid, "st_"+prefix, prefix, uid); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE project_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
	})

	insertStanding := func(idStr string) error {
		_, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, source_context, assignee, design, acceptance_criteria,
                    notes, estimated_minutes, due_at, defer_until, started_at, closed_at,
                    closed_reason, external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'standing', false, 'human', NULL, NULL, '', '', '',
        NULL, NULL, NULL, NULL, NULL, NULL, NULL, $4, now(), now(), '{}'::jsonb)`,
			idStr, pid, idStr, uid)
		return err
	}

	// --- up side: with 000029 applied (head), a standing row inserts OK ---
	standingID := prefix + "-1"
	if err := insertStanding(standingID); err != nil {
		t.Fatalf("expected standing insert to succeed with 000029 up, got: %v", err)
	}

	// --- run 000029 down ---
	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Pin the rollback to explicit version 28 (runs 000029 down) rather than
	// a head-relative Steps(-1): the DB starts at head, and head advances
	// every time a later migration (000030+) lands — Steps(-1) would then
	// roll back the WRONG migration, leaving 000029 applied and silently
	// breaking this assertion. Absolute Migrate(28) is stable across head moves.
	if err := m.Migrate(28); err != nil {
		t.Fatalf("Migrate(28) (000029 down): %v", err)
	}
	// Always return the DB to head for the rest of the suite.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// The data guard must have flipped the seeded standing row to 'open'.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issues WHERE id = $1`, standingID).Scan(&status); err != nil {
		t.Fatalf("read seeded row after down: %v", err)
	}
	if status != "open" {
		t.Errorf("after 000029 down, seeded standing row status = %q, want open", status)
	}

	// And the 4-value CHECK must be back: a fresh standing insert is rejected.
	if err := insertStanding(prefix + "-2"); err == nil {
		t.Errorf("expected standing insert to be rejected after 000029 down, but it succeeded")
	}
}

// TestMigration000033_UpDownRoundTrip exercises sk-163 S1: the up
// migration adds users.is_external (DEFAULT false) plus the
// users_external_not_admin CHECK that forbids is_external=true with
// role='admin'; the down drops the constraint, index, and column.
//
// The test DB starts at head (000033 already applied by the harness), so
// the up side is asserted directly: a plain insert defaults to false, an
// external role='user' insert succeeds, and an external role='admin'
// insert is rejected by the CHECK. We then step the migration DOWN by one
// (runs 000033 down), assert the column is gone, and migrate back UP to
// leave the DB at head for the rest of the suite (-p 1 runs sequentially).
func TestMigration000033_UpDownRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	// Track ids we insert so cleanup removes them regardless of which
	// migration version is live at teardown.
	var seeded []uuid.UUID
	t.Cleanup(func() {
		for _, id := range seeded {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
		}
	})

	insertUser := func(role string, isExternal *bool) (uuid.UUID, error) {
		id := uuid.Must(uuid.NewRandom())
		uname := "ext" + strings.ReplaceAll(id.String()[:18], "-", "")
		seeded = append(seeded, id)
		if isExternal == nil {
			// Exercise the column DEFAULT (no is_external in the column list).
			_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'h', now(), '{}'::jsonb)`,
				id, uname+"@t.l", uname, uname, role)
			return id, err
		}
		_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'h', $6, now(), '{}'::jsonb)`,
			id, uname+"@t.l", uname, uname, role, *isExternal)
		return id, err
	}

	// --- up side (head) ---
	// (a) DEFAULT false: a plain insert lands is_external = false.
	defID, err := insertUser("user", nil)
	if err != nil {
		t.Fatalf("default insert: %v", err)
	}
	var gotExternal bool
	if err := pool.QueryRow(ctx, `SELECT is_external FROM users WHERE id = $1`, defID).Scan(&gotExternal); err != nil {
		t.Fatalf("read default is_external: %v", err)
	}
	if gotExternal {
		t.Errorf("default is_external = true, want false")
	}

	// (b) external + role='user' is allowed.
	tru := true
	if _, err := insertUser("user", &tru); err != nil {
		t.Errorf("external role=user insert rejected, want success: %v", err)
	}

	// (c) external + role='admin' is rejected by users_external_not_admin.
	if _, err := insertUser("admin", &tru); err == nil {
		t.Errorf("external role=admin insert succeeded, want CHECK rejection")
	}

	// --- run 000033 down ---
	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Pin the rollback to explicit version 32 rather than a head-relative
	// Steps(-1): this keeps the test correct after a later 000034+ migration
	// lands (Steps(-1) would then roll back the WRONG migration — exactly
	// the stale-head assumption that silently broke TestMigration000029).
	if err := m.Migrate(32); err != nil {
		t.Fatalf("Migrate(32) (000033 down): %v", err)
	}
	// Always return the DB to head for the rest of the suite.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// After the down, the column must be gone — selecting it errors with
	// undefined_column (SQLSTATE 42703).
	if _, err := pool.Exec(ctx, `SELECT is_external FROM users LIMIT 1`); err == nil {
		t.Errorf("is_external still selectable after 000033 down, want undefined column")
	}
}

// TestMigration000034_UpDownRoundTrip exercises sk-163 S3: the up migration
// installs the cross-table trigger skein_enforce_external_not_owner that
// rejects an external user being made a project owner (custom SQLSTATE
// 'SK001'); the down drops both triggers and the function.
//
// The test DB starts at head (000034 already applied by the harness), so the
// up side is asserted directly: a direct INSERT of an external owner row is
// rejected by the trigger. We then step DOWN to version 33 (runs 000034
// down), assert the trigger function is gone, and migrate back UP to leave the
// DB at head for the rest of the suite (-p 1 runs sequentially).
func TestMigration000034_UpDownRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	// --- fixture: internal owner, external user, project (cleaned up) ---
	ownerID := uuid.Must(uuid.NewRandom())
	ownerName := "ow" + strings.ReplaceAll(ownerID.String()[:18], "-", "")
	extID := uuid.Must(uuid.NewRandom())
	extName := "ex" + strings.ReplaceAll(extID.String()[:18], "-", "")
	pid := uuid.Must(uuid.NewRandom())
	prefix := "t34" + strings.ReplaceAll(pid.String()[:6], "-", "")

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM project_members WHERE project_id = $1`, pid)
		_, _ = pool.Exec(c, `DELETE FROM projects WHERE id = $1`, pid)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{ownerID, extID})
	})

	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', now(), '{}'::jsonb)`,
		ownerID, ownerName+"@t.l", ownerName, ownerName); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', true, now(), '{}'::jsonb)`,
		extID, extName+"@t.l", extName, extName); err != nil {
		t.Fatalf("seed external: %v", err)
	}
	if err := seedProjectRow(ctx, t, pool, pid, "t34_"+prefix, prefix, ownerID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// --- up side (head): direct external-owner INSERT is rejected ---
	if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $3)`, pid, extID, ownerID); err == nil {
		t.Fatalf("external owner INSERT succeeded with 000034 up, want trigger rejection")
	}

	// --- run 000034 down ---
	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Register the restore-to-head BEFORE the down step: if Migrate(33) (or
	// any assertion after it) calls Fatalf, this deferred Up still runs and
	// leaves the shared -p1 DB at head — otherwise a mid-test failure would
	// strand the DB at v33 and corrupt every later package's fixtures.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// Pin the rollback to explicit version 33 (runs 000034 down) rather than a
	// head-relative Steps(-1): a later 000035+ migration would otherwise make
	// Steps(-1) roll back the WRONG migration. Absolute Migrate(33) is stable.
	if err := m.Migrate(33); err != nil {
		t.Fatalf("Migrate(33) (000034 down): %v", err)
	}

	// After the down, the trigger function must be gone.
	var fnExists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'skein_enforce_external_not_owner')`).Scan(&fnExists); err != nil {
		t.Fatalf("probe trigger function: %v", err)
	}
	if fnExists {
		t.Errorf("skein_enforce_external_not_owner still exists after 000034 down, want dropped")
	}
}

// TestMigration_Up_Idempotent: applying the migrations a second time is
// a no-op (golang-migrate.ErrNoChange). This is a smoke test confirming
// our test DB is at the head version + the migrator code path is wired.
func TestMigration_Up_Idempotent(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	// migrate "file://" wants an absolute path; the tests run from
	// the package dir where the SQL files live, so a relative path
	// resolves correctly.
	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 000039_notification_reason_defaults — sk-303.13 / T006
// ---------------------------------------------------------------------------
//
// Coverage-matrix rows 6 and 7 of the wave:
//
//   6 — storage state for new and existing accounts: exactly seven
//       reasons on by default, including two reserved; empty and
//       arbitrary previous prefs get ONE narrow default, nobody is left
//       in the "everything on" state;
//   7 — final schema after up/down/up: UNIQUE (user_id, event_id), four
//       channel-pair states, indexes and defaults; historical rows do
//       not become push-pending without classification.
//
// The tests follow the pattern of the neighbours above: the database starts at head, so both
// checks roll back to version 38, seed fixtures of "prehistoric" data,
// roll 000039 forward again and look at the result. The defer returns the database to
// head — under `-p 1` packages run sequentially and the next package must
// see the full schema.

// reasonDefaults000039 — the narrow default as the MIGRATION 000039 writes it,
// and not a row more. The backfill tests below check the rows the
// migration actually wrote against it.
//
// ⚠ THIS MAP IS FROZEN TOGETHER WITH THE MIGRATION. 000039 is applied, its text
// cannot be edited, and the number of rows it wrote is a historical fact,
// not the current size of the registry. That is why additions of later migrations
// live in separate maps below and are not appended here: the 000039 counter
// is compared with ITS number (see migration_counters_test.go), and an appended row
// would silently make the counter's expectation wrong.
var reasonDefaults000039 = map[string][2]bool{
	// reason                  {feed, push}
	"assignment":          {true, true},
	"accepted":            {true, true},
	"delegated_status":    {true, true},
	"reminder":            {false, true},
	"mention":             {true, true},
	"issue_created":       {false, false},
	"comment":             {false, false},
	"attachment":          {false, false},
	"reparent":            {false, false},
	"auto_unclaim":        {false, false},
	"other_edits":         {false, false},
	"unaccepted_reminder": {false, true},
	"review_reminder":     {false, true},
}

// reasonDefaults000051 — an addition of migration 000051 (sk-489): one reason,
// enabled on both channels.
var reasonDefaults000051 = map[string][2]bool{
	"outcome_changed": {true, true},
}

// reasonDefaultsFromSQL — the state of the settings at the HEAD of the chain: the sum of the maps
// above. This is what a reader will see in the table after a full roll-forward.
var reasonDefaultsFromSQL = func() map[string][2]bool {
	out := make(map[string][2]bool, len(reasonDefaults000039)+len(reasonDefaults000051))
	for _, m := range []map[string][2]bool{reasonDefaults000039, reasonDefaults000051} {
		for k, v := range m {
			if _, dup := out[k]; dup {
				panic("migrations: reason " + k + " is seeded by two migrations — " +
					"the later one would have to be an UPDATE, not an INSERT")
			}
			out[k] = v
		}
	}
	return out
}()

// seedUser inserts a throwaway user and registers its cleanup.
func seedUser(t *testing.T, pool *pgxpool.Pool, tag string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	uid := uuid.Must(uuid.NewRandom())
	uname := tag + strings.ReplaceAll(uid.String()[:14], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', now(), '{}'::jsonb)`,
		uid, uname+"@t.l", uname, uname); err != nil {
		t.Fatalf("seed user %s: %v", tag, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	return uid
}

// TestMigration000039_NarrowDefaultBackfill — row 6.
func TestMigration000039_NarrowDefaultBackfill(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Rollback to the pre-migration state — fixtures must look like
	// accounts that existed BEFORE the deploy.
	if err := m.Migrate(38); err != nil {
		t.Fatalf("Migrate(38) (000039 down): %v", err)
	}
	restored := false
	defer func() {
		if !restored {
			if err := m.Up(); err != nil && err != migrate.ErrNoChange {
				t.Fatalf("restore Up: %v", err)
			}
		}
	}()

	// Three accounts with different previous prefs: no prefs row at all,
	// an empty type list, and an arbitrary list plus disabled push
	// categories.
	noPrefs := seedUser(t, pool, "mnp")
	emptyPrefs := seedUser(t, pool, "mep")
	legacyPrefs := seedUser(t, pool, "mlp")

	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, notify_event_types, default_inbox_scope)
VALUES ($1, '{}'::text[], 'all')`, emptyPrefs); err != nil {
		t.Fatalf("seed empty prefs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, notify_event_types, default_inbox_scope, push_disabled_categories)
VALUES ($1, ARRAY['issue.created','comment.created'], 'watched_only', ARRAY['assigned'])`,
		legacyPrefs); err != nil {
		t.Fatalf("seed legacy prefs: %v", err)
	}

	// Re-apply the migration — that is the deploy moment.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up (000039): %v", err)
	}
	restored = true

	for name, uid := range map[string]uuid.UUID{
		"account without any prefs row": noPrefs,
		"account with empty whitelist":  emptyPrefs,
		"account with legacy whitelist": legacyPrefs,
	} {
		rows, err := pool.Query(ctx, `
SELECT reason, feed_enabled, push_enabled
  FROM user_notification_reason_prefs
 WHERE user_id = $1`, uid)
		if err != nil {
			t.Fatalf("%s: read reason prefs: %v", name, err)
		}
		got := map[string][2]bool{}
		for rows.Next() {
			var reason string
			var feed, push bool
			if err := rows.Scan(&reason, &feed, &push); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			got[reason] = [2]bool{feed, push}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", name, err)
		}

		// One narrow default for everyone, regardless of what was there before.
		if len(got) != len(reasonDefaultsFromSQL) {
			t.Errorf("%s: %d reason rows, want %d", name, len(got), len(reasonDefaultsFromSQL))
		}
		enabled := 0
		for reason, want := range reasonDefaultsFromSQL {
			g, ok := got[reason]
			if !ok {
				t.Errorf("%s: no row for reason %s", name, reason)
				continue
			}
			if g != want {
				t.Errorf("%s: reason %s = feed:%v push:%v, want feed:%v push:%v",
					name, reason, g[0], g[1], want[0], want[1])
			}
		}
		for _, g := range got {
			if g[0] || g[1] {
				enabled++
			}
		}
		// Eight is the state of the STORAGE at the head of the chain. Originally
		// seven were enabled, two of them reserved and not shown in the form;
		// sk-303.14 / T005 activated them, and
		// the storage number did not change — and it should not have: activation
		// touches the state of a reason, not the channel default. The eighth came with
		// 000051 (sk-489) — that one is a new settings ROW, not a change of
		// state. The form's number lives in its own test and is not
		// repeated here.
		//
		// The test brings the database to the HEAD (m.Up()), not to 39, so the expectation
		// here is the head: counting "what 000039 wrote" on a database where
		// 000051 has also run would mean expecting a number that is not in the table.
		if want := len(reasonDefaultsFromSQL); enabled != 8 {
			t.Errorf("%s: %d reasons enabled by default, want 8 (of %d rows)", name, enabled, want)
		}
		// Two reminder categories: a storage row exists and push is on —
		// whether that is a reserve (initial) or an active reason.
		// The move to the active state rests on this pre-written pref:
		// that release did not have to backfill defaults.
		for _, nudge := range []string{"unaccepted_reminder", "review_reminder"} {
			g, ok := got[nudge]
			if !ok {
				t.Errorf("%s: reminder reason %s missing from storage", name, nudge)
				continue
			}
			if !g[1] {
				t.Errorf("%s: reminder reason %s must be enabled in storage", name, nudge)
			}
		}
	}

	// SC-003 globally: no account is left in the "everything on" state.
	var allOn int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM (
    SELECT user_id
      FROM user_notification_reason_prefs
     GROUP BY user_id
    HAVING bool_and(feed_enabled) AND bool_and(push_enabled)
) x`).Scan(&allOn); err != nil {
		t.Fatalf("all-enabled probe: %v", err)
	}
	if allOn != 0 {
		t.Errorf("%d account(s) remain in the all-enabled state", allOn)
	}

	// Previous prefs are not migrated — but they are also not rewritten
	// by the migration: transfer is absent, it is not performed in reverse.
	var scope string
	var types []string
	if err := pool.QueryRow(ctx, `
SELECT default_inbox_scope, notify_event_types FROM user_notification_prefs WHERE user_id = $1`,
		legacyPrefs).Scan(&scope, &types); err != nil {
		t.Fatalf("read legacy prefs after migration: %v", err)
	}
	if scope != "watched_only" || len(types) != 2 {
		t.Errorf("legacy prefs row was rewritten by the migration: scope=%q types=%v", scope, types)
	}

	// Migration counters are written as separate named metrics and match
	// the control selects — otherwise "did it apply" is checked only by
	// sampling data.
	counters := map[string]int64{}
	crows, err := pool.Query(ctx, `
SELECT metric, value FROM migration_counters
 WHERE migration = '000039_notification_reason_defaults'`)
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	for crows.Next() {
		var metric string
		var value int64
		if err := crows.Scan(&metric, &value); err != nil {
			t.Fatalf("scan counter: %v", err)
		}
		counters[metric] = value
	}
	crows.Close()
	for _, metric := range []string{
		"accounts_defaulted", "reason_pref_rows_written",
		"notifications_marked_feed", "accounts_all_enabled_left",
	} {
		if _, ok := counters[metric]; !ok {
			t.Errorf("migration counter %q is missing", metric)
		}
	}
	// ⚠ THE CHECK IS NARROWED BY THE 000039 REASONS, and this is not fitting to the answer.
	// The test brings the database to the HEAD, and at the head settings rows are written not
	// only by 000039: 000051 (sk-489) added its own. The counter belongs to
	// 000039 and speaks about what IT wrote. Comparing it with the full
	// count(*) would mean demanding from an applied migration knowledge of
	// future ones — and every next migration seeding a reason would turn this
	// test red for no reason. The list of reasons is taken from the map frozen together
	// with the migration, not from the current registry.
	own := make([]string, 0, len(reasonDefaults000039))
	for reason := range reasonDefaults000039 {
		own = append(own, reason)
	}
	var accounts, prefRows int64
	if err := pool.QueryRow(ctx, `
SELECT count(DISTINCT user_id), count(*)
  FROM user_notification_reason_prefs
 WHERE reason = ANY($1::text[])`, own).
		Scan(&accounts, &prefRows); err != nil {
		t.Fatalf("control counts: %v", err)
	}
	if counters["accounts_defaulted"] != accounts {
		t.Errorf("counter accounts_defaulted = %d, control select = %d",
			counters["accounts_defaulted"], accounts)
	}
	if counters["reason_pref_rows_written"] != prefRows {
		t.Errorf("counter reason_pref_rows_written = %d, control select = %d",
			counters["reason_pref_rows_written"], prefRows)
	}
	if counters["accounts_all_enabled_left"] != 0 {
		t.Errorf("counter accounts_all_enabled_left = %d, want 0", counters["accounts_all_enabled_left"])
	}
}

// TestMigration000039_CheckConstraintMatchesRegistry — the CHECK on the
// reason column carries a FOURTH copy of the registry (the Go registry,
// the migration INSERT, the test default map and this CHECK). A
// divergence "code knows a reason, CHECK does not" is caught by none of
// the existing checks: it would surface as a DB refusal on a legitimate
// PUT, i.e. 500 instead of 200 for the user.
func TestMigration000039_CheckConstraintMatchesRegistry(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)

	var def string
	if err := pool.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = 'user_notification_reason_prefs'
   AND c.contype = 'c'
   AND pg_get_constraintdef(c.oid) LIKE '%reason%'`).Scan(&def); err != nil {
		t.Fatalf("read the reason CHECK: %v", err)
	}

	// pg_get_constraintdef prints the list as ARRAY['assignment'::text, …];
	// the set of keys is extracted from single quotes.
	inCheck := map[string]bool{}
	parts := strings.Split(def, "'")
	for i := 1; i < len(parts); i += 2 {
		inCheck[parts[i]] = true
	}
	if len(inCheck) == 0 {
		t.Fatalf("no literals parsed out of the constraint definition: %s", def)
	}

	inCode := map[string]bool{}
	for _, r := range notifications.AllReasons() {
		inCode[string(r.Key)] = true
		if !inCheck[string(r.Key)] {
			t.Errorf("reason %s is in the code registry but the CHECK would refuse it", r.Key)
		}
	}
	for key := range inCheck {
		if !inCode[key] {
			t.Errorf("the CHECK allows reason %s which the code registry does not know", key)
		}
	}
}

// TestMigration000039_SchemaUpDownUp — row 7.
func TestMigration000039_SchemaUpDownUp(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(38); err != nil {
		t.Fatalf("Migrate(38) (000039 down): %v", err)
	}
	// The database stays at 38 until the "deploy" below and is returned to
	// HEAD here — unconditionally, because this test's "deploy" only takes
	// the chain to 39, and the next test needs the full schema.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	// After rollback the columns and table must be gone — otherwise
	// "rollback" rolls nothing back.
	for _, col := range []string{"reason", "feed_channel", "push_state"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'notifications' AND column_name = $1)`, col).Scan(&exists); err != nil {
			t.Fatalf("probe column %s: %v", col, err)
		}
		if exists {
			t.Errorf("after down, notifications.%s still exists", col)
		}
	}
	var prefsTable *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('user_notification_reason_prefs')::text`).Scan(&prefsTable); err != nil {
		t.Fatalf("probe prefs table: %v", err)
	}
	if prefsTable != nil {
		t.Errorf("after down, user_notification_reason_prefs still exists")
	}

	// A historical notification row, created BEFORE the deploy.
	uid := seedUser(t, pool, "msr")
	pid := uuid.Must(uuid.NewRandom())
	prefix := "ms" + strings.ReplaceAll(pid.String()[:6], "-", "")
	if err := seedProjectRow(ctx, t, pool, pid, "ms_"+prefix, prefix, uid); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
	})

	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type)
VALUES ($1, 'issue', $2, 'issue.created', 'seed', 'human') RETURNING id`,
		pid, prefix+"-1").Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id)
VALUES ($1, $2, 'issue.created', $3)`, uid, eventID, pid); err != nil {
		t.Fatalf("seed historical notification: %v", err)
	}

	// Deploy — EXACTLY to 39. This test is about migration 1's schema and
	// about the historical row surviving until classification: "reason =
	// NULL until migration 3 classifies it" — a claim that is true at
	// version 39 and meaningless at HEAD, where 000041 has already
	// classified this row and deleted it as a reason turned off by the
	// new default. m.Up() here would make the test a hostage of every
	// later migration.
	if err := m.Migrate(39); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(39): %v", err)
	}

	// Historical row: visible in the feed as it was, but it did not enter
	// the push queue — push state does not become pending without
	// classification.
	var feedChannel bool
	var pushState string
	var attempts int
	var reason *string
	if err := pool.QueryRow(ctx, `
SELECT feed_channel, push_state, push_attempts, reason
  FROM notifications WHERE user_id = $1 AND event_id = $2`, uid, eventID).
		Scan(&feedChannel, &pushState, &attempts, &reason); err != nil {
		t.Fatalf("read historical notification: %v", err)
	}
	if !feedChannel {
		t.Error("historical notification lost its feed channel")
	}
	if pushState != "none" {
		t.Errorf("historical notification push_state = %q, want none", pushState)
	}
	if attempts != 0 {
		t.Errorf("historical notification push_attempts = %d, want 0", attempts)
	}
	if reason != nil {
		t.Errorf("historical notification reason = %q, want NULL until migration 3 classifies it", *reason)
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE push_state <> 'none'`).Scan(&pending); err != nil {
		t.Fatalf("pending probe: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d historical notification(s) entered the push lifecycle", pending)
	}

	// A new row by default does NOT belong to the feed: belonging is
	// stamped by fan-out explicitly, and the default has no right to gift
	// it.
	var freshEventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type)
VALUES ($1, 'issue', $2, 'comment.created', 'seed', 'human') RETURNING id`,
		pid, prefix+"-1").Scan(&freshEventID); err != nil {
		t.Fatalf("seed fresh event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id)
VALUES ($1, $2, 'comment.created', $3)`, uid, freshEventID, pid); err != nil {
		t.Fatalf("insert fresh notification: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT feed_channel, push_state FROM notifications WHERE user_id = $1 AND event_id = $2`,
		uid, freshEventID).Scan(&feedChannel, &pushState); err != nil {
		t.Fatalf("read fresh notification: %v", err)
	}
	if feedChannel {
		t.Error("a fresh notification defaults to feed_channel = true; a push-only row would leak into every feed")
	}
	if pushState != "none" {
		t.Errorf("fresh notification push_state = %q, want none", pushState)
	}

	// UNIQUE (user_id, event_id) survives the up/down/up round: one row
	// per "recipient × event" pair — that is what the whole reason model
	// stands on.
	if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id)
VALUES ($1, $2, 'comment.created', $3)`, uid, freshEventID, pid); err == nil {
		t.Error("duplicate (user_id, event_id) insert succeeded — the UNIQUE constraint is gone")
	}

	// Four channel-pair states are expressible in prefs.
	states := [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}}
	for _, st := range states {
		if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'comment', $2, $3)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = EXCLUDED.feed_enabled,
                                            push_enabled = EXCLUDED.push_enabled`,
			uid, st[0], st[1]); err != nil {
			t.Errorf("channel state feed:%v push:%v is not storable: %v", st[0], st[1], err)
		}
	}

	// A terminal push state and a terminal outcome exist only together —
	// otherwise "sent" and "not yet" are indistinguishable after a restart.
	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_state = 'final' WHERE user_id = $1 AND event_id = $2`,
		uid, freshEventID); err == nil {
		t.Error("push_state=final without an outcome was accepted")
	}
	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_outcome = 'success' WHERE user_id = $1 AND event_id = $2`,
		uid, freshEventID); err == nil {
		t.Error("push_outcome without a final state was accepted")
	}
	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_state = 'final', push_outcome = 'success'
 WHERE user_id = $1 AND event_id = $2`, uid, freshEventID); err != nil {
		t.Errorf("legitimate terminal state rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_state = 'somewhere' WHERE user_id = $1 AND event_id = $2`,
		uid, freshEventID); err == nil {
		t.Error("an unknown push_state was accepted")
	}

	// Indexes of reading surfaces and of the delivery loop are in place.
	for _, idx := range []string{
		"idx_notif_feed_unread",
		"idx_notif_feed_user",
		"idx_notif_push_pending",
		"idx_notif_push_leased",
		"idx_notif_push_final_invisible",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx).Scan(&exists); err != nil {
			t.Fatalf("probe index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("index %s is missing after up/down/up", idx)
		}
	}

	// And the defaults of the new columns are those the runtime rests on.
	var feedDefault, stateDefault, attemptsDefault string
	if err := pool.QueryRow(ctx, `
SELECT
  (SELECT column_default FROM information_schema.columns
    WHERE table_name = 'notifications' AND column_name = 'feed_channel'),
  (SELECT column_default FROM information_schema.columns
    WHERE table_name = 'notifications' AND column_name = 'push_state'),
  (SELECT column_default FROM information_schema.columns
    WHERE table_name = 'notifications' AND column_name = 'push_attempts')`).
		Scan(&feedDefault, &stateDefault, &attemptsDefault); err != nil {
		t.Fatalf("read column defaults: %v", err)
	}
	if !strings.Contains(feedDefault, "false") {
		t.Errorf("feed_channel default = %q, want false", feedDefault)
	}
	if !strings.Contains(stateDefault, "none") {
		t.Errorf("push_state default = %q, want 'none'", stateDefault)
	}
	if !strings.Contains(attemptsDefault, "0") {
		t.Errorf("push_attempts default = %q, want 0", attemptsDefault)
	}
}

// ensureActiveAdmin seeds an active non-external admin when the database has
// none (removed with the test; a user the transition made the common space's
// owner stays, as spaces.owner_id is RESTRICT).
func ensureActiveAdmin(ctx context.Context, t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM users
                WHERE role = 'admin' AND deactivated_at IS NULL AND NOT is_external)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	admin := seedUser(t, pool, "madm")
	_, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, admin)
	return err
}

// seedProjectRow inserts a project row the way the schema at hand takes it:
// on a schema with projects.space_id (000069 and later; NOT NULL since
// 000080, sk-674.18 T024) the project goes into the shared test space, on an
// older one there is no such column.
func seedProjectRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID, name, prefix string, owner uuid.UUID) error {
	t.Helper()
	var hasSpace bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'projects' AND column_name = 'space_id')`).Scan(&hasSpace); err != nil {
		return err
	}
	if !hasSpace {
		// A project without a space reaches 000071/000080 on the way up, and
		// they move it into the common space owned by an active admin — the
		// precondition the api checks before migrating (INV-20).
		if err := ensureActiveAdmin(ctx, t, pool); err != nil {
			return err
		}
		_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb)`,
			id, name, prefix, owner)
		return err
	}
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at, created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, testsupport.TestSpace(t, pool))
	return err
}

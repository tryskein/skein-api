package migrations

// migration_000040_test.go — coverage-matrix row 25 (sk-303.13, T018).
//
// What is proved here. Wave migration 2 strips auto-watches "for a comment"
// and "for a mention" and deletes the watch row ONLY when after the strip
// no other live reason remains (US-8, SC-007). The invariant is stated as
// a general rule, so it is checked as a general rule: on mixed reason
// arrays, where every surviving reason keeps the watch alive — 'manual',
// 'creator', 'assignee' and the sticky 'unwatched' mark alike.
//
// Why 'unwatched' deserves its own case: absence of a row means "not
// unwatched", not "unwatched". If the migration deleted the row
// {unwatched, commenter}, a person who unwatched by hand would start
// receiving the feed again — i.e. the cleanup would return exactly the
// noise it exists to remove.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedProject inserts a project owned by owner and registers its cleanup. The
// prefix is derived from the project uuid so parallel fixtures never collide on
// the hierarchical issue ids built from it.
func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, tag string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	pid := uuid.Must(uuid.NewRandom())
	prefix := tag + strings.ReplaceAll(pid.String()[:6], "-", "")
	if err := seedProjectRow(ctx, t, pool, pid, tag+"_"+prefix, prefix, owner); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE project_id = $1`, pid)
		_, _ = pool.Exec(bg, `DELETE FROM issues WHERE project_id = $1`, pid)
		_, _ = pool.Exec(bg, `DELETE FROM projects WHERE id = $1`, pid)
	})
	return pid, prefix
}

// seedIssue inserts a bare open issue. assignee is the raw handle stored on the
// row (empty string → SQL NULL), mirroring what the issues store writes.
func seedIssue(t *testing.T, pool *pgxpool.Pool, id string, projectID, creator uuid.UUID, assignee string) {
	t.Helper()
	var asg any
	if assignee != "" {
		asg = assignee
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, source_context, assignee, design, acceptance_criteria,
                    notes, estimated_minutes, due_at, defer_until, started_at, closed_at,
                    closed_reason, external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $5, '', 'task', 2, 'open', false, 'human', NULL, $3, '', '', '',
        NULL, NULL, NULL, NULL, NULL, NULL, NULL, $4, now(), now(), '{}'::jsonb)`,
		id, projectID, asg, creator, id); err != nil {
		t.Fatalf("seed issue %s: %v", id, err)
	}
}

func TestMigration000040_PruneAutoWatchReasons(t *testing.T) {
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

	// State BEFORE migration 2. Version 39, not 38: the counters journal is
	// created by migration 1, and without it the "write counters" step has
	// nothing to compare against.
	if err := m.Migrate(39); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(39): %v", err)
	}
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	owner := seedUser(t, pool, "w40a")
	other := seedUser(t, pool, "w40b")
	pid, prefix := seedProject(t, pool, owner, "w40")

	// Each case is its own issue, so the primary key (issue_id, user_id) does
	// not force mixing scenarios in one row.
	type watchCase struct {
		name    string
		user    uuid.UUID
		scope   string
		reasons []string
		// want == nil means "the row must disappear".
		want []string
	}
	cases := []watchCase{
		{"only commenter", owner, "issue", []string{"commenter"}, nil},
		{"only mention", owner, "issue", []string{"mention"}, nil},
		{"both forbidden", owner, "issue", []string{"commenter", "mention"}, nil},
		{"creator survives", owner, "issue", []string{"creator", "commenter"}, []string{"creator"}},
		{"assignee survives", owner, "issue", []string{"commenter", "assignee", "mention"}, []string{"assignee"}},
		{"sticky unwatch survives", owner, "issue", []string{"unwatched", "commenter"}, []string{"unwatched"}},
		{"manual survives", other, "issue", []string{"manual", "mention"}, []string{"manual"}},
		{"order preserved", other, "issue",
			[]string{"manual", "commenter", "unwatched", "mention"}, []string{"manual", "unwatched"}},
		{"subtree scope survives", other, "subtree", []string{"commenter", "manual"}, []string{"manual"}},
		{"untouched row", other, "issue", []string{"creator", "assignee"}, []string{"creator", "assignee"}},
		// The migration neither creates nor touches an empty reasons array:
		// only rows that EMPTIED after stripping forbidden reasons are
		// deleted.
		{"pre-existing empty array untouched", owner, "issue", []string{}, []string{}},
	}

	ids := make([]string, len(cases))
	for i, c := range cases {
		id := prefix + "-" + string(rune('a'+i))
		ids[i] = id
		seedIssue(t, pool, id, pid, owner, "")
		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope)
VALUES ($1, $2, $3::text[], $4)`, id, c.user, c.reasons, c.scope); err != nil {
			t.Fatalf("seed watcher %s: %v", c.name, err)
		}
	}

	// Control numbers are taken BEFORE the deploy and over the WHOLE table:
	// migration counters are global, so comparing them to one fixture's
	// numbers would be comparing different quantities.
	forbidden := []string{"commenter", "mention"}
	var wantAccounts, wantTouched, wantDeleted int64
	if err := pool.QueryRow(ctx, `
SELECT count(DISTINCT user_id), count(*),
       count(*) FILTER (WHERE reasons <@ $1::text[])
  FROM issue_watchers
 WHERE reasons && $1::text[]`, forbidden).
		Scan(&wantAccounts, &wantTouched, &wantDeleted); err != nil {
		t.Fatalf("control counts before the deploy: %v", err)
	}
	if wantTouched == 0 || wantDeleted == 0 {
		t.Fatalf("fixture is inert: touched=%d deleted=%d — the migration would have nothing to do",
			wantTouched, wantDeleted)
	}

	// Deploy of migration 2 — exactly to 40. defer drives the chain from there.
	if err := m.Migrate(40); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(40): %v", err)
	}

	for i, c := range cases {
		var reasons []string
		var scope string
		err := pool.QueryRow(ctx, `
SELECT reasons, scope FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
			ids[i], c.user).Scan(&reasons, &scope)
		if c.want == nil {
			if err == nil {
				t.Errorf("%s: row survived with reasons %v, want it deleted", c.name, reasons)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: row is gone (%v), want reasons %v", c.name, err, c.want)
			continue
		}
		if strings.Join(reasons, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: reasons = %v, want %v", c.name, reasons, c.want)
		}
		if scope != c.scope {
			t.Errorf("%s: scope = %q, want %q", c.name, scope, c.scope)
		}
	}

	// Not a single forbidden reason remains anywhere — a general rule, not
	// the fixture's case list.
	var leftover int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM issue_watchers WHERE reasons && $1::text[]`, forbidden).Scan(&leftover); err != nil {
		t.Fatalf("leftover probe: %v", err)
	}
	if leftover != 0 {
		t.Errorf("%d subscription(s) still carry a pruned reason", leftover)
	}

	// Migration counters — named and equal to the control selects.
	counters := readMigrationCounters(t, pool, "000040_prune_auto_watch_reasons")
	for metric, want := range map[string]int64{
		"watch_accounts_affected":       wantAccounts,
		"watch_rows_with_pruned_reason": wantTouched,
		"watch_rows_deleted":            wantDeleted,
		"watch_rows_kept_other_reason":  wantTouched - wantDeleted,
	} {
		got, ok := counters[metric]
		if !ok {
			t.Errorf("migration counter %q is missing", metric)
			continue
		}
		if got != want {
			t.Errorf("counter %s = %d, control select = %d", metric, got, want)
		}
	}
}

// readMigrationCounters returns the named counters one migration left behind.
func readMigrationCounters(t *testing.T, pool *pgxpool.Pool, migration string) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT metric, value FROM migration_counters WHERE migration = $1`, migration)
	if err != nil {
		t.Fatalf("read counters of %s: %v", migration, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var metric string
		var value int64
		if err := rows.Scan(&metric, &value); err != nil {
			t.Fatalf("scan counter of %s: %v", migration, err)
		}
		out[metric] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("counter rows of %s: %v", migration, err)
	}
	return out
}

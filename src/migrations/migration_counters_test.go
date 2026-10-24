package migrations

// migration_counters_test.go — coverage-matrix row 27 (sk-303.13, plan tasks
// T005/T018/T020).
//
// Observability of migrations is a spec requirement, not decoration:
// "Migrations must leave in the journal the number of affected accounts
// and watches: without that, 'did it apply' is checked only by sampling
// data". Hence two different obligations, and the test checks both:
//
//   * OUTPUT. Each wave migration prints a line naming its counters BY
//     NAME and separately — a single total is useless to the operator.
//     Output is intercepted for real: golang-migrate runs on a connection
//     whose client_min_messages = log, so the migration's own RAISE LOG
//     arrives at the client as a notice and lands in this test.
//   * NUMBERS. Each number from the output is reproduced by an INDEPENDENT
//     select — a control quantity taken before the deploy, or a
//     "was minus became" difference, not the same query the migration
//     itself wrote. A query matching itself proves nothing.
//
// Plus the check T019 exists for: migration 000041 deleted_total equals
// the "to_delete" number from the preflight report.

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// capturingMigrator returns a golang-migrate instance whose connection delivers
// the server's LOG messages to the client, plus an accessor for what was said.
//
// Why the detour through a hand-built *sql.DB: migrate.New opens its own pool and
// gives no hook, so the RAISE LOG lines would go to the server's log file — which
// the test container cannot read. Registering a pgx config with an OnNotice
// handler and handing the resulting driver to NewWithDatabaseInstance keeps the
// migrations running through the REAL golang-migrate path while making their
// output observable.
func capturingMigrator(t *testing.T, url string) (*migrate.Migrate, func() []string) {
	t.Helper()

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// LOG-level reaches the client only at this client_min_messages;
	// by default it stays in the server log.
	cfg.RuntimeParams["client_min_messages"] = "log"

	var mu sync.Mutex
	var lines []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, n.Message)
	}

	name := stdlib.RegisterConnConfig(cfg)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(name) })

	db, err := sql.Open("pgx", name)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// One connection: migrations run strictly sequentially, and interception
	// must not depend on which connection the DO-block landed on.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	drv, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		t.Fatalf("pgx WithInstance: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://.", "pgx5", drv)
	if err != nil {
		t.Fatalf("NewWithDatabaseInstance: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })

	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(lines))
		copy(out, lines)
		return out
	}
}

// findOutputLine returns the captured line the named migration printed, or "".
//
// Match by NUMBER, not by file name: migrations print "migration 000039: …".
// Requiring the full file name in the line would mean editing already-applied
// migration 000039 for format — and applied migrations cannot be edited: on
// stands sitting at version ≥39 the edit simply would not run, and the
// journal would diverge from the text in the repository.
func findOutputLine(lines []string, migration string) string {
	number, _, ok := strings.Cut(migration, "_")
	if !ok {
		return ""
	}
	prefix := "migration " + number + ":"
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestWaveMigrations_NamedCountersInOutput(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := t.Context()

	m, captured := capturingMigrator(t, url)

	// State before the whole wave — the same one preflight runs on in the window.
	if err := m.Migrate(38); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(38): %v", err)
	}
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	// Fixture: one reason for each of the three migrations.
	owner := seedUser(t, pool, "wcta")
	reader := seedUser(t, pool, "wctb")
	pid, prefix := seedProject(t, pool, owner, "wct")
	issueID := prefix + "-1"
	seedIssue(t, pool, issueID, pid, owner, "")

	// 000040: one orphaned watch and one with a surviving reason.
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, ARRAY['commenter'])`,
		issueID, owner); err != nil {
		t.Fatalf("seed orphan watcher: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, ARRAY['manual','mention'])`,
		issueID, reader); err != nil {
		t.Fatalf("seed kept watcher: %v", err)
	}

	// 000041: two histories — one kept, one deleted.
	var keptEvent, prunedEvent int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type, payload)
VALUES ($1, 'issue', $2, 'issue.status_changed', 'seed', 'human', '{}'::jsonb) RETURNING id`,
		pid, issueID).Scan(&keptEvent); err != nil {
		t.Fatalf("seed kept event: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type, payload)
VALUES ($1, 'comment', $2, 'comment.created', 'seed', 'human', '{}'::jsonb) RETURNING id`,
		pid, prefix+"-c").Scan(&prunedEvent); err != nil {
		t.Fatalf("seed pruned event: %v", err)
	}
	for _, ev := range []struct {
		id     int64
		evType string
	}{{keptEvent, "issue.status_changed"}, {prunedEvent, "comment.created"}} {
		if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id) VALUES ($1, $2, $3, $4)`,
			reader, ev.id, ev.evType, pid); err != nil {
			t.Fatalf("seed notification %s: %v", ev.evType, err)
		}
	}

	// ---- independent control quantities, taken BEFORE the deploy ----
	var usersTotal, notificationsBefore int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&usersTotal); err != nil {
		t.Fatalf("control users: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&notificationsBefore); err != nil {
		t.Fatalf("control notifications: %v", err)
	}
	forbidden := []string{"commenter", "mention"}
	var watchAccounts, watchTouched, watchDeleted int64
	if err := pool.QueryRow(ctx, `
SELECT count(DISTINCT user_id), count(*), count(*) FILTER (WHERE reasons <@ $1::text[])
  FROM issue_watchers WHERE reasons && $1::text[]`, forbidden).
		Scan(&watchAccounts, &watchTouched, &watchDeleted); err != nil {
		t.Fatalf("control watchers: %v", err)
	}
	// How many settings rows migration 000039 wrote — the number of ITS list, not
	// the length of the current registry.
	//
	// Here stood len(notifications.DefaultChannelStates()), and it was correct
	// exactly until the first migration that added a reason: the code registry describes the HEAD
	// of the chain, while the 000039 counter is what it wrote in its own window. Once
	// 000051 (sk-489) added the fourteenth reason, "the registry length" became
	// an expectation the counter does not have and cannot have — a migration
	// cannot be rewritten retroactively. The source is still independent of the SQL of 000039
	// itself: it comes from the reasonDefaults000039 map in migrations_test.go,
	// which is frozen together with the migration.
	reasonsInRegistry := int64(len(reasonDefaults000039))

	// Control for 000044 (sk-303.14, T004 — matrix row 15). Taken here,
	// like the rest: BEFORE the deploy and by an INDEPENDENT query. The
	// schema migration counts the same thing with its counters, and the
	// numbers matching is the proof that it ran on the state captured
	// here. The wave's index migrations (000046, 000047) are not added
	// here: they have no counters of their own.
	var issuesTotal, issuesAssigned, prefsRows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issues`).Scan(&issuesTotal); err != nil {
		t.Fatalf("control issues: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issues WHERE assignee IS NOT NULL`).
		Scan(&issuesAssigned); err != nil {
		t.Fatalf("control assigned issues: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_notification_prefs`).Scan(&prefsRows); err != nil {
		t.Fatalf("control notification prefs: %v", err)
	}

	preflight := preflightClassCounts(t, pool)
	preflightTotal, ok := preflight["TOTAL"]
	if !ok {
		t.Fatal("preflight has no TOTAL row")
	}

	// ---- deploy with output interception ----
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up: %v", err)
	}
	lines := captured()
	if len(lines) == 0 {
		t.Fatal("no server output was captured at all — the interception itself is broken")
	}

	var notificationsAfter int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&notificationsAfter); err != nil {
		t.Fatalf("notifications after: %v", err)
	}

	// ---- expectations: migration name → metric → independently computed value ----
	type expectation map[string]int64
	waves := []struct {
		migration string
		want      expectation
	}{
		{
			"000039_notification_reason_defaults",
			expectation{
				"accounts_defaulted":        usersTotal,
				"reason_pref_rows_written":  usersTotal * reasonsInRegistry,
				"notifications_marked_feed": notificationsBefore,
				"accounts_all_enabled_left": 0,
			},
		},
		{
			"000040_prune_auto_watch_reasons",
			expectation{
				"watch_accounts_affected":       watchAccounts,
				"watch_rows_with_pruned_reason": watchTouched,
				"watch_rows_deleted":            watchDeleted,
				"watch_rows_kept_other_reason":  watchTouched - watchDeleted,
			},
		},
		{
			"000041_classify_prune_notification_history",
			expectation{
				"history_rows_classified": notificationsBefore,
				"kept_enabled_reason":     notificationsAfter,
				"deleted_total":           notificationsBefore - notificationsAfter,
			},
		},
		{
			// sk-303.14 / T003. Three separate metrics: how many issues
			// were left with the "not required" flag (SC-015), how many of
			// those assigned were left without a burst schedule base —
			// this number the operator must see at deploy — and how many
			// user-prefs rows received the mechanic's values.
			"000044_review_and_nudges",
			expectation{
				"issues_defaulted_no_review":   issuesTotal,
				"issues_assigned_without_base": issuesAssigned,
				"prefs_rows_defaulted":         prefsRows,
			},
		},
	}

	for _, w := range waves {
		counters := readMigrationCounters(t, pool, w.migration)
		if len(counters) == 0 {
			t.Errorf("%s left no counters at all", w.migration)
			continue
		}

		// Numbers: each equals an independent control quantity.
		for metric, want := range w.want {
			got, ok := counters[metric]
			if !ok {
				t.Errorf("%s: counter %q is missing", w.migration, metric)
				continue
			}
			if got != want {
				t.Errorf("%s: counter %s = %d, independent control = %d",
					w.migration, metric, got, want)
			}
		}

		// Output: the line exists, names the migration and EACH of its
		// metrics by name with the same value that sits in the counters journal.
		line := findOutputLine(lines, w.migration)
		if line == "" {
			t.Errorf("%s printed nothing — its counters exist only in the table", w.migration)
			continue
		}
		for metric, value := range counters {
			pair := fmt.Sprintf("%s=%d", metric, value)
			if !strings.Contains(line, pair) {
				t.Errorf("%s output does not name %q: %s", w.migration, pair, line)
			}
		}
		// A single total is useless to the operator: at least three separate metrics.
		if len(counters) < 3 {
			t.Errorf("%s left only %d counter(s); separate named counters are the point",
				w.migration, len(counters))
		}
	}

	// The check T019 exists for: what preflight named "to_delete",
	// migration 000041 actually deleted.
	c41 := readMigrationCounters(t, pool, "000041_classify_prune_notification_history")
	if c41["deleted_total"] != preflightTotal.ToDelete {
		t.Errorf("deleted_total = %d, preflight promised %d to delete",
			c41["deleted_total"], preflightTotal.ToDelete)
	}
	if c41["history_rows_classified"] != preflightTotal.Records {
		t.Errorf("history_rows_classified = %d, preflight counted %d rows",
			c41["history_rows_classified"], preflightTotal.Records)
	}
}

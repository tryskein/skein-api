package migrations

// migration_000041_test.go — coverage-matrix row 26 (sk-303.13, plan tasks
// T019/T020 + corpus T002).
//
// Three assertions, and they are different:
//
//  1. The SQL classifier projection and the runtime give the SAME
//     classification on every case of the shared corpus. There cannot be
//     one executable function here — a migration does not call application
//     code — so the algorithm exists as two instances, and the only thing
//     holding them together is the regression set that both execute. The
//     corpus is read FROM THE FILE, the same one the runtime half reads: a
//     Go-rewritten list of cases would prove a copy matching a copy.
//
//     ⚠ There are THREE classification instances, not two (sk-303.14 /
//     T005, matrix rows 17–18). The third is the reason projection
//     in the delivery path, internal/outbox/push.go; it is checked against
//     THE SAME corpus by a separate assertion in
//     internal/outbox/push_classification_parity_test.go, and is not
//     considered covered by the first two matching. A green colour of this
//     file says nothing about the third implementation.
//
//     ⚠ The function checked here no longer lives in 000041: sk-303.14
//     replaced it with the 000045 revision (twelfth parameter "digest
//     recipient", two private journal types, two digest types). The file
//     is named after the migration that introduced the function; the
//     assertion is about the function that stands in the schema now.
//
//  2. The closed list of "events that live only in the journal" in SQL
//     equals the list in code. It too exists as two instances, and is
//     checked by enumeration, not by eye.
//
//  3. T019 preflight counters and migration 000041 per-class totals agree
//     on the same data, and the migration itself behaves on each class as
//     promised: three invisibility classes are deleted, enabled-reason
//     history is untouched together with read state and archive
//     (SC-014/021/022, INV-12).
//
// The third check repeats the production sequence literally: preflight
// executes on the schema BEFORE the wave (version 38), migrations are
// applied after it — as in the deploy window, where the gate passes
// before the new image starts.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
)

// corpusPath is the SHARED corpus: the same file internal/notifications reads.
const corpusPath = "../internal/notifications/testdata/reason_cases.json"

// preflightPath is the T019 preflight query. It lives under migrations/ (and so
// inside the image) precisely so this test can execute the operator's actual
// file instead of a copy of its text.
const preflightPath = "preflight/sk-303.13-preflight-notification-history.sql"

type sqlCorpusCase struct {
	ID    string `json:"id"`
	Event struct {
		Type             string   `json:"type"`
		ChangedFields    []string `json:"changedFields"`
		ExplicitAccept   bool     `json:"explicitAccept"`
		StaleLockRelease bool     `json:"staleLockRelease"`
		// OutcomeChanged — the sk-489 fact that arrived with the thirteenth parameter
		// of revision 3. It is read FROM THE SAME corpus file: case R-7, in which
		// this field would be lost on parse, would check an ordinary close under the
		// name of a close with an outcome change and would be green for the
		// wrong reason.
		OutcomeChanged bool `json:"outcomeChanged"`
	} `json:"event"`
	Recipient struct {
		Subscriber    bool `json:"subscriber"`
		NewAssignee   bool `json:"newAssignee"`
		Assignee      bool `json:"assignee"`
		Creator       bool `json:"creator"`
		Actor         bool `json:"actor"`
		Mentioned     bool `json:"mentioned"`
		ReminderOwner bool `json:"reminderOwner"`
		// DigestRecipient — the relation introduced by sk-303.14 together with
		// the function's twelfth parameter. Read FROM THE SAME corpus file:
		// a digest case whose field was lost at parse would check "not the
		// addressee" under the addressee's name and be green for the wrong
		// reason.
		DigestRecipient bool `json:"digestRecipient"`
	} `json:"recipient"`
	Expect struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	} `json:"expect"`
}

func loadSQLCorpus(t *testing.T) []sqlCorpusCase {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("reading corpus %s: %v", corpusPath, err)
	}
	var shell struct {
		Cases []sqlCorpusCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	if len(shell.Cases) == 0 {
		t.Fatal("corpus is empty — the regression would prove nothing")
	}
	return shell.Cases
}

// classifyInSQL calls the migration's projection for one (event, recipient) pair.
func classifyInSQL(t *testing.T, pool *pgxpool.Pool, c sqlCorpusCase) (outcome string, reason *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT (notif_classify($1, $2::text[], $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)).*`,
		c.Event.Type, c.Event.ChangedFields, c.Event.ExplicitAccept, c.Event.StaleLockRelease,
		c.Recipient.Subscriber, c.Recipient.NewAssignee, c.Recipient.Assignee,
		c.Recipient.Creator, c.Recipient.Actor, c.Recipient.Mentioned,
		c.Recipient.ReminderOwner, c.Recipient.DigestRecipient,
		c.Event.OutcomeChanged,
	).Scan(&outcome, &reason); err != nil {
		t.Fatalf("notif_classify(%s): %v", c.ID, err)
	}
	return outcome, reason
}

// TestMigration000041_SQLProjectionMatchesRuntimeCorpus — assertion 1.
func TestMigration000041_SQLProjectionMatchesRuntimeCorpus(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)

	for _, c := range loadSQLCorpus(t) {
		t.Run(c.ID, func(t *testing.T) {
			want := notifications.Classify(
				notifications.EventFacts{
					Type:             c.Event.Type,
					ChangedFields:    c.Event.ChangedFields,
					ExplicitAccept:   c.Event.ExplicitAccept,
					StaleLockRelease: c.Event.StaleLockRelease,
					OutcomeChanged:   c.Event.OutcomeChanged,
				},
				notifications.RecipientRelation{
					IsSubscriber:      c.Recipient.Subscriber,
					IsNewAssignee:     c.Recipient.NewAssignee,
					IsAssignee:        c.Recipient.Assignee,
					IsCreator:         c.Recipient.Creator,
					IsActor:           c.Recipient.Actor,
					IsMentioned:       c.Recipient.Mentioned,
					IsReminderOwner:   c.Recipient.ReminderOwner,
					IsDigestRecipient: c.Recipient.DigestRecipient,
				})

			// The runtime must match the corpus expectation — otherwise
			// there would be nothing to compare SQL against.
			if string(want.Outcome) != c.Expect.Outcome {
				t.Fatalf("runtime outcome = %q, corpus expects %q", want.Outcome, c.Expect.Outcome)
			}

			gotOutcome, gotReason := classifyInSQL(t, pool, c)
			if gotOutcome != string(want.Outcome) {
				t.Errorf("SQL outcome = %q, runtime = %q", gotOutcome, want.Outcome)
			}
			wantReason := string(want.Reason)
			got := ""
			if gotReason != nil {
				got = *gotReason
			}
			if got != wantReason {
				t.Errorf("SQL reason = %q, runtime = %q", got, wantReason)
			}
		})
	}
}

// TestMigration000041_AuditOnlySetMatchesRuntime — assertion 2.
func TestMigration000041_AuditOnlySetMatchesRuntime(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()

	fromCode := notifications.AuditOnlyEventTypes()
	if len(fromCode) == 0 {
		t.Fatal("the runtime audit-only set is empty — nothing to compare")
	}

	// A recipient with ALL relations at once: if SQL considers the type
	// journal-only, it must answer audit_only regardless of who the
	// recipient is.
	for _, evType := range fromCode {
		var outcome string
		var reason *string
		if err := pool.QueryRow(ctx, `
SELECT (notif_classify($1, '{assignee,priority}'::text[], true, true,
                       true, true, true, true, true, true, true, true, true)).*`, evType).
			Scan(&outcome, &reason); err != nil {
			t.Fatalf("notif_classify(%s): %v", evType, err)
		}
		if outcome != string(notifications.OutcomeAuditOnly) {
			t.Errorf("SQL calls %q %q, the runtime registry calls it audit-only", evType, outcome)
		}
	}

	// And conversely: a type that is NOT in the journal list IN CODE has
	// no right to become journal-only in SQL on its own initiative.
	// "Not audit_only" cannot be asserted here — an edit without
	// significant fields, an implicit accept and a manual unclaim are
	// journal-only LEGITIMATELY, by the event's content, not by the type
	// list. So what is compared is not the outcome against a constant,
	// but the SQL outcome against the runtime outcome on the same inputs.
	seen := map[string]bool{}
	for _, c := range loadSQLCorpus(t) {
		if seen[c.Event.Type] || notifications.IsAuditOnlyEventType(c.Event.Type) {
			continue
		}
		seen[c.Event.Type] = true

		facts := notifications.EventFacts{Type: c.Event.Type}
		rel := notifications.RecipientRelation{IsSubscriber: true}
		want := notifications.Classify(facts, rel)

		var outcome string
		var reason *string
		if err := pool.QueryRow(ctx, `
SELECT (notif_classify($1, '{}'::text[], false, false,
                       true, false, false, false, false, false, false, false, false)).*`, c.Event.Type).
			Scan(&outcome, &reason); err != nil {
			t.Fatalf("notif_classify(%s): %v", c.Event.Type, err)
		}
		if outcome != string(want.Outcome) {
			t.Errorf("bare subscriber on %q: SQL outcome = %q, runtime = %q",
				c.Event.Type, outcome, want.Outcome)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no delivery-path type came out of the corpus — the reverse check is inert")
	}
}

// preflightRow is one line of the T019 class table.
type preflightRow struct {
	Records  int64
	Accounts int64
	ToDelete int64
}

// preflightClassCounts runs the operator's T019 query — the actual file, not a
// copy of its text — and returns its class table.
func preflightClassCounts(t *testing.T, pool *pgxpool.Pool) map[string]preflightRow {
	t.Helper()
	raw, err := os.ReadFile(preflightPath)
	if err != nil {
		t.Fatalf("reading preflight %s: %v", preflightPath, err)
	}
	// psql meta-commands and comment lines are stripped: pgx executes plain SQL.
	var body strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, `\`) {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}

	out := map[string]preflightRow{}
	for _, stmt := range strings.Split(body.String(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		rows, err := pool.Query(context.Background(), stmt)
		if err != nil {
			t.Fatalf("preflight statement failed: %v", err)
		}
		fields := rows.FieldDescriptions()
		isClassTable := len(fields) == 4 && string(fields[0].Name) == "class"
		for rows.Next() {
			if !isClassTable {
				continue
			}
			var class string
			var records, accounts, toDelete int64
			if err := rows.Scan(&class, &records, &accounts, &toDelete); err != nil {
				rows.Close()
				t.Fatalf("scan preflight row: %v", err)
			}
			out[class] = preflightRow{Records: records, Accounts: accounts, ToDelete: toDelete}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("preflight rows: %v", err)
		}
	}
	if len(out) == 0 {
		t.Fatal("preflight returned no class rows — the fixture or the query is inert")
	}
	return out
}

// TestMigration000041_ClassesMatchPreflight — assertion 3.
func TestMigration000041_ClassesMatchPreflight(t *testing.T) {
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

	// Schema BEFORE the wave — exactly the one preflight executes on in the window.
	if err := m.Migrate(38); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(38): %v", err)
	}
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	assignee := seedUser(t, pool, "w41a")
	watcher := seedUser(t, pool, "w41b")
	hidden := seedUser(t, pool, "w41c")
	pid, prefix := seedProject(t, pool, watcher, "w41")
	issueID := prefix + "-1"
	var assigneeName string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, assignee).
		Scan(&assigneeName); err != nil {
		t.Fatalf("read assignee handle: %v", err)
	}
	seedIssue(t, pool, issueID, pid, watcher, assigneeName)

	// Previous prefs of the hiding user: a whitelist without comment.created.
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, notify_event_types, default_inbox_scope)
VALUES ($1, ARRAY['issue.created'], 'all')
ON CONFLICT (user_id) DO UPDATE SET notify_event_types = EXCLUDED.notify_event_types`,
		hidden); err != nil {
		t.Fatalf("seed legacy whitelist: %v", err)
	}

	// Event + addressed notification. newData/payload are the same shapes the
	// runtime writes, otherwise reconstructing recipient facts would be
	// checked against a fiction.
	newEvent := func(evType, entityType, entityID, newData, payload string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO events (project_id, entity_type, entity_id, type, actor, actor_type, new_data, payload)
VALUES ($1, $2, $3, $4, 'seed', 'human', $5::jsonb, $6::jsonb) RETURNING id`,
			pid, entityType, entityID, evType, newData, payload).Scan(&id); err != nil {
			t.Fatalf("seed event %s: %v", evType, err)
		}
		return id
	}
	notify := func(user uuid.UUID, eventID int64, evType string) {
		if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id) VALUES ($1, $2, $3, $4)`,
			user, eventID, evType, pid); err != nil {
			t.Fatalf("seed notification %s: %v", evType, err)
		}
	}

	assigneeJSON := `{"assignee":` + strconv.Quote(assigneeName) + `}`
	createdEv := newEvent("issue.created", "issue", issueID, assigneeJSON, `{}`)
	updPriority := newEvent("issue.updated", "issue", issueID, `null`,
		`{"changes":[{"field":"priority","oldValue":2,"newValue":1}]}`)
	updAssignee := newEvent("issue.updated", "issue", issueID, `null`,
		`{"changes":[{"field":"assignee","oldValue":null,"newValue":`+strconv.Quote(assigneeName)+`}]}`)
	statusEv := newEvent("issue.status_changed", "issue", issueID, `null`, `{}`)
	commentEv := newEvent("comment.created", "comment", uuid.Must(uuid.NewRandom()).String(), `null`, `{}`)
	attachEv := newEvent("attachment.added", "attachment", uuid.Must(uuid.NewRandom()).String(), `null`, `{}`)
	reparentEv := newEvent("issue.reparented", "issue", issueID, `null`, `{}`)
	unclaimEv := newEvent("issue.unclaimed", "issue", issueID, `null`, `{}`)
	mentionEv := newEvent("comment.mention", "comment", uuid.Must(uuid.NewRandom()).String(), `null`, `{}`)
	reminderEv := newEvent("reminder.rang", "issue", issueID, `null`, `{}`)
	closedEv := newEvent("issue.closed", "issue", issueID, `null`, `{}`)

	// What must survive and what must disappear. Key is (user, event).
	type row struct {
		name    string
		user    uuid.UUID
		event   int64
		evType  string
		survive bool
		reason  string
	}
	rows := []row{
		{"assignment / new assignee", assignee, createdEv, "issue.created", true, "assignment"},
		{"issue_created / subscriber", watcher, createdEv, "issue.created", false, ""},
		{"assignment / updated assignee", watcher, updAssignee, "issue.updated", true, "assignment"},
		{"other_edits / priority", watcher, updPriority, "issue.updated", false, ""},
		{"delegated_status", watcher, statusEv, "issue.status_changed", true, "delegated_status"},
		{"comment", watcher, commentEv, "comment.created", false, ""},
		{"attachment", watcher, attachEv, "attachment.added", false, ""},
		{"reparent", watcher, reparentEv, "issue.reparented", false, ""},
		{"auto_unclaim", watcher, unclaimEv, "issue.unclaimed", false, ""},
		{"mention", watcher, mentionEv, "comment.mention", true, "mention"},
		{"reminder / feed inapplicable", watcher, reminderEv, "reminder.rang", false, ""},
		{"no reason at all", watcher, closedEv, "issue.closed", false, ""},
		{"legacy hidden by whitelist", hidden, commentEv, "comment.created", false, ""},
	}
	for _, r := range rows {
		notify(r.user, r.event, r.evType)
	}

	// Read state and archive on a kept row: SC-021 promises not only
	// visibility but the previous state of these two fields.
	if _, err := pool.Exec(ctx, `
UPDATE notifications SET read_at = now(), archived_at = now()
 WHERE user_id = $1 AND event_id = $2`, watcher, statusEv); err != nil {
		t.Fatalf("mark read/archived: %v", err)
	}

	// Preflight — on schema 38, as in the window.
	pre := preflightClassCounts(t, pool)

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up: %v", err)
	}

	counters := readMigrationCounters(t, pool, "000041_classify_prune_notification_history")
	for class, metric := range map[string]string{
		"1 legacy_hidden":                        "deleted_legacy_hidden",
		"2 new_default_off / default_off_reason": "deleted_default_off_reason",
		"2 new_default_off / no_reason":          "deleted_no_reason",
		"3 feed_inapplicable":                    "deleted_feed_inapplicable",
		"0 kept_enabled_reason":                  "kept_enabled_reason",
		"TOTAL":                                  "history_rows_classified",
	} {
		want, ok := pre[class]
		if !ok {
			t.Errorf("preflight did not report class %q at all", class)
			continue
		}
		got, ok := counters[metric]
		if !ok {
			t.Errorf("migration counter %q is missing", metric)
			continue
		}
		if got != want.Records {
			t.Errorf("class %q: preflight = %d, migration counter %s = %d",
				class, want.Records, metric, got)
		}
	}

	// Each class by name: what was deleted, what was kept and with which reason.
	for _, r := range rows {
		var reason *string
		var readAt, archivedAt *string
		err := pool.QueryRow(ctx, `
SELECT reason, read_at::text, archived_at::text
  FROM notifications WHERE user_id = $1 AND event_id = $2`, r.user, r.event).
			Scan(&reason, &readAt, &archivedAt)
		if !r.survive {
			if err == nil {
				got := "NULL"
				if reason != nil {
					got = *reason
				}
				t.Errorf("%s: row survived with reason %s, want it pruned", r.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: row is gone (%v), want it kept as %q", r.name, err, r.reason)
			continue
		}
		if reason == nil || *reason != r.reason {
			t.Errorf("%s: reason = %v, want %q", r.name, reason, r.reason)
		}
		if r.event == statusEv && r.user == watcher {
			if readAt == nil || archivedAt == nil {
				t.Errorf("%s: read/archive state lost (read=%v archived=%v)", r.name, readAt, archivedAt)
			}
		}
	}

	// And the general invariant: not a single row of the invisible classes
	// remains — not only among the fixture.
	var leftover int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM notifications n
  LEFT JOIN user_notification_prefs p ON p.user_id = n.user_id
 WHERE n.reason IS NULL
    OR n.reason IN ('reminder','issue_created','comment','attachment','reparent','auto_unclaim','other_edits')
    OR (p.notify_event_types IS NOT NULL AND NOT (n.type = ANY (p.notify_event_types)))`).
		Scan(&leftover); err != nil {
		t.Fatalf("leftover probe: %v", err)
	}
	if leftover != 0 {
		t.Errorf("%d notification(s) of the invisible classes survived", leftover)
	}

	// Classes must be disjoint: the sum of the four deleted plus the kept
	// equals the number of classified rows.
	sum := counters["deleted_legacy_hidden"] + counters["deleted_default_off_reason"] +
		counters["deleted_no_reason"] + counters["deleted_feed_inapplicable"]
	if sum != counters["deleted_total"] {
		t.Errorf("deleted classes sum to %d, deleted_total = %d", sum, counters["deleted_total"])
	}
	if sum+counters["kept_enabled_reason"] != counters["history_rows_classified"] {
		t.Errorf("classes do not partition the history: %d deleted + %d kept <> %d classified",
			sum, counters["kept_enabled_reason"], counters["history_rows_classified"])
	}
}

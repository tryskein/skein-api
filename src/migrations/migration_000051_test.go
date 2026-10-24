package migrations

// migration_000051_test.go — sk-489, task T010, coverage matrix row 39.
//
// What is checked here and what is NOT.
//
// Parity of the three classification implementations on a shared corpus is not here: SQL
// against the runtime lives in migration_000041_test.go (the file is named after the migration
// that introduced the function), the projection of the delivery tract in
// internal/outbox/push_classification_parity_test.go. The equality of the CHECK and
// the code registry is in migrations_test.go. Duplicating them would give two
// places that must turn red at the same time.
//
// Here is what none of those checks sees: the migration carries THREE steps
// of different nature (CHECK, data, a schema object), and it is reversible as a whole too.
// The pattern is migration_000045_test.go; its limit is that 000045 did not
// touch a single data row, so the backfill and its rollback are checked
// here by separate assertions, not by the pattern.
//
// ⚠ WHY THE BACKFILL IS CHECKED ON A FRESH ACCOUNT AND NOT ON "ANY" ONE.
// The settings row of a new reason can appear in two ways: the
// migration writes it and the application code writes it on the first settings edit. For
// the assertion to be about the MIGRATION, the account is seeded at version 50 — that is,
// when the reason is in neither the CHECK nor the SQL registry — and only then
// is 000051 rolled forward.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// classifyV3 calls REDACTION 3 with its thirteenth argument. Used only where
// thirteen arguments are expected to resolve.
func classifyV3(t *testing.T, ctx context.Context, eventType string, creator, subscriber, outcomeChanged bool) (string, *string) {
	t.Helper()
	var outcome string
	var reason *string
	if err := testPool(t).QueryRow(ctx, `
SELECT (notif_classify($1, '{}'::text[], false, false,
                       $2, false, false, $3, false, false, false, false, $4)).*`,
		eventType, subscriber, creator, outcomeChanged).Scan(&outcome, &reason); err != nil {
		t.Fatalf("notif_classify(%s, creator=%v, subscriber=%v, outcomeChanged=%v): %v",
			eventType, creator, subscriber, outcomeChanged, err)
	}
	return outcome, reason
}

// reasonCheckAccepts reports whether the column CHECK lets the value in. The
// probe writes and rolls back: reading pg_get_constraintdef would tell us what
// the text SAYS, this tells us what the database DOES.
func reasonCheckAccepts(t *testing.T, ctx context.Context, userID uuid.UUID, reason string) bool {
	t.Helper()
	tx, err := testPool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, true, true)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = excluded.feed_enabled`,
		userID, reason)
	if err == nil {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" /* check_violation */ ||
		pgErr.ConstraintName != "user_notification_reason_prefs_reason_check" {
		t.Fatalf("probing the reason CHECK with %q failed for an unrelated reason: %v", reason, err)
	}
	return false
}

// TestMigration000051_UpDownUp — row 39: three steps up, three steps down and
// up again, each with its own observable consequence.
func TestMigration000051_UpDownUp(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()
	pool := testPool(t)

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// The return to the head is registered BEFORE the first step down: a failure of any
	// assertion below would otherwise leave the shared base at version 50 and break
	// the fixtures of the next packages of the sequential run.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
	}()

	// ---- state BEFORE the roll-forward: the reason is in neither the CHECK nor the classifier ----
	if err := m.Migrate(50); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(50) (000051 down): %v", err)
	}

	// An account that existed BEFORE the deploy — the backfill is checked on it.
	before := seedUser(t, pool, "m51a")

	if reasonCheckAccepts(t, ctx, before, "outcome_changed") {
		t.Error("before the roll-forward the CHECK accepted the outcome_changed reason — the migration would have had nothing to widen")
	}
	// Control of the opposite side: the CHECK works at all and does not "accept everything".
	if !reasonCheckAccepts(t, ctx, before, "delegated_status") {
		t.Error("before the roll-forward the CHECK rejected the existing reason delegated_status — the probe itself is broken")
	}
	if n := classifyParamCount(t, ctx); n != 12 {
		t.Errorf("before the roll-forward notif_classify has %d parameters, want 12 (revision 2)", n)
	}

	// ---- roll-forward ----
	if err := m.Migrate(51); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(51): %v", err)
	}

	if !reasonCheckAccepts(t, ctx, before, "outcome_changed") {
		t.Error("after the roll-forward the CHECK still rejects outcome_changed")
	}

	// Backfill: EVERY account has a row, both channels are enabled, and the number
	// of rows equals the number of accounts. It is checked by a query, not by the migration's
	// counter: the counter is what the migration SAID about itself.
	var feed, push bool
	if err := pool.QueryRow(ctx, `
SELECT feed_enabled, push_enabled
  FROM user_notification_reason_prefs
 WHERE user_id = $1 AND reason = 'outcome_changed'`, before).Scan(&feed, &push); err != nil {
		t.Fatalf("after the roll-forward the account has no outcome_changed settings row: %v", err)
	}
	if !feed || !push {
		t.Errorf("the backfill gave feed=%v push=%v, want enabled on both channels", feed, push)
	}

	var seeded, accounts int64
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM user_notification_reason_prefs WHERE reason = 'outcome_changed'),
       (SELECT count(*) FROM users)`).Scan(&seeded, &accounts); err != nil {
		t.Fatalf("control counts: %v", err)
	}
	if seeded != accounts {
		t.Errorf("outcome_changed rows %d, accounts %d — the backfill must cover everyone", seeded, accounts)
	}

	// The migration's counter is a separate assertion: it agrees with the independent
	// query above and does not replace it.
	counters := readMigrationCounters(t, pool, "000051_outcome_changed_reason")
	if got, ok := counters["outcome_changed_rows_written"]; !ok {
		t.Error("the migration left no outcome_changed_rows_written counter")
	} else if got != accounts {
		t.Errorf("counter outcome_changed_rows_written = %d, independent query = %d", got, accounts)
	}

	// Revision 3 and its rule R-7 — one assertion per branch.
	if n := classifyParamCount(t, ctx); n != 13 {
		t.Errorf("after the roll-forward notif_classify has %d parameters, want 13 (revision 3)", n)
	}
	if outcome, reason := classifyV3(t, ctx, "issue.outcome_changed", true, false, false); outcome != "reason" ||
		reason == nil || *reason != "outcome_changed" {
		t.Errorf("an outcome change for the author gave (%s, %v), want (reason, outcome_changed)", outcome, reason)
	}
	if outcome, _ := classifyV3(t, ctx, "issue.outcome_changed", false, true, false); outcome != "no_reason_for_recipient" {
		t.Errorf("an outcome change for a subscriber who is not the author gave %s, want no_reason_for_recipient", outcome)
	}
	// R-7 in its sharpest form: the author is SUBSCRIBED to their own issue. An implementation
	// that checked "not the author" instead of "the author beats the subscriber" would have returned
	// a status row to them.
	if outcome, _ := classifyV3(t, ctx, "issue.status_changed", true, true, true); outcome != "no_reason_for_recipient" {
		t.Errorf("a close with an outcome change for the author-subscriber gave %s, want no_reason_for_recipient", outcome)
	}
	if outcome, reason := classifyV3(t, ctx, "issue.status_changed", false, true, true); outcome != "reason" ||
		reason == nil || *reason != "delegated_status" {
		t.Errorf("a close with an outcome change for a subscriber who is not the author gave (%s, %v), want (reason, delegated_status)",
			outcome, reason)
	}
	// And without the outcome-change fact the author still gets a status row —
	// otherwise the branch would suppress all closes.
	if outcome, reason := classifyV3(t, ctx, "issue.status_changed", true, true, false); outcome != "reason" ||
		reason == nil || *reason != "delegated_status" {
		t.Errorf("an ordinary close for the author-subscriber gave (%s, %v), want (reason, delegated_status)",
			outcome, reason)
	}

	// ---- rollback ----
	if err := m.Migrate(50); err != nil {
		t.Fatalf("Migrate(50) (000051 down) on a database with data: %v", err)
	}

	var left int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM user_notification_reason_prefs WHERE reason = 'outcome_changed'`).Scan(&left); err != nil {
		t.Fatalf("count after down: %v", err)
	}
	if left != 0 {
		t.Errorf("after the rollback %d outcome_changed rows remain — the rollback must remove them before narrowing the CHECK", left)
	}
	if reasonCheckAccepts(t, ctx, before, "outcome_changed") {
		t.Error("after the rollback the CHECK still accepts outcome_changed")
	}
	if n := classifyParamCount(t, ctx); n != 12 {
		t.Errorf("after the rollback notif_classify has %d parameters, want 12 (return to revision 2)", n)
	}
	// Revision 2 is restored in behaviour, not only in
	// the number of parameters: a type that only revision 3 knows is unmapped again.
	if outcome, _ := classifyDigestV2(t, ctx, "issue.outcome_changed", false); outcome != "unmapped" {
		t.Errorf("after the rollback an outcome change gave %s, want unmapped: revision 2 does not know this type", outcome)
	}

	// ---- repeated roll-forward ----
	if err := m.Migrate(51); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(51) after down: %v", err)
	}
	if n := classifyParamCount(t, ctx); n != 13 {
		t.Errorf("after the repeated roll-forward %d parameters, want 13", n)
	}
	var again int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM user_notification_reason_prefs WHERE reason = 'outcome_changed'`).Scan(&again); err != nil {
		t.Fatalf("count after re-up: %v", err)
	}
	if again != accounts {
		t.Errorf("after the repeated roll-forward %d rows, %d accounts — the backfill is not idempotent", again, accounts)
	}
}

// classifyParamCount returns the parameter count of the single notif_classify
// instance, failing if there is not exactly one (the overload hazard).
func classifyParamCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	sigs := classifySignatures(t, ctx)
	if len(sigs) != 1 {
		t.Fatalf("notif_classify instances %d, want one: %v", len(sigs), sigs)
	}
	return strings.Count(sigs[0], ",") + 1
}

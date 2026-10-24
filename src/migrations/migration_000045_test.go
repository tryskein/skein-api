package migrations

// migration_000045_test.go — sk-303.14, task T005.
//
// What is checked here and what is NOT here.
//
// Parity of the three classification implementations on the shared corpus
// is not here: SQL vs runtime lives in migration_000041_test.go (the file
// is named after the migration that introduced the function), the delivery
// path projection lives in
// internal/outbox/push_classification_parity_test.go. Duplicating them
// would give two places that must go red at the same time, and sooner or
// later one of them would be weakened.
//
// Here is what none of those checks sees: the migration REPLACES a schema
// object, and replaces it in both directions. The forward run must leave
// exactly one function instance with twelve parameters (adding a parameter
// is DROP + CREATE; CREATE OR REPLACE would create an OVERLOAD, and the
// two algorithm instances would diverge silently, while an eleven-argument
// call would keep working and look healthy). The reverse must return the
// 000041 revision, not "roughly it": a rollback that leaves the schema in
// a state that existed neither before nor after is worse than no rollback.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// classifySignatures returns the argument-type list of every function named
// notif_classify. More than one row is the overload hazard the migration
// exists to avoid.
func classifySignatures(t *testing.T, ctx context.Context) []string {
	t.Helper()
	pool := testPool(t)
	rows, err := pool.Query(ctx, `
SELECT pg_get_function_identity_arguments(p.oid)
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE p.proname = 'notif_classify' AND n.nspname = 'public'
 ORDER BY 1`)
	if err != nil {
		t.Fatalf("reading notif_classify signatures: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			t.Fatalf("scan signature: %v", err)
		}
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("signature rows: %v", err)
	}
	return out
}

// classifyDigestV2 calls REDACTION 2 with its twelfth argument. It is used
// only where twelve arguments are expected to resolve — that is, with the
// chain pinned at version 45, never on the head of the tree.
func classifyDigestV2(t *testing.T, ctx context.Context, eventType string, digestRecipient bool) (string, *string) {
	t.Helper()
	var outcome string
	var reason *string
	if err := testPool(t).QueryRow(ctx, `
SELECT (notif_classify($1, '{}'::text[], false, false,
                       false, false, false, false, false, false, false, $2)).*`,
		eventType, digestRecipient).Scan(&outcome, &reason); err != nil {
		t.Fatalf("notif_classify(%s, digest=%v): %v", eventType, digestRecipient, err)
	}
	return outcome, reason
}

// TestMigration000045_ReplacesTheProjectionInBothDirections — the forward
// and reverse run change the function REVISION, they do not add a second.
func TestMigration000045_ReplacesTheProjectionInBothDirections(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()

	// At version 45: exactly one instance, twelve parameters, the twelfth is
	// the "digest addressee".
	//
	// ⚠ WHY THE CHAIN IS PINNED AT 45 AND NOT READ FROM THE HEAD. The test
	// asserts about REVISION 2, and the chain head no longer carries it: 000051
	// (sk-489) replaced the function with revision 3 that has a thirteenth
	// parameter. Leave the head here and the assertion "twelve parameters" would
	// become an assertion about the next migration, not this one: it would turn
	// red on every future revision and say nothing about the one this file
	// checks. So both measurement points are pinned to 45, and the deferred
	// m.Up() returns the base to the head — the next package of the sequential
	// run needs the full schema.
	assertHeadState := func(stage string) {
		t.Helper()
		sigs := classifySignatures(t, ctx)
		if len(sigs) != 1 {
			t.Fatalf("%s: notif_classify instances %d, want one: %v", stage, len(sigs), sigs)
		}
		if n := strings.Count(sigs[0], ",") + 1; n != 12 {
			t.Errorf("%s: parameters %d, want 12: %s", stage, n, sigs[0])
		}
		if !strings.Contains(sigs[0], "p_is_digest_recipient") {
			t.Errorf("%s: signature has no \"digest recipient\" relation: %s", stage, sigs[0])
		}
		// And revision 2 actually knows the sk-303.14 types — otherwise the
		// signature would match but not the behaviour.
		if outcome, reason := classifyDigestV2(t, ctx, "nudge.unaccepted_digest", true); outcome != "reason" ||
			reason == nil || *reason != "unaccepted_reminder" {
			t.Errorf("%s: unaccepted digest gave (%s, %v), want (reason, unaccepted_reminder)",
				stage, outcome, reason)
		}
		if outcome, _ := classifyDigestV2(t, ctx, "review.marked", true); outcome != "audit_only" {
			t.Errorf("%s: private type gave %s, want audit_only", stage, outcome)
		}
	}

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// The return to the head is registered BEFORE the first step down: a failure
	// of any assertion below would otherwise leave the shared base at an
	// intermediate version and break the fixtures of the next sequential-run packages.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	if err := m.Migrate(45); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(45) (revision 2 pin): %v", err)
	}
	assertHeadState("before rollback")

	if err := m.Migrate(44); err != nil {
		t.Fatalf("Migrate(44) (000045 down): %v", err)
	}

	sigs := classifySignatures(t, ctx)
	if len(sigs) != 1 {
		t.Fatalf("after rollback notif_classify instances %d, want one: %v", len(sigs), sigs)
	}
	if n := strings.Count(sigs[0], ",") + 1; n != 11 {
		t.Errorf("after rollback parameters %d, want 11 (000041 revision): %s", n, sigs[0])
	}
	if strings.Contains(sigs[0], "p_is_digest_recipient") {
		t.Errorf("after rollback the twelfth parameter remained: %s", sigs[0])
	}

	// 000041-revision behaviour is restored verbatim: sk-303.14 types are
	// unknown to it, and it honestly says unmapped, it does not stay silent.
	var outcome string
	var reason *string
	if err := testPool(t).QueryRow(ctx, `
SELECT (notif_classify($1, '{}'::text[], false, false,
                       true, false, false, false, false, false, false)).*`,
		"nudge.unaccepted_digest").Scan(&outcome, &reason); err != nil {
		t.Fatalf("11-argument call after rollback: %v", err)
	}
	if outcome != "unmapped" {
		t.Errorf("after rollback the digest gave %s, want unmapped: the 000041 revision does not know these types", outcome)
	}
	// And one of the original types is still classified — rollback returned a
	// working function, not an empty stub.
	if err := testPool(t).QueryRow(ctx, `
SELECT (notif_classify($1, '{}'::text[], false, false,
                       true, false, false, false, false, false, false)).*`,
		"comment.created").Scan(&outcome, &reason); err != nil {
		t.Fatalf("11-argument call (comment.created) after rollback: %v", err)
	}
	if outcome != "reason" || reason == nil || *reason != "comment" {
		t.Errorf("after rollback comment.created gave (%s, %v), want (reason, comment)", outcome, reason)
	}

	if err := m.Migrate(45); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Migrate(45) after down: %v", err)
	}
	assertHeadState("after re-apply")
}

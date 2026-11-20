package main

// post_migration_test.go — the startup step that runs after the migrations
// (integration, TEST_DATABASE_URL). The reference is the transitional
// contract of the device-consent specification: when the new API is
// deployed again after the application was rolled back, over a schema whose
// migration 000092 is already recorded as applied (so it does not run
// again), subscriptions of users deactivated or deleted meanwhile do not get
// deliveries and are removed at the deployment — labelled or not; active
// users' subscriptions and the device revocations stay.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// Row 26 — transitional contract, redeployment after a rollback.
func TestRunPostMigrationMaintenance_RemovesSubscriptionsOfInactiveUsers(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	// The schema the step meets: 000092 recorded as applied, clean — the
	// migration's own cleanup is not going to run again.
	var version int64
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read the migration state: %v", err)
	}
	if version < 92 || dirty {
		t.Fatalf("schema at version %d (dirty=%v), want 000092 or later applied", version, dirty)
	}

	active, _ := testsupport.SeedUser(t, pool, "user")
	deactivated, _ := testsupport.SeedUser(t, pool, "user")
	deleted, _ := testsupport.SeedUser(t, pool, "user")

	// Subscriptions the rolled-back release kept or created: labelled ones
	// and unlabelled ones written by the old registration form.
	kept := []uuid.UUID{
		testsupport.SeedLabeledPushSubscription(t, pool, active, uuid.New()),
		testsupport.SeedPushSubscription(t, pool, active),
	}
	removed := []uuid.UUID{
		testsupport.SeedLabeledPushSubscription(t, pool, deactivated, uuid.New()),
		testsupport.SeedPushSubscription(t, pool, deactivated),
		testsupport.SeedLabeledPushSubscription(t, pool, deleted, uuid.New()),
		testsupport.SeedPushSubscription(t, pool, deleted),
	}
	revokedLabel := uuid.New()
	testsupport.SeedPushDeviceRevocation(t, pool, deactivated, revokedLabel)
	testsupport.SeedPushDeviceRevocation(t, pool, active, revokedLabel)

	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, deactivated); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(random()::text), 1, 8) WHERE id = $1`, deleted); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	exists := func(id uuid.UUID) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count subscription %s: %v", id, err)
		}
		return n == 1
	}
	revocations := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM push_device_revocations WHERE user_id = ANY($1::uuid[]) AND device_label = $2`,
			[]uuid.UUID{deactivated, active}, revokedLabel).Scan(&n); err != nil {
			t.Fatalf("count revocations: %v", err)
		}
		return n
	}
	for _, id := range append(append([]uuid.UUID{}, kept...), removed...) {
		if !exists(id) {
			t.Fatalf("seeded subscription %s is missing before the step — the case proves nothing", id)
		}
	}

	// The second call is the idempotency half: nothing more goes, nothing fails.
	for call := 1; call <= 2; call++ {
		if err := runPostMigrationMaintenance(ctx, pool); err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		for _, id := range removed {
			if exists(id) {
				t.Errorf("call %d: subscription %s of an inactive user survived", call, id)
			}
		}
		for _, id := range kept {
			if !exists(id) {
				t.Errorf("call %d: subscription %s of an active user was removed", call, id)
			}
		}
		if got := revocations(); got != 2 {
			t.Errorf("call %d: device revocations = %d, want both kept", call, got)
		}
	}
}

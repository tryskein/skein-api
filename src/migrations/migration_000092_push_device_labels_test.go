package migrations

// migration_000092_push_device_labels_test.go — test matrix row 1 ("Data",
// US-9 "deleted at deployment", transitional contract "schema rollback").
// Reference — the spec, written before the migration:
//
//   - subscriptions created before the migration get an empty device label
//     and version 1; the label lookup index covers only non-empty labels;
//   - the revocation table is unique per (user, label) and goes with the user;
//   - subscriptions of users deactivated or deleted at deployment time are
//     deleted, subscriptions of active users stay;
//   - down drops the columns and the revocation table and keeps the remaining
//     subscriptions; up after down restores the schema.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// m92NewColumns are the push_subscriptions columns 000092 adds.
var m92NewColumns = []string{"device_label", "version"}

const (
	m92RevocationsTable = "push_device_revocations"
	m92LabelIndex       = "idx_push_subscriptions_user_label"
)

// seedM92Subscription writes a subscription in the form every writer before
// 000092 uses and returns its id.
func seedM92Subscription(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	m59Exec(t, pool, "seed subscription", `
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth)
VALUES ($1, $2, $3, 'p256dh', 'auth')`, id, owner, "https://push.m92.test/"+id.String())
	return id
}

func m92SubscriptionCount(t *testing.T, pool *pgxpool.Pool, stage string, ids ...uuid.UUID) int {
	t.Helper()
	return m59Count(t, pool, stage, `SELECT count(*) FROM push_subscriptions WHERE id = ANY($1)`, ids)
}

// assertM92Unlabelled checks that a subscription carries no label and
// version 1.
func assertM92Unlabelled(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, stage string) {
	t.Helper()
	var label *uuid.UUID
	var version int64
	if err := pool.QueryRow(context.Background(), `
SELECT device_label, version FROM push_subscriptions WHERE id = $1`, id).Scan(&label, &version); err != nil {
		t.Fatalf("%s: read subscription %s: %v", stage, id, err)
	}
	if label != nil {
		t.Errorf("%s: subscription %s device_label = %s, want NULL", stage, id, *label)
	}
	if version != 1 {
		t.Errorf("%s: subscription %s version = %d, want 1", stage, id, version)
	}
}

// m92Index is the label lookup index as the catalogue describes it.
type m92Index struct {
	table     string
	columns   []string
	unique    bool
	predicate *string
}

func m92ReadIndex(t *testing.T, pool *pgxpool.Pool, stage string) m92Index {
	t.Helper()
	var ix m92Index
	if err := pool.QueryRow(context.Background(), `
SELECT tbl.relname,
       ARRAY(SELECT a.attname::text
               FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
               JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
              ORDER BY k.ord),
       i.indisunique,
       pg_get_expr(i.indpred, i.indrelid)
  FROM pg_index i
  JOIN pg_class idx ON idx.oid = i.indexrelid
  JOIN pg_class tbl ON tbl.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = idx.relnamespace
 WHERE n.nspname = 'public' AND idx.relname = $1`, m92LabelIndex).
		Scan(&ix.table, &ix.columns, &ix.unique, &ix.predicate); err != nil {
		t.Fatalf("%s: read index %s: %v", stage, m92LabelIndex, err)
	}
	return ix
}

func assertM92Schema(t *testing.T, pool *pgxpool.Pool, stage string) {
	t.Helper()
	for _, c := range m92NewColumns {
		if !columnExists(t, pool, "push_subscriptions", c) {
			t.Errorf("%s: push_subscriptions.%s missing", stage, c)
		}
	}
	if !tableExists(t, pool, m92RevocationsTable) {
		t.Errorf("%s: table %s missing", stage, m92RevocationsTable)
	}
	// The plan: a non-unique index on (user_id, device_label) over the rows
	// with a label.
	ix := m92ReadIndex(t, pool, stage)
	if ix.table != "push_subscriptions" {
		t.Errorf("%s: index table = %q, want push_subscriptions", stage, ix.table)
	}
	if len(ix.columns) != 2 || ix.columns[0] != "user_id" || ix.columns[1] != "device_label" {
		t.Errorf("%s: index key = %v, want [user_id device_label]", stage, ix.columns)
	}
	if ix.unique {
		t.Errorf("%s: index is unique, want non-unique", stage)
	}
	if ix.predicate == nil || *ix.predicate != "(device_label IS NOT NULL)" {
		t.Errorf("%s: index predicate = %v, want device_label IS NOT NULL", stage, ix.predicate)
	}
}

func TestMigration000092_PushDeviceLabels(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t)
	if err := m.Migrate(91); err != nil {
		t.Fatalf("Migrate(91): %v", err)
	}

	active := seedUser(t, pool, "m92a")
	deactivated := seedUser(t, pool, "m92d")
	deleted := seedUser(t, pool, "m92x")
	m59Exec(t, pool, "deactivate", `UPDATE users SET deactivated_at = now() WHERE id = $1`, deactivated)
	m59Exec(t, pool, "delete", `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8) WHERE id = $1`, deleted)

	activeSubs := []uuid.UUID{seedM92Subscription(t, pool, active), seedM92Subscription(t, pool, active)}
	deactivatedSub := seedM92Subscription(t, pool, deactivated)
	deletedSub := seedM92Subscription(t, pool, deleted)

	// Checked steps stop at 92; m59Migrator returns to head in t.Cleanup.
	if err := m.Migrate(92); err != nil {
		t.Fatalf("first up: Migrate(92): %v", err)
	}

	t.Run("up: schema, unlabelled version-1 rows, inactive owners lose subscriptions", func(t *testing.T) {
		assertM92Schema(t, pool, "after up")
		if n := m92SubscriptionCount(t, pool, "active after up", activeSubs...); n != len(activeSubs) {
			t.Errorf("after up: active user's subscriptions = %d, want %d", n, len(activeSubs))
		}
		for _, id := range activeSubs {
			assertM92Unlabelled(t, pool, id, "after up")
		}
		if n := m92SubscriptionCount(t, pool, "deactivated after up", deactivatedSub); n != 0 {
			t.Errorf("after up: deactivated user's subscriptions = %d, want 0", n)
		}
		if n := m92SubscriptionCount(t, pool, "deleted after up", deletedSub); n != 0 {
			t.Errorf("after up: deleted user's subscriptions = %d, want 0", n)
		}
	})

	t.Run("up: revocations are unique per user and label and go with the user", func(t *testing.T) {
		label := uuid.New()
		testsupport.SeedPushDeviceRevocation(t, pool, active, label)
		if _, err := pool.Exec(ctx, `
INSERT INTO push_device_revocations (user_id, device_label) VALUES ($1, $2)`, active, label); !isUniqueViolation(err) {
			t.Errorf("second revocation of the same label: err %v, want unique violation", err)
		}
		// The same label of another user is a separate revocation.
		other := seedUser(t, pool, "m92o")
		testsupport.SeedPushDeviceRevocation(t, pool, other, label)

		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, other); err != nil {
			t.Fatalf("delete user row: %v", err)
		}
		if n := m59Count(t, pool, "revocations of a removed user",
			`SELECT count(*) FROM push_device_revocations WHERE user_id = $1`, other); n != 0 {
			t.Errorf("revocations after the users row is deleted = %d, want 0", n)
		}
		if n := m59Count(t, pool, "revocations of the remaining user",
			`SELECT count(*) FROM push_device_revocations WHERE user_id = $1`, active); n != 1 {
			t.Errorf("revocations of the remaining user = %d, want 1", n)
		}
	})

	t.Run("down drops columns, index and table and keeps subscriptions; up after down restores", func(t *testing.T) {
		labelled := testsupport.SeedLabeledPushSubscription(t, pool, active, uuid.New())

		if err := m.Migrate(91); err != nil {
			t.Fatalf("Migrate(91): %v", err)
		}
		for _, c := range m92NewColumns {
			if columnExists(t, pool, "push_subscriptions", c) {
				t.Errorf("after down: push_subscriptions.%s remained", c)
			}
		}
		if tableExists(t, pool, m92RevocationsTable) {
			t.Errorf("after down: table %s remained", m92RevocationsTable)
		}
		if n := m59Count(t, pool, "index after down",
			`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, m92LabelIndex); n != 0 {
			t.Errorf("after down: index %s remained", m92LabelIndex)
		}
		kept := append([]uuid.UUID{labelled}, activeSubs...)
		if n := m92SubscriptionCount(t, pool, "subscriptions after down", kept...); n != len(kept) {
			t.Errorf("after down: active user's subscriptions = %d, want %d", n, len(kept))
		}

		if err := m.Migrate(92); err != nil {
			t.Fatalf("up after down: Migrate(92): %v", err)
		}
		assertM92Schema(t, pool, "after re-up")
		for _, id := range kept {
			assertM92Unlabelled(t, pool, id, "after re-up")
		}
	})
}

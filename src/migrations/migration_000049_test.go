package migrations

// migration_000049_test.go — integration class for migration 000049 (sk-424).
//
// The migration puts the lock's OWNER (issues.locked_by_user_id) next to the lock and
// restores the owners of already existing locks from the journal. It is checked by
// two assertions, and the second matters more than the first:
//
//	1) the forward and backward run is clean (up/down/up) — the column appears, goes away
//	   and appears again;
//	2) the BACKFILL really restores the owner — and does NOT invent one where
//	   there is nothing to restore from.
//
// WHY A REAL STORE AND NOT A HAND-WRITTEN EVENT. The backfill looks for the lock by a
// TRIPLE: an issue.claimed event of this issue whose new_data->>'lockedBy'
// equals the current locked_by and whose occurred_at equals the current locked_at. The match
// on the moment relies on the lock and its event being written in ONE
// transaction, where locked_at = now() and occurred_at DEFAULT now() give one
// stamp. A test that wrote the event by hand would substitute the moment itself and check
// its own arithmetic instead of this property. So the lock here is taken by
// issues.Store.Claim — the same single writer of a non-empty lock as in
// production.
//
// WHY THERE IS A NEGATIVE HALF. The assertion "the owner
// was restored" on its own would pass for a backfill that sets the
// owner for everything — and such a backfill would bring back the defect: a lock with a foreign
// owner lets the wrong person in. So next to it stands a lock WITHOUT a matching
// event: after the roll-forward its owner must stay empty.

import (
	"context"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
)

// readLockOwner returns issues.locked_by_user_id (nil = "owner unknown").
func readLockOwner(t *testing.T, pool *pgxpool.Pool, id string) *uuid.UUID {
	t.Helper()
	var owner *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT locked_by_user_id FROM issues WHERE id = $1`, id).Scan(&owner); err != nil {
		t.Fatalf("read lock owner of %s: %v", id, err)
	}
	return owner
}

func TestMigration000049_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)

	if !columnExists(t, pool, "issues", "locked_by_user_id") {
		t.Fatal("before rollback: issues.locked_by_user_id is absent")
	}

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// The return to the head is registered BEFORE the step down: a failure of any assertion
	// below would otherwise leave the shared base at version 48 and break the fixtures of all
	// following packages of the sequential run.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// An absolute version, not Steps(-1): the appearance of 000050 would make a step back
	// a rollback of SOMEONE ELSE'S migration.
	if err := m.Migrate(48); err != nil {
		t.Fatalf("Migrate(48) (000049 down): %v", err)
	}
	if columnExists(t, pool, "issues", "locked_by_user_id") {
		t.Error("after rollback issues.locked_by_user_id is still there")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	if !columnExists(t, pool, "issues", "locked_by_user_id") {
		t.Error("after the repeated roll-forward issues.locked_by_user_id did not come back")
	}
}

// TestMigration000049_BackfillRestoresOwnerOnlyFromItsOwnClaim — the backfill takes the
// owner from the event of THIS lock and from nowhere else.
func TestMigration000049_BackfillRestoresOwnerOnlyFromItsOwnClaim(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner, ownerName, pid, prefix := storeFixture(t, pool, "m49")
	store := issues.NewStore(pool)

	// (a) a lock through the real path: the lock and its event in one transaction.
	claimed := prefix + "-1"
	seedIssue(t, pool, claimed, pid, owner, "")
	if _, err := store.Claim(t.Context(), issues.ClaimInput{
		ID: claimed, SessionID: "sess-m49", Actor: issues.HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Precondition: the claim writes the owner at all. Without this check "the owner
	// is in place after the roll-forward" would pass even on a column that nobody fills.
	if got := readLockOwner(t, pool, claimed); got == nil || *got != owner {
		t.Fatalf("the claim did not record the owner: got %v, want %v", got, owner)
	}

	// (b) a lock WITHOUT a matching event: there is nothing to restore from.
	orphan := prefix + "-2"
	seedIssue(t, pool, orphan, pid, owner, "")
	if _, err := pool.Exec(context.Background(), `
UPDATE issues SET locked_by = 'sess-m49-orphan', locked_at = now(), status = 'in_progress'
 WHERE id = $1`, orphan); err != nil {
		t.Fatalf("seed orphan lock: %v", err)
	}

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

	// Down — the column and all owners in it disappear; up — the backfill recomputes them
	// from the journal, which is exactly what will happen on deploy.
	if err := m.Migrate(48); err != nil {
		t.Fatalf("Migrate(48): %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}

	if got := readLockOwner(t, pool, claimed); got == nil || *got != owner {
		t.Errorf("the backfill did not restore the lock owner: got %v, want %v", got, owner)
	}
	if got := readLockOwner(t, pool, orphan); got != nil {
		t.Errorf("the backfill invented an owner %v for a lock without an event — such a lock "+
			"must stay \"owner unknown\"", *got)
	}
}

package migrations

// migration_000063_test.go — sk-622.21, T025, found in review: the full scan that validates issue_watchers_no_tombstone
// runs in its own migration, not in 000062's transaction.
//
// Reference — the review finding and the lock levels documented for
// ALTER TABLE: ADD CONSTRAINT … NOT VALID takes ACCESS EXCLUSIVE until the
// end of its transaction, VALIDATE CONSTRAINT alone takes SHARE UPDATE
// EXCLUSIVE. golang-migrate runs one file as one transaction, so the scan
// avoids the strong lock only if the two statements live in different files.
// What the database shows for that:
//
//   - at version 62 the constraint exists and is NOT validated
//     (pg_constraint.convalidated = false) — 000062 did not scan;
//   - at version 63 it is validated;
//   - down 000063 keeps it (validation cannot be undone; the down is a
//     no-op), and down 000062 after it drops it — the R2 → R1 rollback.

import (
	"os"
	"testing"
)

// TestMigration000063_ValidationIsItsOwnMigration — see the file header.
func TestMigration000063_ValidationIsItsOwnMigration(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")

	// From 61 up: a down of 000063 alone would leave the constraint
	// validated (its down is a no-op), so 000062 is re-applied fresh.
	if err := m.Migrate(61); err != nil {
		t.Fatalf("Migrate(61): %v", err)
	}
	if err := m.Migrate(62); err != nil {
		t.Fatalf("Migrate(62) from 61: %v", err)
	}
	if exists, validated := m62ConstraintValidated(t, pool); !exists || validated {
		t.Fatalf("at 62: constraint exists=%v validated=%v, want exists and NOT validated — "+
			"000062 must not scan issue_watchers under its ACCESS EXCLUSIVE", exists, validated)
	}

	if err := m.Migrate(63); err != nil {
		t.Fatalf("Migrate(63): %v", err)
	}
	if exists, validated := m62ConstraintValidated(t, pool); !exists || !validated {
		t.Fatalf("at 63: constraint exists=%v validated=%v, want both true", exists, validated)
	}

	if err := m.Migrate(62); err != nil {
		t.Fatalf("Migrate(62) (000063 down): %v", err)
	}
	if exists, _ := m62ConstraintValidated(t, pool); !exists {
		t.Fatalf("after down 000063 the constraint is gone, want it kept (down 000063 is a no-op)")
	}
	if err := m.Migrate(61); err != nil {
		t.Fatalf("Migrate(61) (000062 down after 000063 down): %v", err)
	}
	if exists, _ := m62ConstraintValidated(t, pool); exists {
		t.Fatalf("after down 000062 the constraint is still there")
	}
}

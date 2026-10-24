package config

// instance_settings_test.go — sk-674.18, test matrix row 8 (D-2, K-2).
//
// Reference: K-2 — the database access function reads the admin flag from
// the single instance_settings row, which skein-api writes at every startup
// from SKEIN_INSTANCE_ADMIN_ENABLED and reads back. The value after startup
// must equal the variable, and a later startup with another value must
// overwrite the row (not keep the first one) and pass its read-back.

import (
	"context"
	"testing"

	"skein-api/internal/testsupport"
)

func storedAdminEnabled(t *testing.T, db testDB) bool {
	t.Helper()
	var enabled bool
	if err := db.pool.QueryRow(context.Background(),
		`SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&enabled); err != nil {
		t.Fatalf("reading instance_settings: %v", err)
	}
	return enabled
}

// TestPrepareDatabase_InstanceSettingsFollowTheVariable is matrix row 8.
// One database, three startups with the flag on, off and on again: after
// each, the stored flag equals the variable of that startup.
func TestPrepareDatabase_InstanceSettingsFollowTheVariable(t *testing.T) {
	db := freshDatabase(t, 0)
	// A plain user, so the admin-off startup is not the "nobody can enter"
	// refusal of an empty user base (INV-21) — this row is about the flag.
	testsupport.SeedUser(t, db.pool, "user")

	for i, adminOn := range []bool{true, false, true} {
		cfg := startupConfig()
		cfg.InstanceAdminEnabled = adminOn
		cfg.CommonSpaceEnabled = false
		if err := prepare(t, db, cfg); err != nil {
			t.Fatalf("startup %d (admin enabled=%v) = %v, want success", i+1, adminOn, err)
		}
		if got := storedAdminEnabled(t, db); got != adminOn {
			t.Fatalf("after startup %d instance_settings.admin_enabled = %v, want %v — the row "+
				"must follow SKEIN_INSTANCE_ADMIN_ENABLED of the latest startup", i+1, got, adminOn)
		}
	}
}

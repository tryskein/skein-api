package apikeys_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
)

// seedUserWithExternal inserts a user row with an explicit is_external flag
// (role is always 'user' — the migration CHECK forbids external+admin) so we
// can attach a key and assert LookupByPrefix carries the flag.
func seedUserWithExternal(t *testing.T, pool *pgxpool.Pool, isExternal bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "akx" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', $5, now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, isExternal); err != nil {
		t.Fatalf("seed user (external=%v): %v", isExternal, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestLookupByPrefix_IsExternal verifies sk-163 S1: the api-key lookup
// SELECTs users.is_external and surfaces it on LookupResult, for both an
// external and an internal owner. This is the cold-path source of the flag
// the MCP surface-gate (later slices) relies on.
func TestLookupByPrefix_IsExternal(t *testing.T) {
	pool := newTestPool(t)
	store := apikeys.New(pool)
	ctx := context.Background()

	mint := func(userID uuid.UUID) string {
		_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if _, _, err := store.Create(ctx, userID, "ext-lookup-test", prefix, hash); err != nil {
			t.Fatalf("Create: %v", err)
		}
		return prefix
	}

	extPrefix := mint(seedUserWithExternal(t, pool, true))
	intPrefix := mint(seedUserWithExternal(t, pool, false))

	extRes, err := store.LookupByPrefix(ctx, extPrefix)
	if err != nil {
		t.Fatalf("LookupByPrefix (external): %v", err)
	}
	if !extRes.IsExternal {
		t.Errorf("external owner: LookupResult.IsExternal = false, want true")
	}

	intRes, err := store.LookupByPrefix(ctx, intPrefix)
	if err != nil {
		t.Fatalf("LookupByPrefix (internal): %v", err)
	}
	if intRes.IsExternal {
		t.Errorf("internal owner: LookupResult.IsExternal = true, want false")
	}
}

package apikeys_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
)

const testDatabaseURL = "TEST_DATABASE_URL"

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUserDirect inserts a user row (active or deactivated) so we can
// attach an api key to them without going through users.Store. Cleanup
// via t.Cleanup.
func seedUserDirect(t *testing.T, pool *pgxpool.Pool, deactivated bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ak" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata, deactivated_at)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb,
        CASE WHEN $5 THEN now() ELSE NULL END)`,
		id, uname+"@test.local", uname, uname, deactivated); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestLookupByPrefix_DeactivatedUser_NotFound verifies the R3.1 W2 fix:
// even if an API key still has revoked_at IS NULL (e.g. a missed cascade
// in some hypothetical future flow), a key belonging to a deactivated
// user must NOT authenticate. The defence-in-depth filter
// `AND u.deactivated_at IS NULL` on the LookupByPrefix join guarantees
// this regardless of revocation state.
func TestLookupByPrefix_DeactivatedUser_NotFound(t *testing.T) {
	pool := newTestPool(t)
	store := apikeys.New(pool)

	// 1. Seed an ACTIVE user, issue a key, confirm lookup works.
	userID := seedUserDirect(t, pool, false)
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, _, err := store.Create(context.Background(), userID, "test", prefix, hash); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Sanity: lookup succeeds while user is active and key is unrevoked.
	if _, err := store.LookupByPrefix(context.Background(), prefix); err != nil {
		t.Fatalf("LookupByPrefix (active user): %v", err)
	}

	// 2. Manually deactivate WITHOUT going through users.Store.Deactivate
	// (which cascades RevokeAllForUser). This simulates the failure mode
	// W2 defends against — an unrevoked key on a deactivated user.
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("manual deactivate: %v", err)
	}

	// 3. Lookup must now fail with ErrNotFound — the JOIN clause filters
	// out the row even though revoked_at is still NULL on the api_keys row.
	_, err = store.LookupByPrefix(context.Background(), prefix)
	if !errors.Is(err, apikeys.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (deactivated user must not authenticate)", err)
	}

	// Confirm the api_keys row itself is still revoked_at = NULL — the
	// filter must work without the key being explicitly revoked.
	var revokedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at FROM api_keys WHERE prefix = $1`, prefix).Scan(&revokedAt); err != nil {
		t.Fatalf("read api_keys row: %v", err)
	}
	if revokedAt != nil {
		t.Fatalf("api_keys.revoked_at = %v, want NULL — test wiring would not exercise the deactivated_at filter otherwise", revokedAt)
	}
}

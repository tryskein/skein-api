package push

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
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

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "psu" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// seededEndpoint reads the endpoint of the subscription a testsupport seed
// wrote.
func seededEndpoint(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var endpoint string
	if err := pool.QueryRow(context.Background(), `SELECT endpoint FROM push_subscriptions WHERE id = $1`, id).Scan(&endpoint); err != nil {
		t.Fatalf("read seeded endpoint: %v", err)
	}
	return endpoint
}

func TestStore_DeleteByEndpointAny(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user := seedUser(t, pool)

	endpoint := seededEndpoint(t, pool, testsupport.SeedPushSubscription(t, pool, user))
	if err := store.DeleteByEndpointAny(ctx, endpoint); err != nil {
		t.Fatalf("delete-any: %v", err)
	}
	if l, _ := store.ListForUser(ctx, user); len(l) != 0 {
		t.Fatalf("expected row removed by delete-any, got %d", len(l))
	}
	// Idempotent on a missing endpoint.
	if err := store.DeleteByEndpointAny(ctx, "https://push.example/missing"); err != nil {
		t.Fatalf("idempotent delete-any: %v", err)
	}
}

func TestStore_CascadeOnUserDelete(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool)
	endpoint := seededEndpoint(t, pool, testsupport.SeedPushSubscription(t, pool, user))

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`,
		endpoint).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected ON DELETE CASCADE to remove subscriptions, got %d", n)
	}
}

package push

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

func TestStore_UpsertInsertsThenList(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user := seedUser(t, pool)

	sub, err := store.Upsert(ctx, user, "https://push.example/ep1", "p256-a", "auth-a", "Firefox/1.0")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if sub.ID == uuid.Nil {
		t.Fatal("expected a generated id")
	}
	if sub.UserID != user || sub.Endpoint != "https://push.example/ep1" {
		t.Fatalf("unexpected stored row: %+v", sub)
	}
	if sub.UA == nil || *sub.UA != "Firefox/1.0" {
		t.Fatalf("expected ua Firefox/1.0, got %v", sub.UA)
	}

	list, err := store.ListForUser(ctx, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Endpoint != "https://push.example/ep1" {
		t.Fatalf("expected 1 subscription, got %+v", list)
	}
}

func TestStore_UpsertDedupesByEndpoint(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	userA := seedUser(t, pool)
	userB := seedUser(t, pool)

	first, err := store.Upsert(ctx, userA, "https://push.example/shared", "p1", "a1", "")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Same endpoint, different user + refreshed keys → re-homes the SAME row.
	second, err := store.Upsert(ctx, userB, "https://push.example/shared", "p2", "a2", "Chrome")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected same row id on endpoint conflict: %s vs %s", first.ID, second.ID)
	}
	if second.UserID != userB || second.P256dh != "p2" || second.Auth != "a2" {
		t.Fatalf("expected re-homed row to userB with refreshed keys, got %+v", second)
	}

	// userA no longer owns it; userB does.
	if l, _ := store.ListForUser(ctx, userA); len(l) != 0 {
		t.Fatalf("expected userA to have 0 subscriptions, got %d", len(l))
	}
	if l, _ := store.ListForUser(ctx, userB); len(l) != 1 {
		t.Fatalf("expected userB to have 1 subscription, got %d", len(l))
	}
}

func TestStore_DeleteByEndpointScopedToOwner(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	userA := seedUser(t, pool)
	userB := seedUser(t, pool)

	if _, err := store.Upsert(ctx, userA, "https://push.example/a", "p", "a", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// userB deleting userA's endpoint is a no-op (scoped to owner).
	if err := store.DeleteByEndpoint(ctx, userB, "https://push.example/a"); err != nil {
		t.Fatalf("delete (wrong owner): %v", err)
	}
	if l, _ := store.ListForUser(ctx, userA); len(l) != 1 {
		t.Fatalf("expected userA row to survive wrong-owner delete, got %d", len(l))
	}

	// userA deleting their own endpoint removes it.
	if err := store.DeleteByEndpoint(ctx, userA, "https://push.example/a"); err != nil {
		t.Fatalf("delete (owner): %v", err)
	}
	if l, _ := store.ListForUser(ctx, userA); len(l) != 0 {
		t.Fatalf("expected userA row removed, got %d", len(l))
	}

	// Idempotent: deleting again is a silent no-op.
	if err := store.DeleteByEndpoint(ctx, userA, "https://push.example/a"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestStore_DeleteByIDForUserScopedToOwner(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	userA := seedUser(t, pool)
	userB := seedUser(t, pool)

	sub, err := store.Upsert(ctx, userA, "https://push.example/byid", "p", "a", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// userB deleting userA's row by id matches nothing → false, row survives.
	deleted, err := store.DeleteByIDForUser(ctx, userB, sub.ID)
	if err != nil {
		t.Fatalf("delete (wrong owner): %v", err)
	}
	if deleted {
		t.Fatal("expected wrong-owner delete-by-id to return false")
	}
	if l, _ := store.ListForUser(ctx, userA); len(l) != 1 {
		t.Fatalf("expected userA row to survive wrong-owner delete, got %d", len(l))
	}

	// userA deleting their own row by id → true, row removed.
	deleted, err = store.DeleteByIDForUser(ctx, userA, sub.ID)
	if err != nil {
		t.Fatalf("delete (owner): %v", err)
	}
	if !deleted {
		t.Fatal("expected owner delete-by-id to return true")
	}
	if l, _ := store.ListForUser(ctx, userA); len(l) != 0 {
		t.Fatalf("expected userA row removed, got %d", len(l))
	}

	// Missing id is a no-op → false (idempotent).
	deleted, err = store.DeleteByIDForUser(ctx, userA, uuid.Must(uuid.NewRandom()))
	if err != nil {
		t.Fatalf("delete (missing id): %v", err)
	}
	if deleted {
		t.Fatal("expected missing-id delete to return false")
	}
}

func TestStore_DeleteByEndpointAny(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user := seedUser(t, pool)

	if _, err := store.Upsert(ctx, user, "https://push.example/gone", "p", "a", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.DeleteByEndpointAny(ctx, "https://push.example/gone"); err != nil {
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
	store := NewStore(pool)
	user := seedUser(t, pool)

	if _, err := store.Upsert(ctx, user, "https://push.example/cascade", "p", "a", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`,
		"https://push.example/cascade").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected ON DELETE CASCADE to remove subscriptions, got %d", n)
	}
}

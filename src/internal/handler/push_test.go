package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
)

func newPushPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
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

func seedPushUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ph" + strings.ReplaceAll(id.String()[:16], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake', now(), '{}'::jsonb)`,
		id, uname+"@t.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// withChiID stuffs the {id} path param into the request context so calling the
// handler directly mirrors what the chi router injects in production.
func withChiID(req *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// List exposes endpointHash (sha256 hex of the endpoint) and NEVER the raw
// endpoint / p256dh / auth.
func TestPush_ListExposesEndpointHashNotEndpoint(t *testing.T) {
	pool := newPushPool(t)
	store := push.NewStore(pool)
	h := NewPushHandler(pool, store, "pub", true)
	user := seedPushUser(t, pool)

	const endpoint = "https://push.example/list-hash"
	if _, err := store.Upsert(context.Background(), user, endpoint, "p256", "auth", "Firefox"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rec := httptest.NewRecorder()
	h.List(rec, authReq(http.MethodGet, "/api/v1/push/subscriptions", nil, user))
	if rec.Code != http.StatusOK {
		t.Fatalf("List status: %d body=%s", rec.Code, rec.Body.String())
	}

	// Lean DTO: no raw endpoint / p256dh / auth anywhere on the wire.
	raw := rec.Body.String()
	for _, leaked := range []string{endpoint, "p256dh", "\"auth\"", "p256\"", "auth\""} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("List response leaked %q: %s", leaked, raw)
		}
	}

	var got []subscriptionDTO
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(got))
	}

	sum := sha256.Sum256([]byte(endpoint))
	want := hex.EncodeToString(sum[:])
	if got[0].EndpointHash != want {
		t.Fatalf("endpointHash mismatch: got %q want %q", got[0].EndpointHash, want)
	}
	if len(got[0].EndpointHash) != 64 {
		t.Fatalf("expected 64-char hex hash, got %d chars", len(got[0].EndpointHash))
	}
}

// DELETE by id removes the caller's device; a second delete (gone / not owned)
// is 404; a malformed id is 400.
func TestPush_DeleteByID(t *testing.T) {
	pool := newPushPool(t)
	store := push.NewStore(pool)
	h := NewPushHandler(pool, store, "pub", true)
	user := seedPushUser(t, pool)
	other := seedPushUser(t, pool)

	sub, err := store.Upsert(context.Background(), user, "https://push.example/del-by-id", "p", "a", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Malformed id → 400.
	rec := httptest.NewRecorder()
	req := withChiID(authReq(http.MethodDelete, "/api/v1/push/subscriptions/not-a-uuid", nil, user), "not-a-uuid")
	h.DeleteByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: expected 400, got %d", rec.Code)
	}

	// Another user deleting this id matches nothing → 404, row survives.
	rec = httptest.NewRecorder()
	req = withChiID(authReq(http.MethodDelete, "/api/v1/push/subscriptions/"+sub.ID.String(), nil, other), sub.ID.String())
	h.DeleteByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong owner: expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if l, _ := store.ListForUser(context.Background(), user); len(l) != 1 {
		t.Fatalf("expected row to survive wrong-owner delete, got %d", len(l))
	}

	// Owner deletes by id → 204, row gone.
	rec = httptest.NewRecorder()
	req = withChiID(authReq(http.MethodDelete, "/api/v1/push/subscriptions/"+sub.ID.String(), nil, user), sub.ID.String())
	h.DeleteByID(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner delete: expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if l, _ := store.ListForUser(context.Background(), user); len(l) != 0 {
		t.Fatalf("expected row removed, got %d", len(l))
	}

	// Deleting again (now gone) → 404.
	rec = httptest.NewRecorder()
	req = withChiID(authReq(http.MethodDelete, "/api/v1/push/subscriptions/"+sub.ID.String(), nil, user), sub.ID.String())
	h.DeleteByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("repeat delete: expected 404, got %d", rec.Code)
	}
}

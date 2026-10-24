package auth_test

// middleware_apikey_freshness_test.go — sk-674.18 T036, matrix row 58
// (INV-26, deactivation). Reference — the spec, written before the check:
// deactivation ends every session of the user and makes the keys invalid,
// "after the server answers the operation, requests with these sessions and keys
// are rejected (no cache extends their validity)"; reactivation does
// not bring them back (K-16: the epoch is not lowered, revoked keys stay
// revoked). So after DeactivateInTx the previous session (REST) and the key
// (MCP) are refused on the very first request although the key was warm in
// the key cache, and after a reactivation they are still refused.
//
// The package is auth_test, not auth: the session check lives in handler,
// which imports auth.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func TestDeactivation_RefusesWarmKeyAndSessionAtOnce(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	admin, adminName := testsupport.SeedUser(t, pool, "admin")
	u, _ := testsupport.SeedUser(t, pool, "user")
	actor := users.HumanActor(admin, adminName)

	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, _, err := apikeys.New(pool).Create(ctx, u, "freshness-test", prefix, hash); err != nil {
		t.Fatalf("create key: %v", err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// MCP: the key middleware, one instance — one cache — for the whole test.
	keyChain := auth.RequireAPIKey(pool, auth.MinBcryptCost, "")(ok)
	keyRequest := func() int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(auth.HeaderAPIKey, plain)
		req.Header.Set(auth.HeaderSurface, "internal") // the BFF's /mcp — an internal user's key is refused on the public surface
		rec := httptest.NewRecorder()
		keyChain.ServeHTTP(rec, req)
		return rec.Code
	}
	// REST: the session chain as cmd/api/main.go composes it.
	rest := chi.NewRouter()
	rest.Use(handler.ParseUserHeaders)
	rest.Use(handler.RequireSessionEpoch(pool))
	rest.Get("/probe", ok)
	var epoch int64
	if err := pool.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id = $1`, u).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	sessionRequest := func() int {
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		req.Header.Set(auth.HeaderUserID, u.String())
		req.Header.Set(auth.HeaderUserRole, "user")
		req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
		rec := httptest.NewRecorder()
		rest.ServeHTTP(rec, req)
		return rec.Code
	}

	// Positive control, and the key warmed: the second request is a cache hit.
	for i := 0; i < 2; i++ {
		if code := keyRequest(); code != http.StatusOK {
			t.Fatalf("key before the deactivation (request %d) = %d, want 200", i+1, code)
		}
	}
	if code := sessionRequest(); code != http.StatusOK {
		t.Fatalf("session before the deactivation = %d, want 200", code)
	}

	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		_, err := store.DeactivateInTx(ctx, tx, u, actor)
		return err
	}); err != nil {
		t.Fatalf("DeactivateInTx: %v", err)
	}
	if code := keyRequest(); code != http.StatusUnauthorized {
		t.Errorf("warm key on the first request after the deactivation = %d, want 401 — the cache extended it", code)
	}
	if code := sessionRequest(); code != http.StatusUnauthorized {
		t.Errorf("session on the first request after the deactivation = %d, want 401", code)
	}

	if err := store.Reactivate(ctx, u, actor); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if code := keyRequest(); code != http.StatusUnauthorized {
		t.Errorf("the key after the reactivation = %d, want 401 — reactivation does not restore keys", code)
	}
	if code := sessionRequest(); code != http.StatusUnauthorized {
		t.Errorf("the previous session after the reactivation = %d, want 401 — reactivation does not lower the epoch", code)
	}
}

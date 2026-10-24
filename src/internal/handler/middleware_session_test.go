package handler

// middleware_session_test.go — sk-674.18 T036, matrix row 57 (INV-26,
// sessions). Reference — the spec, written before the check: a password
// reset ends the user's other sessions and leaves the agent keys working;
// "after the server's response to the operation, requests with these sessions … are rejected
// (no cache extends their validity)". So after BumpSessionEpochInTx:
// the previous X-Skein-Session-Epoch is refused with 401 session_revoked,
// the new one passes, another user's sessions are untouched (the epoch is
// per user), and the same user's API key still works (the raise revokes no
// key).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// sessionRouter is the user-session chain as cmd/api/main.go composes it
// for /api/v1: ParseUserHeaders, then RequireSessionEpoch, then the route.
func sessionRouter(t *testing.T) chi.Router {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	r := chi.NewRouter()
	r.Use(ParseUserHeaders)
	r.Use(RequireSessionEpoch(pool))
	r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return r
}

func sessionRequest(t *testing.T, router http.Handler, user uuid.UUID, epoch int64) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderUserRole, "user")
	req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body) // a 200 body is empty
	return rec.Code, body["error"]
}

func TestSessionEpoch_RaiseEndsTheUsersSessionsOnly(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	router := sessionRouter(t)
	u, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")

	// u's agent key, warmed in the key middleware before the raise.
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, _, err := apikeys.New(pool).Create(ctx, u, "epoch-test", prefix, hash); err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyChain := auth.RequireAPIKey(pool, auth.MinBcryptCost, "")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	keyRequest := func() int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(auth.HeaderAPIKey, plain)
		req.Header.Set(auth.HeaderSurface, "internal") // the BFF's /mcp — an internal user's key is refused on the public surface
		rec := httptest.NewRecorder()
		keyChain.ServeHTTP(rec, req)
		return rec.Code
	}

	// Positive control: before the raise both sessions and the key pass.
	if code, _ := sessionRequest(t, router, u, 0); code != http.StatusOK {
		t.Fatalf("u's session before the raise = %d, want 200", code)
	}
	if code := keyRequest(); code != http.StatusOK {
		t.Fatalf("u's key before the raise = %d, want 200", code)
	}

	var raised int64
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		raised, err = users.BumpSessionEpochInTx(ctx, tx, u)
		return err
	}); err != nil {
		t.Fatalf("BumpSessionEpochInTx: %v", err)
	}
	if raised == 0 {
		t.Fatal("the epoch did not rise")
	}

	if code, errCode := sessionRequest(t, router, u, 0); code != http.StatusUnauthorized || errCode != auth.CodeSessionRevoked {
		t.Errorf("u's session with the previous epoch = %d %q, want 401 %q", code, errCode, auth.CodeSessionRevoked)
	}
	if code, _ := sessionRequest(t, router, u, raised); code != http.StatusOK {
		t.Errorf("u's session with the new epoch %d = %d, want 200", raised, code)
	}
	if code, _ := sessionRequest(t, router, other, 0); code != http.StatusOK {
		t.Errorf("another user's session after u's raise = %d, want 200 — the epoch is per user", code)
	}
	if code := keyRequest(); code != http.StatusOK {
		t.Errorf("u's agent key after the raise = %d, want 200 — the raise revokes no key", code)
	}
}

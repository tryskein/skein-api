package handler

// auth_me_test.go — sk-674.18 T037, matrix row 118 (US-20). Reference —
// US-20 of the spec and the contract T013: GET /auth/me carries
// emailConfirmed and hasApiKeys by the actual state — the e-mail confirmed
// or not, an active (not revoked) API key or none.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

func TestAuthMe_AccountFacts(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.With(ParseUserHeaders, RequireUser).Get("/auth/me", h.Me)
	u, name := testsupport.SeedUser(t, pool, "user")
	me := func() (bool, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
		req.Header.Set(auth.HeaderUserID, u.String())
		req.Header.Set(auth.HeaderUsername, name)
		req.Header.Set(auth.HeaderUserRole, "user")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("/auth/me: %d %s", rec.Code, rec.Body.String())
		}
		confirmed, ok1 := body["emailConfirmed"].(bool)
		keys, ok2 := body["hasApiKeys"].(bool)
		if !ok1 || !ok2 {
			t.Fatalf("/auth/me lacks emailConfirmed or hasApiKeys: %s", rec.Body.String())
		}
		return confirmed, keys
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	exec(`UPDATE users SET email_confirmed_at = NULL WHERE id = $1`, u)
	if c, k := me(); c || k {
		t.Errorf("unconfirmed, no key: emailConfirmed=%v hasApiKeys=%v", c, k)
	}
	exec(`UPDATE users SET email_confirmed_at = now() WHERE id = $1`, u)
	if c, _ := me(); !c {
		t.Error("a confirmed e-mail reads emailConfirmed=false")
	}
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	keys := apikeys.New(pool)
	key, _, err := keys.Create(ctx, u, "auth me", prefix, hash)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, key) })
	if _, k := me(); !k {
		t.Error("an active key reads hasApiKeys=false")
	}
	if err := keys.Revoke(ctx, u, key); err != nil {
		t.Fatal(err)
	}
	if _, k := me(); k {
		t.Error("with its only key revoked, hasApiKeys reads true")
	}
}

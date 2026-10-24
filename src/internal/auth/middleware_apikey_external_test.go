package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
)

// TestRequireAPIKey_IsExternal_ColdAndWarm proves the api-key→cache→RPC ctx
// hop for sk-163 S1 on BOTH the cold path (DB lookup + bcrypt) and the warm
// path (cache hit): the resolved is_external flag must reach the downstream
// handler's ctx either way. Without the cache carrying the flag (spec §9),
// the second request would silently drop it.
func TestRequireAPIKey_IsExternal_ColdAndWarm(t *testing.T) {
	pool := newOIDCTestPool(t) // skips when TEST_DATABASE_URL is unset
	ctx := context.Background()

	// Seed an EXTERNAL user (role=user, is_external=true) directly.
	uid := uuid.Must(uuid.NewRandom())
	uname := "akx" + strings.ReplaceAll(uid.String()[:18], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', true, now(), '{}'::jsonb)`,
		uid, uname+"@t.l", uname, uname); err != nil {
		t.Fatalf("seed external user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})

	// Issue a real api key for that user (low bcrypt cost for test speed).
	plain, prefix, hash, err := GenerateAPIKey(MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	store := apikeys.New(pool)
	if _, _, err := store.Create(ctx, uid, "ext-test", prefix, hash); err != nil {
		t.Fatalf("Create key: %v", err)
	}

	// Probe handler records what RequireAPIKey stashed in ctx.
	var sawExternal bool
	var sawUser bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		sawUser = ok
		sawExternal = p.IsExternal
		w.WriteHeader(http.StatusOK)
	})
	mw := RequireAPIKey(pool, MinBcryptCost, "")(probe)

	doReq := func() {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(HeaderAPIKey, plain)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("middleware returned %d, want 200", rec.Code)
		}
	}

	// Cold path: DB lookup + bcrypt verify, then cache populate.
	doReq()
	if !sawUser {
		t.Fatalf("cold: principal not present in ctx")
	}
	if !sawExternal {
		t.Errorf("cold: IsExternal=false in ctx, want true")
	}

	// Warm path: same key → cache hit, must still carry is_external.
	sawExternal = false
	doReq()
	if !sawExternal {
		t.Errorf("warm (cache hit): IsExternal=false in ctx, want true")
	}
}

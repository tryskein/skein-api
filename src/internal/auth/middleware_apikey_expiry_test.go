package auth_test

// middleware_apikey_expiry_test.go — matrix row 24 (US-4, SC-004 for MCP,
// SC-013). Reference — the spec, written before the code: a key that has
// expired, was revoked or whose owner was deactivated is refused on any
// operation with a 401 whose body is the same and carries no reason; the key
// being warm in the cache changes nothing. A failure to read the key's status
// is the 500 /mcp has always answered, not a 401. A key created before
// lifetimes existed (no expiry) keeps working.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

type mcpAnswer struct {
	code int
	body []byte
}

// mcpChain is the /mcp key middleware over pool, one instance (one cache)
// per call, in front of a handler answering 200.
func mcpChain(pool *pgxpool.Pool) func(plain string) mcpAnswer {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	chain := auth.RequireAPIKey(pool, auth.MinBcryptCost, "")(ok)
	return func(plain string) mcpAnswer {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(auth.HeaderAPIKey, plain)
		req.Header.Set(auth.HeaderSurface, "internal") // the BFF's /mcp
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return mcpAnswer{code: rec.Code, body: rec.Body.Bytes()}
	}
}

func TestRequireAPIKey_WarmKeyRefusedOnNextRequest(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	// The answer to a key that was never issued: the reference body.
	unknownPlain, _, _, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	unknown := mcpChain(pool)(unknownPlain)
	if unknown.code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d, want 401", unknown.code)
	}

	for _, c := range []struct {
		name   string
		change func(t *testing.T, user, key uuid.UUID)
	}{
		{"expired", func(t *testing.T, _, key uuid.UUID) {
			if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, key); err != nil {
				t.Fatal(err)
			}
		}},
		{"revoked", func(t *testing.T, user, key uuid.UUID) {
			if err := apikeys.New(pool).Revoke(ctx, user, key); err != nil {
				t.Fatal(err)
			}
		}},
		{"owner deactivated", func(t *testing.T, user, _ uuid.UUID) {
			if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, user); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, _ := testsupport.SeedUser(t, pool, "user")
			plain, id := issueKey(t, pool, u)
			call := mcpChain(pool)
			// Cold, then warm: the second request is answered by the cache.
			for i := 0; i < 2; i++ {
				if got := call(plain); got.code != http.StatusOK {
					t.Fatalf("request %d before the change = %d, want 200", i+1, got.code)
				}
			}
			c.change(t, u, id)
			got := call(plain)
			if got.code != http.StatusUnauthorized {
				t.Fatalf("warm key on the next request = %d, want 401", got.code)
			}
			if !bytes.Equal(got.body, unknown.body) {
				t.Fatalf("body = %q, want the unknown-key body %q byte for byte", got.body, unknown.body)
			}
		})
	}

	t.Run("a failed status read is the 500 of /mcp", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, _ := issueKey(t, pool, u)
		own := testsupport.NewTestPool(t)
		call := mcpChain(own)
		if got := call(plain); got.code != http.StatusOK {
			t.Fatalf("before the failure = %d, want 200", got.code)
		}
		own.Close()
		got := call(plain)
		if got.code != http.StatusInternalServerError || string(got.body) != `{"error":"internal_error"}` {
			t.Fatalf("after the failure = %d %q, want 500 {\"error\":\"internal_error\"}", got.code, got.body)
		}
	})

	t.Run("a migrated key without an expiry passes", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, id := issueKey(t, pool, u)
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = NULL WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		call := mcpChain(pool)
		for i := 0; i < 2; i++ {
			if got := call(plain); got.code != http.StatusOK {
				t.Fatalf("request %d = %d %q, want 200", i+1, got.code, got.body)
			}
		}
	})
}

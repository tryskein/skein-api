package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
)

// newLazyPool returns a pool that is never dialled. pgxpool connects on first
// acquire, so a gate that rejects a request before any lookup can be exercised
// without a database — which is the point: the discovery surface must be
// guarded in the unit suite too, not only where Postgres happens to be up.
func newLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://challenge-test:challenge-test@127.0.0.1:1/challenge-test?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

const testChallenge = `Bearer resource_metadata="https://skein-mcp.test/.well-known/oauth-protected-resource/mcp"`

// TestRequireAPIKey_ChallengeOnEveryUnauthenticatedBranch — sk-304.
//
// An MCP client starts by probing the door UNAUTHENTICATED and reads the
// `WWW-Authenticate` header off the 401 to learn where the protected-resource
// metadata lives (RFC 9728 §5.1; the MCP authorization spec makes the header
// the primary discovery route). The header therefore has to be on EVERY 401
// this gate can produce, not just the "no header" one: a client whose key was
// revoked, mistyped, or shaped wrong has exactly the same discovery problem.
//
// The branches below are the ones reachable without a database. The
// DB-dependent ones (prefix not found, hash mismatch) are covered by
// TestRequireAPIKey_ChallengeOnDBRejection.
func TestRequireAPIKey_ChallengeOnEveryUnauthenticatedBranch(t *testing.T) {
	// Every case short-circuits in the shape filter before a lookup, so no
	// database is needed. RequireAPIKey only refuses a nil pool.
	pool := newLazyPool(t)

	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler reached — the request should have been rejected")
		w.WriteHeader(http.StatusOK)
	})
	mw := RequireAPIKey(pool, MinBcryptCost, testChallenge)(probe)

	cases := []struct {
		name string
		key  string // value of X-Skein-Api-Key; "" = header absent
	}{
		{name: "header absent", key: ""},
		{name: "blank header", key: "   "},
		{name: "wrong tag", key: "gho_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{name: "too short", key: "ska_x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.key != "" {
				req.Header.Set(HeaderAPIKey, tc.key)
			}
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != testChallenge {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, testChallenge)
			}
			// The body stays the single generic message — the challenge adds a
			// discovery pointer, it must not become a failure-reason oracle.
			if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"unauthenticated"}` {
				t.Fatalf("body = %q, want the generic unauthenticated body", body)
			}
		})
	}
}

// TestRequireAPIKey_ChallengeOnDBRejection covers the two 401 branches that
// need a real database: a well-shaped key whose prefix is unknown, and a
// well-shaped key whose prefix exists but whose hash does not match. Both are
// what a client with a stale or revoked credential actually hits, and both
// must still hand it the discovery pointer.
func TestRequireAPIKey_ChallengeOnDBRejection(t *testing.T) {
	pool := newOIDCTestPool(t) // skips when TEST_DATABASE_URL is unset
	ctx := context.Background()

	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler reached — the request should have been rejected")
		w.WriteHeader(http.StatusOK)
	})
	mw := RequireAPIKey(pool, MinBcryptCost, testChallenge)(probe)

	assert401WithChallenge := func(t *testing.T, key string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(HeaderAPIKey, key)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != testChallenge {
			t.Fatalf("WWW-Authenticate = %q, want %q", got, testChallenge)
		}
	}

	t.Run("prefix not found", func(t *testing.T) {
		// A syntactically valid key that was never issued.
		unknown, _, _, err := GenerateAPIKey(MinBcryptCost)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		assert401WithChallenge(t, unknown)
	})

	t.Run("hash mismatch", func(t *testing.T) {
		uid := uuid.Must(uuid.NewRandom())
		uname := "akc" + strings.ReplaceAll(uid.String()[:18], "-", "")
		if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', false, now(), '{}'::jsonb)`,
			uid, uname+"@t.l", uname, uname); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
		})

		plain, prefix, hash, err := GenerateAPIKey(MinBcryptCost)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if _, _, err := apikeys.New(pool).Create(ctx, uid, "challenge-test", prefix, hash); err != nil {
			t.Fatalf("Create key: %v", err)
		}

		// Same prefix (so the lookup succeeds), different secret ⇒ bcrypt
		// mismatch. Flip the last character of the secret part.
		last := plain[len(plain)-1]
		replacement := byte('a')
		if last == 'a' {
			replacement = 'b'
		}
		tampered := plain[:len(plain)-1] + string(replacement)
		assert401WithChallenge(t, tampered)
	})
}

// TestRequireAPIKey_EmptyChallengeOmitsHeader — the challenge is a hint, never
// the auth decision. A deployment that has not computed one (or a test that
// does not care) must still get a clean 401 rather than a malformed header.
func TestRequireAPIKey_EmptyChallengeOmitsHeader(t *testing.T) {
	pool := newLazyPool(t)

	mw := RequireAPIKey(pool, MinBcryptCost, "")(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q, want it absent", got)
	}
}

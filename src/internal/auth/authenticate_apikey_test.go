package auth_test

// authenticate_apikey_test.go — matrix rows 22 and 23 (US-4, D-4, FOR-PLAN
// "warm path"). Reference — the spec and plan D-4, written before the code:
// authenticating a key yields one of three kinds of outcome — the identity
// and the grant; a rejection of the credential with a reason from a closed
// set (no key or not of the format, unknown, mismatch, revoked, expired,
// owner inactive); an internal error when the lookup or the status read
// fails, never passed off as a bad key. The decision and the grant are read
// on every request, warm or cold: the cache keeps only the bcrypt result.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// issueKey stores a key with the owner grant (manage, scope all, the default
// lifetime) for userID and returns the plain key and its id.
func issueKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) (string, uuid.UUID) {
	t.Helper()
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	id, _, err := testsupport.CreateOwnerKey(context.Background(), pool, userID, "authenticate-test", prefix, hash)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return plain, id
}

// tamper flips the last character of the secret part: the prefix stays, so
// the stored key is found and the bcrypt verification fails.
func tamper(plain string) string {
	last := plain[len(plain)-1]
	repl := byte('a')
	if last == 'a' {
		repl = 'b'
	}
	return plain[:len(plain)-1] + string(repl)
}

func wantRejected(t *testing.T, out auth.APIKeyOutcome, reason auth.APIKeyRejection) {
	t.Helper()
	if out.Kind != auth.APIKeyRejected || out.Reason != reason {
		t.Fatalf("outcome = kind %d reason %q (err %v), want rejected %q", out.Kind, out.Reason, out.Err, reason)
	}
}

func wantAuthenticated(t *testing.T, out auth.APIKeyOutcome, user, key uuid.UUID) {
	t.Helper()
	if out.Kind != auth.APIKeyAuthenticated {
		t.Fatalf("outcome = kind %d reason %q (err %v), want authenticated", out.Kind, out.Reason, out.Err)
	}
	if out.Key.UserID != user || out.Key.KeyID != key {
		t.Fatalf("identity = user %s key %s, want user %s key %s", out.Key.UserID, out.Key.KeyID, user, key)
	}
}

// TestAuthenticateAPIKey_Outcomes — row 22: each reason of a rejected
// credential is its own outcome, a valid key yields the identity and the
// grant, and a failed lookup or status read is an internal error on the cold
// and on the warm path. Each case uses a fresh authenticator, so it takes the
// cold path unless the case warms it on purpose.
func TestAuthenticateAPIKey_Outcomes(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	newAuthn := func() *auth.APIKeyAuthenticator { return auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost) }

	t.Run("no key and a key not of the format are malformed", func(t *testing.T) {
		for _, plain := range []string{"", "ska_short", "glpat-0123456789abcdefghijkl"} {
			wantRejected(t, newAuthn().AuthenticateAPIKey(ctx, plain), auth.RejectMalformed)
		}
	})

	t.Run("a key never issued is unknown", func(t *testing.T) {
		plain, _, _, err := auth.GenerateAPIKey(auth.MinBcryptCost)
		if err != nil {
			t.Fatal(err)
		}
		wantRejected(t, newAuthn().AuthenticateAPIKey(ctx, plain), auth.RejectUnknown)
	})

	t.Run("a wrong secret under a stored prefix is a mismatch", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, _ := issueKey(t, pool, u)
		wantRejected(t, newAuthn().AuthenticateAPIKey(ctx, tamper(plain)), auth.RejectMismatch)
	})

	t.Run("a revoked key is revoked", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, id := issueKey(t, pool, u)
		if err := apikeys.New(pool).Revoke(ctx, u, id); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		wantRejected(t, newAuthn().AuthenticateAPIKey(ctx, plain), auth.RejectRevoked)
	})

	t.Run("a key past its lifetime is expired", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, id := issueKey(t, pool, u)
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		wantRejected(t, newAuthn().AuthenticateAPIKey(ctx, plain), auth.RejectExpired)
	})

	t.Run("a key of a deactivated owner is owner inactive", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, _ := issueKey(t, pool, u)
		// The key itself stays unrevoked: the owner's state alone decides.
		if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, u); err != nil {
			t.Fatal(err)
		}
		wantRejected(t, newAuthn().AuthenticateAPIKey(ctx, plain), auth.RejectOwnerInactive)
	})

	t.Run("a valid key yields the identity and the grant", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, id := issueKey(t, pool, u)
		out := newAuthn().AuthenticateAPIKey(ctx, plain)
		wantAuthenticated(t, out, u, id)
		if out.Key.UserRole != "user" {
			t.Fatalf("role = %q, want user", out.Key.UserRole)
		}
		g := auth.RequestGrant(out.Key)
		if g.Level != apikeys.LevelManage || g.Admin || g.Scope.Mode != apikeys.ScopeAll || g.ExpiresAt == nil {
			t.Fatalf("grant = %+v, want manage, no admin, scope all, with an expiry", g)
		}
	})

	t.Run("an admin owner's key carries the effective admin flag", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "admin")
		plain, id := issueKey(t, pool, u)
		out := newAuthn().AuthenticateAPIKey(ctx, plain)
		wantAuthenticated(t, out, u, id)
		if !auth.RequestGrant(out.Key).Admin {
			t.Fatalf("grant admin = false, want true for a key with the flag of an internal admin")
		}
	})

	t.Run("a failed lookup on the cold path is an internal error", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, _ := issueKey(t, pool, u)
		closed := testsupport.NewTestPool(t)
		closed.Close()
		out := auth.NewAPIKeyAuthenticator(closed, auth.MinBcryptCost).AuthenticateAPIKey(ctx, plain)
		if out.Kind != auth.APIKeyInternalError || out.Err == nil {
			t.Fatalf("outcome = kind %d reason %q, want internal error with the cause", out.Kind, out.Reason)
		}
		if out.Warm {
			t.Fatalf("outcome on a never-seen key reported warm")
		}
	})

	t.Run("a failed status read on the warm path is an internal error", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		plain, id := issueKey(t, pool, u)
		own := testsupport.NewTestPool(t)
		authn := auth.NewAPIKeyAuthenticator(own, auth.MinBcryptCost)
		wantAuthenticated(t, authn.AuthenticateAPIKey(ctx, plain), u, id)
		own.Close()
		out := authn.AuthenticateAPIKey(ctx, plain)
		if out.Kind != auth.APIKeyInternalError || out.Err == nil {
			t.Fatalf("outcome = kind %d reason %q, want internal error with the cause", out.Kind, out.Reason)
		}
		if !out.Warm {
			t.Fatalf("the second request did not hit the cache — the warm path is not exercised")
		}
	})
}

// TestAuthenticateAPIKey_GrantFollowsTheDatabase — row 23: the grant of the
// next request reflects the database on the warm and on the cold path — a
// narrowed level and scope, then a lifetime in the past. Warmth is asserted in
// the same subtest: the request after the change is a cache hit.
func TestAuthenticateAPIKey_GrantFollowsTheDatabase(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	u, _ := testsupport.SeedUser(t, pool, "user")
	project, _ := testsupport.SeedProject(t, pool, u, "grant-fresh")
	plain, id := issueKey(t, pool, u)

	warm := auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost)
	first := warm.AuthenticateAPIKey(ctx, plain)
	wantAuthenticated(t, first, u, id)
	if first.Warm {
		t.Fatalf("first request reported warm")
	}
	if g := auth.RequestGrant(first.Key); g.Level != apikeys.LevelManage || g.Scope.Mode != apikeys.ScopeAll {
		t.Fatalf("grant before the change = %+v, want manage, scope all", g)
	}

	t.Run("narrowed level and scope", func(t *testing.T) {
		expiry := first.Key.Grant.ExpiresAt.Add(-24 * time.Hour)
		next := apikeys.Grant{
			Level:     apikeys.LevelRead,
			Scope:     apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{project}},
			ExpiresAt: &expiry,
		}
		if err := apikeys.New(pool).Narrow(ctx, u, id, next); err != nil {
			t.Fatalf("Narrow: %v", err)
		}
		check := func(t *testing.T, out auth.APIKeyOutcome) {
			t.Helper()
			wantAuthenticated(t, out, u, id)
			g := auth.RequestGrant(out.Key)
			if g.Level != apikeys.LevelRead || g.Scope.Mode != apikeys.ScopeProjects ||
				!slices.Equal(g.Scope.Projects, []uuid.UUID{project}) ||
				g.ExpiresAt == nil || !g.ExpiresAt.Equal(expiry) {
				t.Fatalf("grant after the change = %+v, want read, scope projects [%s], expiry %s", g, project, expiry)
			}
		}
		out := warm.AuthenticateAPIKey(ctx, plain)
		if !out.Warm {
			t.Fatalf("the request after the change did not hit the cache")
		}
		check(t, out)
		cold := auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost).AuthenticateAPIKey(ctx, plain)
		if cold.Warm {
			t.Fatalf("a fresh authenticator reported warm")
		}
		check(t, cold)
	})

	t.Run("lifetime in the past", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		out := warm.AuthenticateAPIKey(ctx, plain)
		if !out.Warm {
			t.Fatalf("the request after the change did not hit the cache")
		}
		wantRejected(t, out, auth.RejectExpired)
		wantRejected(t, auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost).AuthenticateAPIKey(ctx, plain), auth.RejectExpired)
	})
}

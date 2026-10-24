package apikeys_test

import (
	"context"
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
)

// sk-105.5 (S4) — API-key permission boundary.
//
// The security invariant: an api_key resolves to EXACTLY its owning user.
// The MCP middleware authenticates a request by (1) slicing the prefix off
// the presented plaintext, (2) LookupByPrefix → (key_id, user_id, role,
// hash), (3) bcrypt-verifying the plaintext against that row's hash. The
// resolved user_id IS the caller identity used for every downstream
// visibility/permission check (internal/auth/middleware_apikey.go). There
// is no parameter, header, or field by which a caller can ask to act AS a
// different user — identity is derived solely from which stored row the
// prefix matches.
//
// These tests pin that derivation at the resolution seam (apikeys.Store +
// auth.VerifyAPIKey), the same seam the middleware drives. They assert:
//   1. User A's key resolves to A's user_id, never to B's.
//   2. A's plaintext does NOT bcrypt-verify against B's stored hash — a
//      caller cannot present A's key and have it accepted as B's row even
//      if a prefix collision were forced.
//   3. A key whose prefix matches no row fails resolution (no implicit
//      fallback to "some" user).

// TestAPIKeyBoundary_ResolvesToOwningUserOnly verifies LookupByPrefix maps
// each user's key to that user's id — never the other user's — and that
// each key's plaintext verifies against its own hash but NOT the other's.
func TestAPIKeyBoundary_ResolvesToOwningUserOnly(t *testing.T) {
	pool := newTestPool(t)
	store := apikeys.New(pool)
	ctx := context.Background()

	userA := seedUserDirect(t, pool, false)
	userB := seedUserDirect(t, pool, false)

	plainA, prefixA, hashA, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey A: %v", err)
	}
	plainB, prefixB, hashB, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey B: %v", err)
	}
	if _, _, err := store.Create(ctx, userA, "key-a", prefixA, hashA); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	if _, _, err := store.Create(ctx, userB, "key-b", prefixB, hashB); err != nil {
		t.Fatalf("Create B: %v", err)
	}

	// A's prefix resolves to A — and to A's hash, never B's.
	resA, err := store.LookupByPrefix(ctx, prefixA)
	if err != nil {
		t.Fatalf("LookupByPrefix A: %v", err)
	}
	if resA.UserID != userA {
		t.Fatalf("A's key resolved to user %s, want %s (A) — key must map to its owning user only", resA.UserID, userA)
	}
	if resA.UserID == userB {
		t.Fatalf("A's key resolved to user B (%s) — cross-user impersonation", userB)
	}

	resB, err := store.LookupByPrefix(ctx, prefixB)
	if err != nil {
		t.Fatalf("LookupByPrefix B: %v", err)
	}
	if resB.UserID != userB {
		t.Fatalf("B's key resolved to user %s, want %s (B)", resB.UserID, userB)
	}

	// A's plaintext verifies against A's resolved hash...
	if err := auth.VerifyAPIKey(resA.KeyHash, plainA); err != nil {
		t.Fatalf("A's plaintext failed to verify against A's hash: %v", err)
	}
	// ...but must NOT verify against B's hash — presenting A's key can
	// never authenticate as B's row.
	if err := auth.VerifyAPIKey(resB.KeyHash, plainA); err == nil {
		t.Fatalf("A's plaintext verified against B's hash — a key must not authenticate as another user's row")
	}
	// Symmetric: B's plaintext must not verify against A's hash.
	if err := auth.VerifyAPIKey(resA.KeyHash, plainB); err == nil {
		t.Fatalf("B's plaintext verified against A's hash — cross-user authentication")
	}
}

// TestAPIKeyBoundary_UnknownPrefixNoFallback verifies that a prefix
// matching no stored row fails resolution with ErrNotFound — there is no
// implicit fallback to a default / first / admin user. A caller presenting
// an unrecognised key gets nobody's identity, not somebody's.
func TestAPIKeyBoundary_UnknownPrefixNoFallback(t *testing.T) {
	pool := newTestPool(t)
	store := apikeys.New(pool)
	ctx := context.Background()

	// Seed a real key so the table is non-empty (guards against a
	// "happens to pass because table is empty" false positive).
	user := seedUserDirect(t, pool, false)
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, _, err := store.Create(ctx, user, "real", prefix, hash); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A different, never-stored prefix must not resolve to anyone.
	bogus, _, _, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey bogus: %v", err)
	}
	if _, err := store.LookupByPrefix(ctx, auth.ExtractPrefix(bogus)); err == nil {
		t.Fatalf("unknown prefix resolved to a user — must return ErrNotFound (no fallback identity)")
	}
}

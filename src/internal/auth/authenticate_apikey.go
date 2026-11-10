package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
)

// APIKeyOutcomeKind is the kind of the outcome of authenticating a key.
type APIKeyOutcomeKind int

const (
	// APIKeyAuthenticated — the key is valid: the outcome carries the
	// caller's identity and the key's grant.
	APIKeyAuthenticated APIKeyOutcomeKind = iota + 1
	// APIKeyRejected — the presented credential does not authenticate; the
	// outcome carries the reason.
	APIKeyRejected
	// APIKeyInternalError — the decision could not be read (the lookup or the
	// status read failed). It is never a verdict on the key: a database
	// failure must not tell a client its key is wrong.
	APIKeyInternalError
)

// APIKeyRejection is the reason a presented credential does not
// authenticate. The set is closed. The reason is for logs, metrics and the
// failure limiter; every reason answers the client with the same body.
type APIKeyRejection string

const (
	// RejectMalformed — no key, or not of the key format.
	RejectMalformed APIKeyRejection = "malformed"
	// RejectUnknown — no key is stored under the presented prefix (or the
	// matched key no longer exists).
	RejectUnknown APIKeyRejection = "unknown"
	// RejectMismatch — a key is stored under the prefix, but the presented
	// key does not verify against it.
	RejectMismatch APIKeyRejection = "mismatch"
	// RejectRevoked — the presented key verifies and is revoked.
	RejectRevoked APIKeyRejection = "revoked"
	// RejectExpired — the presented key verifies and its lifetime ended.
	RejectExpired APIKeyRejection = "expired"
	// RejectOwnerInactive — the presented key verifies and its owner is
	// deactivated or deleted.
	RejectOwnerInactive APIKeyRejection = "owner_inactive"
)

// APIKeyOutcome is the result of AuthenticateAPIKey. Exactly one branch is
// meaningful, chosen by Kind: Key for APIKeyAuthenticated, Reason for
// APIKeyRejected, Err for APIKeyInternalError.
type APIKeyOutcome struct {
	Kind APIKeyOutcomeKind
	// Key is the key's status read on this request: the owner's identity
	// and the key's grant. Set only when authenticated.
	Key apikeys.KeyStatus
	// Reason names why the credential was rejected.
	Reason APIKeyRejection
	// Err is the failure that prevented a decision.
	Err error
	// Warm reports that the bcrypt verification was answered from the
	// cache rather than computed on this request.
	Warm bool
}

// APIKeyAuthenticator decides whether a presented key authenticates. It
// writes no response: the caller maps the outcome to its own wire format
// (RequireAPIKey for /mcp; the public API its own).
//
// What is cached, and what is not (D-4): the cache keeps only the result of
// the bcrypt verification — the presented key matched key id X, or it
// matched nothing. Whether key X is usable now — revocation, expiry, the
// owner's activity — and what it grants are read through apikeys.Store.Status
// on every request, warm or cold, so a narrowed grant, a revocation, an
// expiry or a deactivation takes effect on the next request.
type APIKeyAuthenticator struct {
	store      *apikeys.Store
	cache      *apiKeyCache
	bcryptCost int
	now        func() time.Time
}

// NewAPIKeyAuthenticator builds an authenticator over the pool. bcryptCost
// is the cost live keys are hashed at; the unknown-prefix branch verifies a
// dummy hash of the same cost, so it costs what a mismatch costs.
func NewAPIKeyAuthenticator(pool *pgxpool.Pool, bcryptCost int) *APIKeyAuthenticator {
	if pool == nil {
		panic("NewAPIKeyAuthenticator: pool is nil")
	}
	return &APIKeyAuthenticator{
		store:      apikeys.New(pool),
		cache:      newAPIKeyCache(apiKeyCacheCapacity, apiKeyCacheTTL, nil),
		bcryptCost: bcryptCost,
		now:        time.Now,
	}
}

// AuthenticateAPIKey authenticates the presented plain key.
//
//  1. Shape: not of the key format → rejected, malformed (no DB hit).
//  2. Warm: the cache holds the bcrypt result for this exact key — a
//     mismatch → rejected, mismatch; a match → step 4 with the cached id.
//  3. Cold: the keys stored under the prefix; none → a dummy bcrypt verify
//     (the same cost as a mismatch) and rejected, unknown; the presented key
//     verifies against none → rejected, mismatch (cached); it verifies →
//     the match is cached.
//  4. Status of the matched key: gone → unknown; revoked, expired, owner
//     inactive → rejected with that reason; usable → authenticated, with
//     the identity and the grant read now.
//
// A failed lookup or status read is an internal error, on either path.
func (a *APIKeyAuthenticator) AuthenticateAPIKey(ctx context.Context, plain string) APIKeyOutcome {
	if !IsAPIKeyShape(plain) {
		return rejected(RejectMalformed, false)
	}
	prefix := ExtractPrefix(plain)
	if prefix == "" {
		return rejected(RejectMalformed, false)
	}

	if entry, hit := a.cache.Get(plain); hit {
		if !entry.ok {
			return rejected(RejectMismatch, true)
		}
		return a.decide(ctx, entry.keyID, true)
	}

	dummyHashOnce.Do(func() { dummyHash = computeDummyHash(a.bcryptCost) })
	candidates, err := a.store.CandidatesByPrefix(ctx, prefix)
	if err != nil {
		return APIKeyOutcome{Kind: APIKeyInternalError, Err: fmt.Errorf("looking up the api key: %w", err)}
	}
	if len(candidates) == 0 {
		// W1: pay the bcrypt cost so "prefix unknown" takes as long as
		// "prefix known, key wrong". Not cached, so the path stays the same
		// on every request.
		constantTimeMissCompare(plain)
		return rejected(RejectUnknown, false)
	}
	for _, c := range candidates {
		if VerifyAPIKey(c.KeyHash, plain) == nil {
			a.cache.Put(plain, apiKeyCacheEntry{keyID: c.KeyID, ok: true})
			return a.decide(ctx, c.KeyID, false)
		}
	}
	a.cache.Put(plain, apiKeyCacheEntry{ok: false})
	return rejected(RejectMismatch, false)
}

// decide reads the matched key's status and turns it into the outcome.
func (a *APIKeyAuthenticator) decide(ctx context.Context, keyID uuid.UUID, warm bool) APIKeyOutcome {
	st, err := a.store.Status(ctx, keyID)
	switch {
	case errors.Is(err, apikeys.ErrNotFound):
		return rejected(RejectUnknown, warm)
	case err != nil:
		return APIKeyOutcome{Kind: APIKeyInternalError, Err: err, Warm: warm}
	case st.Revoked():
		return rejected(RejectRevoked, warm)
	case st.Expired(a.now()):
		return rejected(RejectExpired, warm)
	case !st.OwnerActive:
		return rejected(RejectOwnerInactive, warm)
	}
	return APIKeyOutcome{Kind: APIKeyAuthenticated, Key: st, Warm: warm}
}

// TouchLastUsed bumps the key's last_used_at (throttled in SQL). Callers
// treat an error as soft: the request is already authenticated.
func (a *APIKeyAuthenticator) TouchLastUsed(ctx context.Context, keyID uuid.UUID) error {
	return a.store.TouchLastUsed(ctx, keyID)
}

func rejected(reason APIKeyRejection, warm bool) APIKeyOutcome {
	return APIKeyOutcome{Kind: APIKeyRejected, Reason: reason, Warm: warm}
}

// RequestGrant is the grant of the key a request authenticated with, as it
// applies to this request: Admin is the effective admin flag (set on the key
// and the owner an internal instance admin), not the stored one.
func RequestGrant(st apikeys.KeyStatus) apikeys.Grant {
	g := st.Grant
	g.Admin = st.EffectiveAdmin
	return g
}

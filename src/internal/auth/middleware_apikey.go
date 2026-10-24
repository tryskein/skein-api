package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"skein-api/internal/apikeys"
)

// apiKeyAuthLogger captures structured audit fields without leaking the
// plaintext key. The middleware reaches for slog.Default if no logger is
// passed.
//
// We could plumb this through Deps; keeping it implicit keeps the wiring
// short and matches RequireInternalSecret's style.

// dummyHashOnce lazily computes a sentinel bcrypt hash at the configured
// cost. Used in the prefix-not-found branch so that branch takes
// approximately the same time as a real "prefix found, bad hash"
// verification — see W1 in the R2.1 review. The cost here MUST match the
// cost actually used to hash live keys; otherwise the dummy verify is
// cheaper and the timing oracle remains.
var (
	dummyHashOnce sync.Once
	dummyHash     []byte
)

// computeDummyHash hashes a sentinel value at the supplied cost. We accept
// a cost rather than reading a package-global so tests can pass a low
// cost and the production wiring can pass cfg.BcryptCost — the same
// value used by handler.NewAPIKeysHandler.
func computeDummyHash(cost int) []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-skein-api-key-hash-target"), cost)
	if err != nil {
		// MinCost ≤ cost ≤ MaxCost is enforced at config-load time; a
		// failure here means cfg.BcryptCost is wrong, which is a
		// programming error worth surfacing immediately.
		panic("auth: failed to compute dummy api-key hash: " + err.Error())
	}
	return h
}

// constantTimeMissCompare runs a bcrypt verify against the package-level
// dummy hash so the prefix-not-found path takes the same wall-clock time
// as a real prefix-hit-with-wrong-key. The return value is intentionally
// discarded — the call exists for its CPU cost only.
//
// Why this is needed: without it, an attacker submitting candidate
// prefixes can distinguish "prefix not in DB" (one SQL round-trip, μs)
// from "prefix in DB but wrong key" (SQL + bcrypt verify, ~250 ms at
// cost 12). The 12-char prefix space makes enumeration impractical, but
// confirming whether a leaked prefix is still active vs revoked is a
// real ops/audit forensics signal we shouldn't leak. Linux crypt(3)
// has done this for decades.
func constantTimeMissCompare(plain string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plain))
}

// RequireAPIKey returns a middleware that authenticates an MCP request via
// the X-Skein-Api-Key header. On success it stashes:
//
//	UserID, UserRole, IsExternal, APIKeyID — the resolved user identity (lookup → bcrypt)
//	SessionID                              — copied from the Mcp-Session-Id header (may be "")
//
// in the request context, then calls next. On failure it writes a 401 with
// a generic "unauthenticated" body — no info leaks about whether the key
// is unknown, malformed, or revoked.
//
// Surface gate (sk-163 S7a, spec §9 — FAIL-CLOSED): after the key resolves
// to (userID, role, is_external), the request is rejected with 403 when it
// arrived on the PUBLIC surface (HeaderSurface absent / not "internal") AND
// the key owner is NOT external. The marker asserts "internal"; ABSENCE is
// treated as public, so a mis-routed or unmarked request can never reach the
// privileged internal path with an internal-owner key. External-owner keys
// pass on either surface; internal-owner keys pass only on the internal
// surface. The gate runs identically on the cold (DB) and warm (cache) paths
// because is_external rides in the cache entry (S1) — a warm hit must be
// gated exactly like a cold lookup.
//
// Trust chain for the marker: /mcp requires RequireInternalSecret, a secret
// only the BFF holds; the BFF strips every inbound X-Skein-* header before
// adding its own, and stamps HeaderSurface=internal only on the trusted
// internal host. So when this middleware sees the marker it provably came
// from the BFF, not the client.
//
// Lookup path:
//  1. Read header. If missing/short/wrong-tag → 401 (no DB hit).
//  2. Cache hit → stash ctx and dispatch (skips DB and bcrypt). Cache
//     miss falls through to the DB path below.
//  3. ExtractPrefix → LookupByPrefix (single SQL round-trip, joins users).
//  4. bcrypt.Verify(plain, hash). On mismatch → negative-cache + 401.
//  5. TouchLastUsed (throttled in SQL; soft-fail on error — log only).
//  6. Stash ctx, next.
//
// bcryptCost is the cost used to hash live keys. The middleware seeds a
// dummy bcrypt hash at this cost so the prefix-not-found branch takes
// the same time as a real hash mismatch (see W1 mitigation,
// constantTimeMissCompare).
//
// Note: the function returns a constructor so wiring at startup (where
// the pool is built) can stay in main.go without smuggling the pool
// through a global.
func RequireAPIKey(pool *pgxpool.Pool, bcryptCost int, challenge string) func(http.Handler) http.Handler {
	if pool == nil {
		panic("RequireAPIKey: pool is nil")
	}
	store := apikeys.New(pool)
	cache := newAPIKeyCache(apiKeyCacheCapacity, apiKeyCacheTTL, nil)

	// Lazily seed the dummy hash on first request; computing it at boot
	// would block startup for ~250 ms at cost 12. Once-init is fine — by
	// the time the second request arrives, the value is set.
	seedDummy := func() {
		dummyHashOnce.Do(func() {
			dummyHash = computeDummyHash(bcryptCost)
		})
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			plain := strings.TrimSpace(r.Header.Get(HeaderAPIKey))

			// Surface marker (sk-163 S7a). Read once up-front; the
			// fail-closed gate below consults it on whichever path
			// (warm/cold) resolves is_external. Absent → public.
			surfaceInternal := SurfaceIsInternal(r.Header.Get(HeaderSurface))

			// Cheap shape filter — saves a DB round-trip on obviously
			// invalid input (missing header, wrong tag, too short).
			if !IsAPIKeyShape(plain) {
				slog.WarnContext(ctx, "mcp auth failed: missing or malformed api key",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
				)
				writeUnauthenticatedChallenge(w, challenge)
				return
			}

			prefix := ExtractPrefix(plain)
			if prefix == "" {
				writeUnauthenticatedChallenge(w, challenge)
				return
			}

			// Hot path: cache hit returns the resolved identity without
			// touching the DB or running bcrypt. Negative entries (ok=false)
			// short-circuit a typo'd key with a generic 401 — no DB hit
			// either. TTL-bounded so revocation propagates within
			// apiKeyCacheTTL.
			if entry, hit := cache.Get(plain); hit {
				if !entry.ok {
					slog.WarnContext(ctx, "mcp auth failed: cached negative",
						"path", r.URL.Path,
						"masked", MaskAPIKey(plain),
					)
					writeUnauthenticatedChallenge(w, challenge)
					return
				}
				// The cache maps the key to its id only (K-16): its status,
				// the owner's role, external flag, deactivation and deletion
				// are read now, by primary key, so a revocation or a
				// deactivation answers 401 from the next request on.
				status, err := store.Status(ctx, entry.keyID)
				if errors.Is(err, apikeys.ErrNotFound) {
					slog.WarnContext(ctx, "mcp auth failed: key or its user no longer active",
						"path", r.URL.Path,
						"masked", MaskAPIKey(plain),
						"keyId", entry.keyID.String(),
					)
					writeUnauthenticatedChallenge(w, challenge)
					return
				}
				if err != nil {
					slog.ErrorContext(ctx, "mcp auth failed: status read error",
						"path", r.URL.Path,
						"keyId", entry.keyID.String(),
						"error", err.Error(),
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"internal_error"}`))
					return
				}
				// Fail-closed surface gate on the WARM path, on the external
				// flag read just now.
				if surfaceGateRejects(surfaceInternal, status.IsExternal) {
					slog.WarnContext(ctx, "mcp auth failed: internal key on public surface (warm)",
						"path", r.URL.Path,
						"masked", MaskAPIKey(plain),
						"keyId", entry.keyID.String(),
					)
					writeSurfaceForbidden(w)
					return
				}
				// TouchLastUsed is throttled at the SQL layer (1 minute), so
				// hitting it on every cache hit costs at most one UPDATE
				// per key per minute — the same amortisation we'd get
				// without a cache.
				if err := store.TouchLastUsed(ctx, entry.keyID); err != nil {
					slog.WarnContext(ctx, "failed to touch api key last_used_at",
						"keyId", entry.keyID.String(),
						"error", err.Error(),
					)
				}
				ctx = WithUserID(ctx, status.UserID)
				ctx = WithUserRole(ctx, status.UserRole)
				ctx = WithIsExternal(ctx, status.IsExternal)
				ctx = WithAPIKeyID(ctx, entry.keyID)
				if sid := strings.TrimSpace(r.Header.Get(HeaderMcpSessionID)); sid != "" {
					ctx = WithSessionID(ctx, sid)
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Make sure the dummy hash exists before either the not-found or
			// the mismatch branch can execute.
			seedDummy()

			lookup, err := store.LookupByPrefix(ctx, prefix)
			switch {
			case errors.Is(err, apikeys.ErrNotFound):
				// W1: pay the bcrypt cost on prefix-miss so the timing of
				// "prefix not in DB" matches "prefix found, hash mismatch".
				// We do NOT cache this case — keeping the path identical
				// per request makes the timing-oracle mitigation robust
				// against future cache-side changes.
				constantTimeMissCompare(plain)
				slog.WarnContext(ctx, "mcp auth failed: prefix not found",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
				)
				writeUnauthenticatedChallenge(w, challenge)
				return
			case err != nil:
				// DB error — log with context, return 500. Don't 401 here:
				// a transient pool failure shouldn't tell the agent its key
				// is wrong (it would invalidate the cached key in their config).
				slog.ErrorContext(ctx, "mcp auth failed: lookup error",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
					"error", err.Error(),
				)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal_error"}`))
				return
			}

			if err := VerifyAPIKey(lookup.KeyHash, plain); err != nil {
				// Negative-cache wrong keys so a typo doesn't cost bcrypt
				// on every retry. We still return 401 to the client.
				cache.Put(plain, apiKeyCacheEntry{ok: false})
				slog.WarnContext(ctx, "mcp auth failed: hash mismatch",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
					"keyId", lookup.KeyID.String(),
				)
				writeUnauthenticatedChallenge(w, challenge)
				return
			}

			// Successful verify — populate cache so subsequent requests
			// within the TTL window skip bcrypt entirely. The entry maps the
			// key to its id and owner only; the warm path re-reads the
			// status, role and external flag (K-16).
			cache.Put(plain, apiKeyCacheEntry{
				keyID:  lookup.KeyID,
				userID: lookup.UserID,
				ok:     true,
			})

			// Fail-closed surface gate on the COLD path. We cache the entry
			// first (the key IS valid — it's only being used on the wrong
			// surface, and a later internal-surface request should hit the
			// warm path), then reject. External-owner keys pass on either
			// surface; internal-owner keys only on the internal surface.
			if surfaceGateRejects(surfaceInternal, lookup.IsExternal) {
				slog.WarnContext(ctx, "mcp auth failed: internal key on public surface (cold)",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
					"keyId", lookup.KeyID.String(),
				)
				writeSurfaceForbidden(w)
				return
			}

			// Best-effort last_used_at touch. Throttled in SQL so a burst
			// of MCP requests doesn't spam the row; failure here must not
			// block the request — the user is already authenticated.
			if err := store.TouchLastUsed(ctx, lookup.KeyID); err != nil {
				slog.WarnContext(ctx, "failed to touch api key last_used_at",
					"keyId", lookup.KeyID.String(),
					"error", err.Error(),
				)
			}

			// Identity → ctx. Same shape as cookie-auth's ParseUserHeaders
			// so handlers can call UserIDFrom / UserRoleFrom without
			// caring which middleware put it there.
			ctx = WithUserID(ctx, lookup.UserID)
			ctx = WithUserRole(ctx, lookup.UserRole)
			ctx = WithIsExternal(ctx, lookup.IsExternal)
			ctx = WithAPIKeyID(ctx, lookup.KeyID)

			// MCP session id: present on every request EXCEPT the very
			// first initialize call (the SDK generates it server-side and
			// returns it in the response header). Empty is valid; the
			// agents UPSERT skips when missing.
			if sid := strings.TrimSpace(r.Header.Get(HeaderMcpSessionID)); sid != "" {
				ctx = WithSessionID(ctx, sid)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeUnauthenticatedChallenge writes the same generic 401 as
// writeUnauthenticated, preceded by the RFC 9728 §5.1 `WWW-Authenticate`
// challenge that tells the client where this resource's metadata document
// lives (sk-304). The MCP authorization spec makes the header mandatory on a
// 401 from an MCP server and makes it the client's PRIMARY discovery route —
// without it the client has to guess between the root and the path-aware
// well-known URI, and guessing wrong is how sk-304 presented ("resource
// mismatch" → interactive auth → headless agent gives up with a valid key in
// hand).
//
// The challenge carries `resource_metadata` and nothing else. RFC 6750 §3
// adds `error="invalid_token"` only when a credential WAS presented and
// rejected; our 401 covers "no key" and "bad key" identically on purpose
// (see writeUnauthenticated — the body must not let an attacker enumerate the
// failure reason), so a per-reason error code here would leak exactly what
// the shared body withholds.
//
// challenge is precomputed at startup (oauthmeta.Descriptor.Challenge). An
// empty challenge degrades to the plain 401 rather than emitting a malformed
// header — the header is a hint, never the auth decision.
func writeUnauthenticatedChallenge(w http.ResponseWriter, challenge string) {
	if challenge != "" {
		w.Header().Set("WWW-Authenticate", challenge)
	}
	writeUnauthenticated(w)
}

// writeUnauthenticated writes a generic 401. Same body for every failure
// mode (missing header, prefix not found, hash mismatch, revoked key) so
// an attacker cannot enumerate the failure reason.
func writeUnauthenticated(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	if _, err := w.Write([]byte(`{"error":"unauthenticated"}`)); err != nil {
		slog.Warn("write unauthenticated response failed", "error", err)
	}
}

// surfaceGateRejects implements the fail-closed surface decision (sk-163
// S7a, spec §9). It returns true — reject — when the request arrived on the
// PUBLIC surface (no trusted internal marker) AND the resolving key's owner
// is an INTERNAL user. The asymmetry is the whole point: ABSENCE of the
// marker means public, never privileged, so an unmarked/mis-routed request
// can never carry an internal credential into the privileged path.
//
//	surfaceInternal | ownerExternal | result
//	----------------+---------------+--------
//	     true        |     either    | allow (internal surface)
//	     false       |     true      | allow (external key, public OK)
//	     false       |     false     | REJECT (internal key, public surface)
func surfaceGateRejects(surfaceInternal, ownerExternal bool) bool {
	return !surfaceInternal && !ownerExternal
}

// writeSurfaceForbidden writes the 403 for an internal-owner key presented on
// the public surface. Distinct status from writeUnauthenticated (the key IS
// valid — it is simply not permitted on this surface) so ops can tell a
// surface-policy rejection apart from a bad/unknown key in logs and metrics.
func writeSurfaceForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	if _, err := w.Write([]byte(`{"error":"forbidden"}`)); err != nil {
		slog.Warn("write surface forbidden response failed", "error", err)
	}
}

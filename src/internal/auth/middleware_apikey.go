package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
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
//	UserID, UserRole, IsExternal, APIKeyID — the resolved user identity
//	KeyGrant                               — the key's grant as it applies now (RequestGrant)
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
// because is_external is read with the key's status on every request — a
// warm hit is gated exactly like a cold lookup.
//
// Trust chain for the marker: /mcp requires RequireInternalSecret, a secret
// only the BFF holds; the BFF strips every inbound X-Skein-* header before
// adding its own, and stamps HeaderSurface=internal only on the trusted
// internal host. So when this middleware sees the marker it provably came
// from the BFF, not the client.
//
// The decision itself is AuthenticateAPIKey (D-4): this middleware maps its
// outcome to the /mcp wire format — a rejection of the credential, whatever
// its reason, is the same 401 with the challenge; an internal error (the
// lookup or the status read failed) is 500 `{"error":"internal_error"}`, so a
// transient pool failure never tells the agent its key is wrong. The bcrypt
// result is cached; status, owner and grant are read on every request.
//
// bcryptCost is the cost used to hash live keys (see
// NewAPIKeyAuthenticator).
//
// Note: the function returns a constructor so wiring at startup (where
// the pool is built) can stay in main.go without smuggling the pool
// through a global.
func RequireAPIKey(pool *pgxpool.Pool, bcryptCost int, challenge string) func(http.Handler) http.Handler {
	if pool == nil {
		panic("RequireAPIKey: pool is nil")
	}
	authn := NewAPIKeyAuthenticator(pool, bcryptCost)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			plain := strings.TrimSpace(r.Header.Get(HeaderAPIKey))

			out := authn.AuthenticateAPIKey(ctx, plain)
			switch out.Kind {
			case APIKeyRejected:
				slog.WarnContext(ctx, "mcp auth failed",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
					"reason", string(out.Reason),
					"warm", out.Warm,
				)
				writeUnauthenticatedChallenge(w, challenge)
				return
			case APIKeyAuthenticated:
			default:
				slog.ErrorContext(ctx, "mcp auth failed: key lookup or status read error",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
					"warm", out.Warm,
					"error", fmt.Sprint(out.Err),
				)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal_error"}`))
				return
			}
			key := out.Key

			// Fail-closed surface gate (sk-163 S7a), on the external flag
			// read on this request — the same on the warm and cold paths.
			// It belongs to /mcp; the public entry does not apply it.
			if surfaceGateRejects(SurfaceIsInternal(r.Header.Get(HeaderSurface)), key.IsExternal) {
				slog.WarnContext(ctx, "mcp auth failed: internal key on public surface",
					"path", r.URL.Path,
					"masked", MaskAPIKey(plain),
					"keyId", key.KeyID.String(),
					"warm", out.Warm,
				)
				writeSurfaceForbidden(w)
				return
			}

			// Best-effort last_used_at touch. Throttled in SQL (one UPDATE
			// per key per minute); failure must not block the request — the
			// user is already authenticated.
			if err := authn.TouchLastUsed(ctx, key.KeyID); err != nil {
				slog.WarnContext(ctx, "failed to touch api key last_used_at",
					"keyId", key.KeyID.String(),
					"error", err.Error(),
				)
			}

			// Identity → ctx. Same shape as cookie-auth's ParseUserHeaders
			// so handlers can call UserIDFrom / UserRoleFrom without
			// caring which middleware put it there; the grant rides along.
			ctx = WithUserID(ctx, key.UserID)
			ctx = WithUserRole(ctx, key.UserRole)
			ctx = WithIsExternal(ctx, key.IsExternal)
			ctx = WithAPIKeyID(ctx, key.KeyID)
			ctx = WithKeyGrant(ctx, RequestGrant(key))

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

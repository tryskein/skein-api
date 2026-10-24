package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"skein-api/internal/monitorcreds"
)

// monitorValidateTimeout bounds the per-request validate-and-renew call.
// A stuck pool or DB must not pin the poll runner's connection open
// indefinitely; on timeout the call returns a non-sentinel error and the
// middleware maps it to 5xx (the runner retries), never to 401. The
// readiness probe uses a similar short bound (2s); 5s gives renew headroom
// over a momentarily contended pool while still failing fast.
const monitorValidateTimeout = 5 * time.Second

// RequireMonitorToken returns middleware that authenticates a /agent/v1
// request via a short-lived monitor credential (skm_…) presented in the
// X-Skein-Api-Key header. It is the second of the two auth doors (spec §5):
// deliberately NOT an extension of RequireAPIKey, and it shares no parser
// with it.
//
// On success it stashes the validated monitorcreds.Claims in a private
// context key (WithMonitorClaims) — NOT the MCP user-context — then calls
// next. The /agent/v1 handlers read scope via MonitorClaimsFrom. Because
// the MCP identity keys (UserIDFrom / APIKeyIDFrom / …) are never populated
// here, a monitor token cannot masquerade as an api-key caller.
//
// Flow:
//  1. Read X-Skein-Api-Key. If not skm_-shape → 401 (no DB hit). This is
//     what makes an ska_ api key fail at this door.
//  2. monitorcreds.ValidateAndRenew (one atomic UPDATE … RETURNING). This
//     validates audience + every liveness predicate AND slides expires_at
//     server-side in the same statement.
//     - ErrInvalidMonitorToken (0 rows: bad digest, wrong audience,
//     expired, past deadline, revoked, minting key revoked, owner
//     deactivated) → 401. The presenter is never told which predicate
//     failed.
//     - any other error (timeout, conn loss) → 5xx. A transient DB failure
//     must NOT 401: that would make the poll runner discard a valid
//     token instead of retrying.
//  3. Stash claims → ctx, dispatch.
//
// No cache: every request validates-and-renews. Caching would defeat both
// the sliding-expiry renew and revocation (a cached positive would outlive
// a revoke). Cost is one write txn per poll (~25–50/s at 50–100 active
// monitors on a 2s interval) — an accepted budget, not an oversight.
//
// audience scopes the door to a single credential use ("notifications:poll"
// in v1); it is matched inside the UPDATE so a token minted for one audience
// cannot be replayed against another. Returns a constructor so wiring at
// startup keeps the *monitorcreds.Store (and its pool) out of globals,
// matching RequireAPIKey's shape.
func RequireMonitorToken(store *monitorcreds.Store, audience string) func(http.Handler) http.Handler {
	if store == nil {
		panic("RequireMonitorToken: store is nil")
	}
	if strings.TrimSpace(audience) == "" {
		panic("RequireMonitorToken: audience is empty")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			plain := strings.TrimSpace(r.Header.Get(HeaderAPIKey))

			// Cheap shape filter — rejects a missing header, garbage, or an
			// ska_ api key presented at the wrong door without a DB hit.
			if !IsMonitorTokenShape(plain) {
				slog.WarnContext(ctx, "monitor auth failed: missing or malformed token",
					"path", r.URL.Path,
					"masked", MaskMonitorToken(plain),
				)
				writeUnauthenticated(w)
				return
			}

			// Bounded context so a hung pool/DB can't hold the request open
			// past the timeout. On expiry ValidateAndRenew returns a wrapped
			// context error (not ErrInvalidMonitorToken) → 5xx below.
			vctx, cancel := context.WithTimeout(ctx, monitorValidateTimeout)
			defer cancel()

			claims, err := store.ValidateAndRenew(vctx, plain, audience)
			switch {
			case errors.Is(err, monitorcreds.ErrInvalidMonitorToken):
				slog.WarnContext(ctx, "monitor auth failed: invalid credential",
					"path", r.URL.Path,
					"masked", MaskMonitorToken(plain),
				)
				writeUnauthenticated(w)
				return
			case err != nil:
				// DB/internal error — 500, NOT 401. Mirrors the lookup-error
				// branch in RequireAPIKey: a transient pool failure must not
				// tell the caller its token is wrong. The poll runner retries
				// 5xx; it would discard a valid token on 401.
				slog.ErrorContext(ctx, "monitor auth failed: validate error",
					"path", r.URL.Path,
					"masked", MaskMonitorToken(plain),
					"error", err.Error(),
				)
				writeMonitorInternalError(w)
				return
			}

			// Scope → ctx (private key, not MCP user-context). Downstream
			// /agent/v1 handlers read it via MonitorClaimsFrom.
			ctx = WithMonitorClaims(ctx, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeMonitorInternalError writes a generic 500. Matches the body
// RequireAPIKey emits on a DB lookup error so the two doors are
// indistinguishable on internal failure. Kept local so this middleware
// never touches middleware_apikey.go.
func writeMonitorInternalError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":"internal_error"}`))
}

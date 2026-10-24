package handler

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// RequireInternalSecret returns a middleware that 401s every request whose
// X-Skein-Internal-Secret header does not match the configured shared
// secret (skein-web → skein-api channel auth). The comparison uses
// crypto/subtle.ConstantTimeCompare to avoid leaking the secret length or
// matching prefix via response timing.
//
// A blank secret at construction time is a programming error — the
// middleware would silently accept any request. We panic to surface this
// as soon as the binary boots; in practice config.Load enforces a 16-char
// minimum so this branch is defence-in-depth only.
//
// challenge is the RFC 9728 §5.1 `WWW-Authenticate` value to put on the 401
// (sk-304 review F5), or "" for mounts where a protected-resource challenge
// would be a lie. It is per-MOUNT, not global, because this middleware guards
// four different doors: /mcp, whose 401 the MCP authorization spec requires to
// carry the challenge unconditionally — this gate runs BEFORE the api-key gate,
// so without it the first 401 a client can provoke on that door is silent and
// discovery stalls exactly as sk-304 describes — and /api/v1, /agent/v1 and the
// OIDC provision route, whose 401s are cookie/monitor-token failures with no
// OAuth protected resource behind them. Advertising a metadata document there
// would point a client at a resource identifier that does not describe the door
// it was refused at.
func RequireInternalSecret(secret, challenge string) func(http.Handler) http.Handler {
	if secret == "" {
		panic("RequireInternalSecret: SKEIN_INTERNAL_SECRET is empty")
	}
	expected := []byte(secret)
	expectedLen := len(expected)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := r.Header.Get(auth.HeaderInternalSecret)

			// ConstantTimeCompare requires equal-length inputs; pad/truncate
			// to expectedLen so a wrong-length attacker can't short-circuit
			// the comparison. The final ConstantTimeCompare result still
			// catches the length mismatch via the side check below.
			pad := make([]byte, expectedLen)
			copy(pad, provided)
			match := subtle.ConstantTimeCompare(pad, expected)
			if len(provided) != expectedLen {
				match = 0
			}

			if match != 1 {
				slog.WarnContext(r.Context(), "internal secret check failed",
					"path", loggablePath(r.URL.Path),
					"remote", r.RemoteAddr,
				)
				// An empty challenge degrades to the plain 401 rather than
				// emitting a malformed header: the pointer is a hint, never
				// the authorization decision.
				if challenge != "" {
					w.Header().Set("WWW-Authenticate", challenge)
				}
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "invalid internal secret",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// loggablePath is a request path safe for the log (K-11:
// a raw link token is stored nowhere) — every log record written before the
// handler (RequireInternalSecret, ParseUserHeaders, RequireSessionEpoch,
// RequireAdmin) uses it. The gates refuse before any route
// is matched, so they cannot log the route pattern; instead every segment that
// could be a secret — 20 or more characters of the base64url alphabet that
// are not a UUID (a link token is 43) — is replaced by "{redacted}". Ids,
// prefixes and words keep the path readable.
func loggablePath(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if len(seg) >= 20 && isTokenAlphabet(seg) {
			if _, err := uuid.Parse(seg); err != nil {
				segs[i] = "{redacted}"
			}
		}
	}
	return strings.Join(segs, "/")
}

func isTokenAlphabet(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ParseUserHeaders is best-effort: it reads X-Skein-User-* headers and
// stashes parsed values in the request context. Missing or malformed
// headers are silently dropped — the chain still proceeds, and downstream
// guards (RequireUser) are responsible for enforcing presence on routes
// that need it.
//
// This split exists because /auth/login does NOT have a user yet (the
// caller is the BFF doing the login on behalf of someone *not yet
// authenticated*), so the same middleware stack would otherwise need a
// per-route bypass. Better to keep parsing and enforcement separate.
func ParseUserHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if raw := strings.TrimSpace(r.Header.Get(auth.HeaderUserID)); raw != "" {
			if id, err := uuid.Parse(raw); err == nil {
				ctx = auth.WithUserID(ctx, id)
			} else {
				// A malformed user-id header coming from our own BFF is a
				// real bug — log it but don't 401, because RequireUser will
				// catch the missing-context case on routes that care.
				slog.WarnContext(ctx, "X-Skein-User-Id header could not be parsed as UUID",
					"path", loggablePath(r.URL.Path),
				)
			}
		}
		if raw := strings.TrimSpace(r.Header.Get(auth.HeaderUserEmail)); raw != "" {
			ctx = auth.WithUserEmail(ctx, strings.ToLower(raw))
		}
		if raw := strings.TrimSpace(r.Header.Get(auth.HeaderUsername)); raw != "" {
			ctx = auth.WithUsername(ctx, raw)
		}
		if raw := strings.TrimSpace(r.Header.Get(auth.HeaderUserRole)); raw != "" {
			ctx = auth.WithUserRole(ctx, raw)
		}
		// is_external (sk-163 S1): only "true" sets the flag. Any other
		// value (including the absent header and the literal "false") leaves
		// it at the safe internal default — WithIsExternal(false) is a no-op
		// for IsExternalFrom but stamped explicitly for symmetry.
		if raw := strings.TrimSpace(r.Header.Get(auth.HeaderUserIsExternal)); raw != "" {
			ctx = auth.WithIsExternal(ctx, strings.EqualFold(raw, "true"))
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireSessionEpoch checks, on every request that carries a user through
// the X-Skein-User-* headers (ParseUserHeaders), that the session is still
// valid (sk-674.18 K-16, INV-26): one read of the user by primary key —
// active (deactivated_at IS NULL), not deleted (deleted_at IS NULL), and the
// session epoch the BFF sends in X-Skein-Session-Epoch equal to the stored
// one. Anything else — the header absent or not a decimal integer, an older
// epoch, a deactivated or deleted user, no such user — is 401
// `{"error":"session_revoked"}` (auth.CodeSessionRevoked); skein-web ends the
// session on it. No cache stands in front of the read, so a refusal holds
// from the moment the operation that raised the epoch answered, on every
// replica.
//
// The same read carries the user's role and external flag, and they REPLACE
// those of the X-Skein-User-* headers in the request context (sk-674.18
// T037, review m-11, E-14): an admin demoted — or the role taken away —
// loses the admin routes (RequireAdmin) from his very next request, without
// waiting for the BFF to end his session; the session itself stays.
//
// A request without a user (login, OIDC provisioning, the hub batch) passes
// through untouched; API-key requests (/mcp) carry no epoch and are checked
// by their key's and their user's status in auth.RequireAPIKey.
func RequireSessionEpoch(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			userID, ok := auth.UserIDFrom(ctx)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			revoked := func(reason string) {
				slog.InfoContext(ctx, "session revoked", "userId", userID.String(), "reason", reason, "path", loggablePath(r.URL.Path))
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": auth.CodeSessionRevoked})
			}
			raw := strings.TrimSpace(r.Header.Get(auth.HeaderSessionEpoch))
			sent, err := strconv.ParseInt(raw, 10, 64)
			if raw == "" || err != nil {
				revoked("no session epoch")
				return
			}
			var active, external bool
			var epoch int64
			var role string
			err = pool.QueryRow(ctx, `
SELECT deactivated_at IS NULL AND deleted_at IS NULL, session_epoch, role, is_external
  FROM users WHERE id = $1`, userID).Scan(&active, &epoch, &role, &external)
			if errors.Is(err, pgx.ErrNoRows) {
				revoked("no such user")
				return
			}
			if err != nil {
				slog.ErrorContext(ctx, "session check failed", "userId", userID.String(), "error", err.Error())
				writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
				return
			}
			if !active {
				revoked("user deactivated or deleted")
				return
			}
			if sent != epoch {
				revoked("session epoch changed")
				return
			}
			ctx = auth.WithIsExternal(auth.WithUserRole(ctx, role), external)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireUser is a per-route gate that 401s when ParseUserHeaders did not
// populate a user UUID in the context. Apply on routes that operate on
// behalf of a logged-in user (/me, all business endpoints) — NOT on
// /auth/login or /auth/logout, where the user is either not yet known or
// no longer relevant.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserIDFrom(r.Context()); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "unauthenticated",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin gates a route on the caller having `users.role = 'admin'`.
// Compose AFTER RequireUser — this middleware does NOT check presence of
// the user id (RequireUser does), it only checks the role value already
// stashed by ParseUserHeaders. 403 with the Skein-standard ProblemDetails
// shape `{error: "not_admin", message: "..."}` when the role is missing
// or not "admin".
//
// We deliberately respond 403 (Forbidden) rather than 404 (hide existence)
// for admin-only routes: the route's existence is part of the public API
// contract — non-admin users learn that admin features exist by reading
// the OpenAPI spec or by clicking through the UI's hidden tab. Hiding the
// route would not buy us security and would force the BFF to special-case
// 404-vs-genuine-404 on every admin call.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Admin-ceiling derivation (sk-163 S2): an EXTERNAL caller is
		// rejected even when their stored role says admin — is_external
		// clamps the decision via Principal.IsAdmin. PrincipalFrom returns
		// ok=false (and a zero Principal whose IsAdmin is false) when no
		// user identity is in ctx, so a missing role still 403s.
		p, _ := auth.PrincipalFrom(r.Context())
		if !p.IsAdmin() {
			slog.WarnContext(r.Context(), "admin gate denied",
				"path", loggablePath(r.URL.Path),
				"role", p.Role,
				"isExternal", p.IsExternal,
			)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "not_admin",
				"message": "admin role required",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

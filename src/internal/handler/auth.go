package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/storage/generated"
	"skein-api/internal/users"
)

// loginBodyMaxBytes caps the JSON body for /auth/login. Login payloads are
// tiny (two string fields); anything bigger is either an attack or a bug
// and should be rejected before we spend memory on json.Decode.
const loginBodyMaxBytes = 4 * 1024 // 4 KiB

// dummyPasswordPlaintext seeds the timing-leak fix on login: when the
// supplied email doesn't exist, we still run a bcrypt compare against this
// constant so response time doesn't reveal email presence to a probe.
const dummyPasswordPlaintext = "skein-bootstrap-dummy-password-do-not-use"

// Defense-in-depth login limiter thresholds (sk-163 S9). These mirror the
// BFF's primary Redis throttle (skein-web LoginRateLimiter) so the backstop
// trips at the same pressure if the BFF is bypassed. Tuned to never fire on
// normal use: an external collaborator (or break-glass admin) fumbling a
// password a handful of times stays well under the per-email ceiling, and the
// per-IP ceiling sits high enough that a shared NAT egress is not throttled
// while a single-IP spray still is.
const (
	loginLimiterEmailThreshold = 10          // per-account failures before throttle
	loginLimiterIPThreshold    = 30          // per-IP failures (spray) before throttle
	loginLimiterWindow         = time.Minute // rolling fixed window for both buckets
)

// AuthHandler bundles dependencies for the three /auth endpoints (login,
// logout, me). Build once at startup; reuse across requests.
type AuthHandler struct {
	pool       *pgxpool.Pool
	bcryptCost int

	// dummyHash is generated once at construction so login can run a
	// constant-time bcrypt compare against it for non-existent emails (see
	// dummyPasswordPlaintext above). Generating per-request would itself
	// be a timing oracle.
	dummyHash string

	// authMode is "local", "keycloak", or "hybrid".
	//   - local:    every user may password-login (legacy default).
	//   - keycloak: standalone password login is disabled (410 Gone), except
	//     the break-glass admin — see R5.1 plan §8.5.7 + sk-163 S4a.
	//   - hybrid:   password login proceeds, but only is_external accounts
	//     (plus the break-glass admin) actually authenticate; internal users
	//     are funnelled into the generic invalid-credentials failure AFTER
	//     bcrypt so the gate is not a timing oracle.
	// provisioner is the OIDC auto-provisioner backing the internal POST
	// /api/v1/auth/oidc/provision endpoint; nil when authMode == "local"
	// since the route is only registered in keycloak/hybrid mode wiring.
	authMode string

	// breakGlassAdmin is the lowercased email of the single admin permitted
	// to password-login even when OIDC is active (keycloak/hybrid). Empty
	// disables the mechanism. See config.Config.BreakGlassAdmin for the
	// security rationale + trade-off.
	breakGlassAdmin string

	// limiter is the per-pod defense-in-depth login throttle (sk-163 S9).
	// Consulted before any DB lookup or bcrypt so a throttled caller costs
	// nothing. The BFF's Redis limiter is the primary; this is the backstop
	// for a caller that reaches /api/v1/auth/login directly.
	limiter *loginLimiter

	provisioner *auth.Provisioner
}

// NewAuthHandler constructs an AuthHandler, pre-computing a bcrypt hash
// over a constant plaintext so login can branch into Verify on missing
// users without leaking existence via response time.
//
// authMode/provisioner enable R5.1 SSO behaviour:
//   - authMode == "keycloak" → /auth/login responds 410 Gone (except the
//     break-glass admin); authMode == "hybrid" → password login gated to
//     external users (+ break-glass admin); see Login.
//   - provisioner != nil      → /auth/oidc/provision is callable.
//
// breakGlassAdmin is the email allowed to password-login while OIDC is
// active; it is lowercased here so the per-request comparison is a plain
// equality check against the already-lowercased login email. Empty disables
// the mechanism.
//
// Pass authMode="local", breakGlassAdmin="", provisioner=nil for unchanged
// legacy behaviour (this is the default in main.go when SKEIN_AUTH_MODE is
// unset).
func NewAuthHandler(pool *pgxpool.Pool, bcryptCost int, authMode, breakGlassAdmin string, provisioner *auth.Provisioner) (*AuthHandler, error) {
	dummy, err := auth.Hash(dummyPasswordPlaintext, bcryptCost)
	if err != nil {
		return nil, err
	}
	return &AuthHandler{
		pool:            pool,
		bcryptCost:      bcryptCost,
		dummyHash:       dummy,
		authMode:        authMode,
		breakGlassAdmin: strings.ToLower(strings.TrimSpace(breakGlassAdmin)),
		limiter: newLoginLimiter(
			loginLimiterEmailThreshold, loginLimiterIPThreshold, loginLimiterWindow),
		provisioner: provisioner,
	}, nil
}

// loginClientIP resolves the client IP for the per-IP login throttle. It uses
// ONLY the BFF-forwarded X-Skein-Client-Ip (the real client, which skein-web
// resolves from its trusted proxy chain and stamps only when a trusted proxy is
// configured — see sk-163 S9). There is deliberately NO RemoteAddr fallback:
//   - On the normal BFF path, RemoteAddr is the BFF pod IP — keying the per-IP
//     bucket on it would lump every login into one global bucket (a DoS vector).
//   - chi's middleware.RealIP derives RemoteAddr from X-Forwarded-For / X-Real-IP,
//     which a direct caller can spoof — an untrustworthy key.
//
// So an absent/empty header → "" → the per-IP bucket is SKIPPED; the per-email
// bucket still applies unconditionally.
//
// Trust: skein-api trusts X-Skein-Client-Ip only because /api/v1/* is gated by
// RequireInternalSecret (BFF-only credential), and the BFF strips any
// client-supplied X-Skein-* before stamping its own. A different holder of the
// internal secret could forge this header — an accepted internal-trust
// assumption (the same trust we already place in X-Skein-User-*), not a new
// exposure: the per-IP backstop is best-effort, the per-email backstop and the
// BFF remain the primary controls.
func loginClientIP(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(auth.HeaderClientIP))
}

// isBreakGlass reports whether email is the configured break-glass admin.
// email must already be lowercased+trimmed (Login does this before calling).
// Returns false when no break-glass admin is configured.
func (h *AuthHandler) isBreakGlass(email string) bool {
	return h.breakGlassAdmin != "" && h.breakGlassAdmin == email
}

// passwordLoginAllowed decides whether a successfully-verified credential
// pair is permitted to establish a session, given the deployment auth mode
// and the account class. It is consulted ONLY after a real bcrypt verify has
// already succeeded (see Login) so it can never become a pre-bcrypt timing
// oracle:
//   - local:           every account may password-login (unchanged).
//   - keycloak/hybrid:  only external accounts (is_external) plus the single
//     break-glass admin. An internal account in these modes is rejected with
//     the SAME generic invalid-credentials response as a wrong password.
func (h *AuthHandler) passwordLoginAllowed(isExternal bool, email string) bool {
	if h.authMode == "local" {
		return true
	}
	return isExternal || h.isBreakGlass(email)
}

// userResponse is the public shape of a user — never includes
// password_hash, never includes Keycloak subject, never includes raw
// metadata. Used for the response of /auth/login and /auth/me.
type userResponse struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Username    string     `json:"username"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	IsExternal  bool       `json:"isExternal"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastLoginAt *time.Time `json:"lastLoginAt"`
}

// Login authenticates a user via email + password and, on success, updates
// last_login_at. On any failure mode (missing user, bad password, malformed
// hash) it returns 422 Unprocessable Entity with a generic message —
// The project guidelines treat invalid credentials as a business error
// (not 401), and never reveals whether the email exists.
//
// Internal endpoint — does NOT issue a browser session cookie.
//
// This handler is mounted at POST /api/v1/auth/login on skein-api and only
// verifies credentials, returning the user JSON on success. The browser
// cookie session (skein.sid) is established by the BFF route POST
// /api/auth/login on skein-web (see AuthController in skein-web), which
// internally calls this endpoint and then runs SignInAsync to issue the
// encrypted session cookie. Hitting /api/v1/auth/login directly through
// skein-web's ApiProxyMiddleware succeeds with 200 + user JSON but does
// NOT set a Set-Cookie header — testers must use /api/auth/login (no v1)
// for cookie-based auth, or migrate to API-key auth (R2.1) for service-
// to-service callers.
//
// Algorithm:
//  1. Decode {email, password}; reject malformed/oversized JSON with 400.
//  2. Lookup credentials by lowercased email.
//  3. If not found, run bcrypt.Verify against the pre-computed dummy hash
//     so response time matches the "user exists, wrong password" branch,
//     then return 422.
//  4. If found but password_hash is NULL (Keycloak-only user), return 422
//     — standalone login is not available for Keycloak-only accounts.
//  5. bcrypt.Verify the supplied password against the stored hash; on
//     mismatch return 422.
//  6. UPDATE last_login_at (best effort: a failure here logs at WARN but
//     does NOT fail the login — losing a last-login timestamp must not lock
//     a user out).
//  7. Respond 200 with a hash-free userResponse.
//
// Logging: success logs userId + username; failure logs WARN without email
// (to avoid PII in logs).
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// We decode the body BEFORE evaluating the keycloak 410, because the
	// break-glass exemption is keyed on the email — we can't tell whether to
	// shut the password channel until we know who is asking. Decoding first
	// is observationally identical for non-break-glass callers (they still
	// get the 410 below), and body-shape errors (400) are unchanged.
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyMaxBytes)
	defer r.Body.Close()

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "email and password are required",
		})
		return
	}

	// sk-163 S9 — defense-in-depth throttle. Consult the per-pod limiter
	// BEFORE the DB lookup and bcrypt so a throttled caller never costs a
	// hash; the BFF's Redis limiter is the primary, this is the backstop for a
	// caller hitting the API directly. The break-glass admin is throttled here
	// like any other email (its bucket fills via the credential-path failures
	// below), closing the one privileged password oracle. The 429 is a pure
	// function of failure count — it never reveals whether the email exists.
	clientIP := loginClientIP(r)
	if h.limiter.blocked(email, clientIP) {
		slog.WarnContext(ctx, "login throttled by defense-in-depth limiter",
			"authMode", h.authMode,
		)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "rate_limited",
		})
		return
	}

	// R5.1 — in keycloak mode standalone password login is disabled.
	// We return 410 Gone (not 422) because the endpoint is permanently
	// retired for this deployment, not transiently rejecting a single
	// credential pair. The BFF translates this into a redirect to
	// /oauth2/start, but we also surface a human-readable message for
	// direct API consumers.
	//
	// sk-163 S4a — the break-glass admin is the one exception: it keeps a
	// password path open so a fresh OIDC deployment isn't bricked before the
	// first external user exists. A break-glass email falls through to the
	// normal credential path below. This is a deliberate, documented break
	// in the 410-for-everyone shape: in keycloak mode the break-glass email
	// is distinguishable (it gets 422/200, not 410) — acceptable because it
	// is a single configured email and brute force against it is still gated
	// by bcrypt + the S9 rate-limiter. Hybrid mode does NOT take this branch
	// at all (authMode != "keycloak"), so it has no pre-bcrypt email branch.
	if h.authMode == "keycloak" && !h.isBreakGlass(email) {
		slog.WarnContext(ctx, "login attempted in keycloak mode — rejected with 410")
		writeJSON(w, http.StatusGone, map[string]string{
			"error":   "login_disabled",
			"message": "Standalone login disabled. Use SSO.",
		})
		return
	}

	q := generated.New(h.pool)

	creds, err := q.GetUserCredentialsByEmail(ctx, email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Equalize timing with the "wrong password" branch.
		_ = auth.Verify(h.dummyHash, req.Password)
		h.limiter.recordFailure(email, clientIP)
		slog.WarnContext(ctx, "login failed: user not found")
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "invalid credentials",
		})
		return
	case err != nil:
		slog.ErrorContext(ctx, "login failed: db error", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal error",
		})
		return
	}

	if creds.PasswordHash == nil {
		// Keycloak-only user — standalone auth not configured for them.
		// Still verify against the dummy hash to keep timing flat.
		_ = auth.Verify(h.dummyHash, req.Password)
		h.limiter.recordFailure(email, clientIP)
		slog.WarnContext(ctx, "login failed: keycloak-only user attempted standalone login",
			"userId", uuidString(creds.ID),
			"username", creds.Username,
		)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "invalid credentials",
		})
		return
	}

	// R3.1 C1 — reject deactivated users. The deactivation status arrives
	// in the same row as the password hash (single round-trip; see
	// GetUserCredentialsByEmail) so a transient DB error on a separate
	// SELECT cannot make this branch fall through and authenticate a
	// deactivated user. We never tell the caller why login failed
	// (collapse with bad-password into the generic invalid_credentials
	// branch); leaking deactivation status would let an attacker enumerate
	// disabled accounts. Run the dummy verify so timing matches the bad-
	// password path.
	if creds.DeactivatedAt.Valid {
		_ = auth.Verify(h.dummyHash, req.Password)
		h.limiter.recordFailure(email, clientIP)
		slog.WarnContext(ctx, "login failed: user is deactivated",
			"userId", uuidString(creds.ID),
			"username", creds.Username,
		)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "invalid credentials",
		})
		return
	}

	if err := auth.Verify(*creds.PasswordHash, req.Password); err != nil {
		h.limiter.recordFailure(email, clientIP)
		slog.WarnContext(ctx, "login failed: password mismatch",
			"userId", uuidString(creds.ID),
			"username", creds.Username,
		)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "invalid credentials",
		})
		return
	}

	// sk-163 S4a — auth-mode × account-class gate. In keycloak/hybrid mode
	// the password channel authenticates ONLY external accounts (plus the
	// break-glass admin). Crucially this check runs AFTER the bcrypt verify
	// above, not before:
	//
	//   - If we rejected an internal account BEFORE bcrypt, the fast return
	//     would be a timing oracle — "internal account exists" (no bcrypt)
	//     would be distinguishable from "external, wrong password" (bcrypt)
	//     and "no such user" (dummy bcrypt). By the time we reach here a real
	//     bcrypt has already run, so the timing matches the mismatch path.
	//   - The response is the IDENTICAL generic invalid-credentials 422, not
	//     a distinct 403: a non-external account supplying the CORRECT
	//     password is indistinguishable (status + body + timing) from one
	//     supplying a WRONG password. This is what stops the gate from
	//     leaking account class. In local mode passwordLoginAllowed is always
	//     true, so this branch never fires and behaviour is unchanged.
	if !h.passwordLoginAllowed(creds.IsExternal, email) {
		h.limiter.recordFailure(email, clientIP)
		slog.WarnContext(ctx, "login rejected: password channel closed for this account class",
			"userId", uuidString(creds.ID),
			"username", creds.Username,
			"authMode", h.authMode,
		)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "invalid credentials",
		})
		return
	}

	// sk-163 S9 — clear the per-email throttle bucket on success so a user
	// who fumbled a few times does not carry the count forward. The per-IP
	// bucket is intentionally left to expire on its own window.
	h.limiter.resetEmail(email)

	// sk-674.18 K-16: the session is bound to the
	// credential that was verified. The hash was read and checked outside
	// any transaction; a password reset (or a deactivation, a deletion) may
	// have committed since — with a raised epoch the stale proof would carry
	// across the reset. So the user's row is read again under a row lock: if
	// the hash is not the one verified or the account is no longer active,
	// the login is refused like a wrong password; otherwise the last-login
	// stamp and the authentication response (accounts.AuthResponse with the
	// current session epoch, T012) are written and read in that same
	// transaction — a reset waiting on the lock moves the epoch only after
	// this answer's epoch is fixed, and that session then ends by it.
	var resp accounts.AuthResponse
	stale := false
	err = pgx.BeginTxFunc(ctx, h.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var hash *string
		var active bool
		if err := tx.QueryRow(ctx, `
SELECT password_hash, deactivated_at IS NULL AND deleted_at IS NULL FROM users WHERE id = $1 FOR UPDATE`,
			creds.ID).Scan(&hash, &active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				stale = true
				return nil
			}
			return err
		}
		if hash == nil || *hash != *creds.PasswordHash || !active {
			stale = true
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, creds.ID); err != nil {
			return err
		}
		var err error
		resp, err = users.BuildAuthResponse(ctx, tx, uuid.UUID(creds.ID.Bytes))
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "login: binding the session to the verified credential failed",
			"userId", uuidString(creds.ID), "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if stale {
		h.limiter.recordFailure(email, clientIP)
		slog.WarnContext(ctx, "login refused: the credential changed after it was verified",
			"userId", uuidString(creds.ID))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "invalid credentials",
		})
		return
	}
	slog.InfoContext(ctx, "login succeeded",
		"userId", resp.ID,
		"username", resp.Username,
		"role", resp.Role,
	)
	writeJSON(w, http.StatusOK, resp)
}

// Logout is a placeholder for the R5 token-revocation flow. Today it just
// 204s — session lifecycle is owned by skein-web (the BFF), which clears
// its cookie when this returns. We keep the endpoint here so the BFF
// integration shape stays stable across the auth slices.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the authenticated user's profile, refetched from the database
// rather than echoing the X-Skein-User-* headers so a stale cookie can't
// keep showing data the user has since updated. ParseUserHeaders +
// RequireUser middleware guarantee a userID is present in ctx.
//
// @Summary The caller's profile
// @Description The authenticated user, read from the database (never echoed from the session): id, email, username, name, role, isExternal, createdAt, lastLoginAt, and — since sk-674.18 — emailConfirmed (the email is confirmed: an account created by an admin or through SSO, or confirmed by the link; the account tab offers the confirmation email only when false and mail is on) and hasApiKeys (at least one active, not revoked API key — the empty projects column says "Your agent creates projects" when there is none). Read-only.
// @Tags Auth
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} meResponse "The caller"
// @Failure 401 {object} map[string]any "unauthenticated, session_revoked"
// @Failure 422 {object} map[string]any "user not found (the session outlived the account)"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/auth/me [get]
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, ok := auth.UserIDFrom(ctx)
	if !ok {
		// Defensive: RequireUser should have caught this. If we land here,
		// surface a clean 401 rather than panicking.
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "unauthenticated",
		})
		return
	}

	q := generated.New(h.pool)
	row, err := q.GetUserByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// User present in BFF cookie but no longer in DB — treated as
		// "not found" (422 per the business-error rule).
		// The BFF should clear the cookie on its next response.
		slog.WarnContext(ctx, "Me: user from headers not found in DB", "userId", id.String())
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "user not found",
		})
		return
	case err != nil:
		slog.ErrorContext(ctx, "Me: db error", "userId", id.String(), "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal error",
		})
		return
	}

	// sk-674.18 T037 (US-20, contract T013): whether the email is confirmed
	// (the closed set of US-5) and whether the user has an active API key —
	// the account tab and the empty projects column read them.
	var emailConfirmed, hasAPIKeys bool
	if err := h.pool.QueryRow(ctx, `
SELECT u.email_confirmed_at IS NOT NULL,
       EXISTS (SELECT 1 FROM api_keys k WHERE k.user_id = u.id AND k.revoked_at IS NULL)
  FROM users u WHERE u.id = $1`, id).Scan(&emailConfirmed, &hasAPIKeys); err != nil {
		slog.ErrorContext(ctx, "Me: api key lookup failed", "userId", id.String(), "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal error",
		})
		return
	}
	resp := meResponse{
		userResponse: userResponse{
			ID:          id.String(),
			Email:       row.Email,
			Username:    row.Username,
			Name:        row.Name,
			Role:        row.Role,
			IsExternal:  row.IsExternal,
			CreatedAt:   timestamp(row.CreatedAt),
			LastLoginAt: timestampPtr(row.LastLoginAt),
		},
		EmailConfirmed: emailConfirmed,
		HasAPIKeys:     hasAPIKeys,
	}
	writeJSON(w, http.StatusOK, resp)
}

// meResponse is the body of GET /auth/me: the user plus, since sk-674.18
// T037, the two account facts of the contract (spaces/contract.go).
type meResponse struct {
	userResponse
	// EmailConfirmed — the email is confirmed (users.email_confirmed_at).
	EmailConfirmed bool `json:"emailConfirmed"`
	// HasAPIKeys — the user has at least one active (not revoked) API key.
	HasAPIKeys bool `json:"hasApiKeys"`
}

// oidcProvisionMaxBytes caps the JSON body for /auth/oidc/provision. The
// four claims (sub/email/username/name) together fit easily into 4 KiB;
// anything bigger is malformed input from the BFF and should be rejected
// before we allocate memory for json.Decode.
const oidcProvisionMaxBytes = 4 * 1024 // 4 KiB

// OIDCProvision is the R5.1 BFF-only endpoint that turns Keycloak claims
// (already validated by the .NET BFF) into a skein-api user identity.
// Two responsibilities:
//
//  1. Match by keycloak_sub → UPDATE last_login_at + email + name and
//     return the user JSON.
//  2. Not matched + SKEIN_AUTO_PROVISION=true → INSERT a fresh row
//     (role=user, or role=admin when the email matches
//     SKEIN_BOOTSTRAP_ADMIN_EMAIL) and return the user JSON.
//  3. Not matched + auto-provision off → 403 user_not_provisioned.
//
// Route wiring: RequireInternalSecret ONLY. We deliberately do NOT compose
// ParseUserHeaders / RequireUser because the caller has no user yet —
// this is the very call that establishes one. The internal-secret check
// is sufficient: only skein-web BFF holds the secret.
//
// Error mapping (see R5.1 plan §8.5.10):
//   - bad JSON / wrong shape   → 400 invalid_request
//   - any of 4 claims empty    → 422 missing_claims
//   - sub unknown + no auto    → 403 user_not_provisioned
//   - the email of a deleted account (reserved, INV-18) → 403 user_not_provisioned
//   - username clash exhausted → 409 username_conflict
//   - DB / unknown             → 500 internal_error
//   - happy path               → 200 with {id,email,username,role}
func (h *AuthHandler) OIDCProvision(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Defensive: the route is only registered when provisioner is wired,
	// but never panic on a nil dereference if config drift puts us here.
	if h.provisioner == nil {
		slog.ErrorContext(ctx, "oidc provision called but provisioner is nil — refusing")
		writeJSON(w, http.StatusGone, map[string]string{
			"error":   "oidc_disabled",
			"message": "OIDC provision endpoint is not enabled on this deployment.",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, oidcProvisionMaxBytes)
	defer r.Body.Close()

	var req struct {
		Sub      string `json:"sub"`
		Email    string `json:"email"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}

	// Early empty-field check produces a clean 422 before we hit the
	// Provisioner. Required claims are Email + Username — the two
	// values oauth2-proxy forwards via X-Auth-Request-Email and
	// X-Auth-Request-User. Name is OPTIONAL: oauth2-proxy with
	// set_xauthrequest=true does not forward a display-name header,
	// so the BFF sends name="" by design; the Provisioner falls back
	// to username on INSERT and keeps the stored value on UPDATE via
	// COALESCE. Sub is also optional (reserved for R5.x Bearer flow).
	if strings.TrimSpace(req.Email) == "" ||
		strings.TrimSpace(req.Username) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "missing_claims"})
		return
	}

	result, err := h.provisioner.Provision(ctx, auth.ProvisionInput{
		Sub:      req.Sub,
		Email:    req.Email,
		Username: req.Username,
		Name:     req.Name,
	})
	switch {
	case errors.Is(err, auth.ErrMissingClaims):
		// Provisioner-side duplicate of the early check above — same
		// shape so the BFF doesn't have to special-case it.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "missing_claims"})
		return
	case errors.Is(err, auth.ErrUserNotProvisioned), errors.Is(err, auth.ErrIdentityReserved):
		// ErrIdentityReserved (sk-674.18 T041, INV-18): the email belongs to
		// a deleted account — no account is created with it again; the
		// login is refused with the code skein-web already handles.
		slog.WarnContext(ctx, "oidc provision rejected: user not provisioned", "reserved", errors.Is(err, auth.ErrIdentityReserved))
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "user_not_provisioned",
			"message": "Contact admin to grant access",
		})
		return
	case errors.Is(err, auth.ErrUsernameExhausted):
		// Pathological case — every candidate john, john2, … colliding.
		// Return 409 so the BFF surfaces "could not derive unique
		// username" rather than retrying forever on its side.
		slog.WarnContext(ctx, "oidc provision rejected: username suffix loop exhausted")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "username_conflict",
			"message": "Could not derive unique username",
		})
		return
	case errors.Is(err, auth.ErrExternalAccountConflict):
		// sk-163 S5 — the inbound SSO identity matched (or collided with) a
		// LOCAL account flagged is_external=true. Refuse the auto-link: an
		// external collaborator must never be upgraded into an internal SSO
		// identity (spec §8). This is a deliberate refusal, not a server
		// fault, so 403 (not 500). Resolving the collision is an explicit,
		// audited admin operation.
		slog.WarnContext(ctx, "oidc provision refused: matched an external local account — not linking")
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "external_account_conflict",
			"message": "This email belongs to an external account and cannot be linked to SSO. Contact an admin.",
		})
		return
	case err != nil:
		slog.ErrorContext(ctx, "oidc provision failed: internal error", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	// sk-674.18 K-16: the shared authentication response with the current
	// session epoch (accounts.AuthResponse, T012).
	resp, err := users.BuildAuthResponse(ctx, h.pool, result.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "oidc provision: building the response failed",
			"userId", result.UserID.String(), "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// uuidString renders a pgtype.UUID as the canonical 8-4-4-4-12 form,
// returning empty string when not Valid. Centralized so handlers don't
// each reimplement the formatting.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	// pgtype.UUID stores the raw 16 bytes in the same layout as
	// uuid.UUID, so the conversion is free and String() emits the same
	// canonical lowercase 8-4-4-4-12 text the hand-rolled hex loop did.
	return uuid.UUID(u.Bytes).String()
}

func timestamp(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

func timestampPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

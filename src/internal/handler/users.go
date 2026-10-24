package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/person"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// UsersHandler bundles the dependencies for /api/v1/users/* and
// /api/v1/me/password. Build once at startup.
//
// All admin-gated endpoints (/users/*) sit under handler.RequireAdmin in
// the route mount — the handler itself does NOT re-check the role
// because the middleware already returned 403 with the closed-set
// `not_admin` error code. /me/password sits under RequireUser only, so
// any authenticated user can rotate their own password.
type UsersHandler struct {
	pool          *pgxpool.Pool
	store         *users.Store
	projectsStore *projects.Store

	// pwChangeLimiter is the defense-in-depth throttle on POST
	// /me/password (sk-350.2). See the passwordChangeLimiter* constants
	// below for the rationale and the key/threshold choice.
	pwChangeLimiter *loginLimiter
}

// Defense-in-depth throttle for self-service password rotation (sk-350.2).
// /auth/login's loginLimiter defends an ANONYMOUS credential-guessing
// surface (unknown caller, so it keys on the claimed email plus a spray-
// resistant per-IP bucket). This endpoint has no such surface — the caller
// already authenticated, so there is nothing to "guess". What it does have,
// since sk-303.20, is an inflated per-call PRICE: up to two bcrypt-cost
// operations (Verify + Hash) plus, when SKEIN_PWNED_PASSWORDS_ENABLED=true,
// an outbound HTTP call bounded by SKEIN_PWNED_PASSWORDS_TIMEOUT (default
// 2s). An authenticated caller — or a compromised session/API key acting as
// one — can otherwise hold several pool connections and CPU cost just by
// hammering this handle in a loop.
//
// So the key is deliberately the caller's USER ID, not an email or IP:
//   - per-user, because the identity is already established and trusted —
//     it's the natural unit of "how much expensive work has THIS identity
//     spent recently", and it can't be spoofed to spread load the way an
//     anonymous email/IP pair can.
//   - no secondary (IP) bucket: unlike login this isn't gating an
//     anonymous spray vector, so there is no second dimension to defend.
//     The limiter is still constructed with the shared loginLimiter type
//     (ipThreshold=0 — that bucket is never engaged because the IP key
//     passed at every call site below is always "").
//
// Every call counts toward the bucket via recordAttempt — success included
// — because a successful change is exactly as expensive as a rejected one;
// this is why the handler calls recordAttempt (unconditional) rather than
// recordFailure (login's failures-only, reset-on-success variant, which
// would let a caller who always supplies a correct current password bypass
// the budget entirely).
//
// Threshold=5 per 1-minute window: generous enough that a user who mistypes
// their current password once or twice before succeeding — the ordinary
// unhappy-then-happy path — never trips it, while still bounding a looped
// caller to 5 bcrypt-cost round trips per minute per identity.
const (
	passwordChangeLimiterThreshold = 5
	passwordChangeLimiterWindow    = time.Minute
)

// NewUsersHandler constructs a handler with a fresh users.Store wrapping
// the supplied pool, bcrypt cost, and apikeys.Store (for the deactivation
// cascade — see users.Store.Deactivate). The projects.Store is needed by
// /me/memberships which lists project_members rows for the calling user.
func NewUsersHandler(pool *pgxpool.Pool, store *users.Store, projectsStore *projects.Store) *UsersHandler {
	return &UsersHandler{
		pool:          pool,
		store:         store,
		projectsStore: projectsStore,
		pwChangeLimiter: newLoginLimiter(
			passwordChangeLimiterThreshold, 0, passwordChangeLimiterWindow),
	}
}

// ----------------- request shapes -----------------

type createUserRequest struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`

	// IsExternal — sk-163 S8. When true the account is created as an
	// external collaborator (capped at role='user'). Admin-only (the route
	// is RequireAdmin-gated). Defaults to false so existing callers create
	// internal users unchanged. External + role='admin' is rejected by the
	// store with a clean 422 external_cannot_be_admin (ErrExternalCannotBeAdmin);
	// the UI forces role='user' when this is set so that path is a backstop.
	IsExternal bool `json:"isExternal"`

	// SpaceIDs — sk-674.18 T037 (US-6): the spaces (short ids) the account
	// joins at once, the admin's checkboxes. Omitted — the common space, if
	// any; given (even []) — exactly these. Not for an external account.
	SpaceIDs []string `json:"spaceIds"`
}

type updateUserRequest struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Password *string `json:"password"`
}

type changePasswordRequest struct {
	// CurrentPassword is the caller's existing password, verified against
	// the stored bcrypt hash before anything else happens.
	CurrentPassword string `json:"currentPassword"`
	// NewPassword replaces the current one once every check in
	// ChangeMyPassword's algorithm passes. Must be 8..72 bytes once UTF-8
	// encoded; there is no composition rule (NIST SP 800-63B rev. 4).
	NewPassword string `json:"newPassword"`
}

// changePasswordBodyMaxBytes mirrors the tight cap on /auth/login. A
// {currentPassword, newPassword} body is at most a couple hundred bytes;
// the global 1 MiB decodeJSON cap is wasteful and lets a misbehaving
// client stream 1 MiB before json.Decode rejects. Match Login's 4 KiB so
// password endpoints have consistent surface.
const changePasswordBodyMaxBytes = 4 * 1024

// ----------------- helpers -----------------

// adminActor builds the audit Actor for an admin REST call. Mirrors
// caller() in projects.go but emits a users.Actor (Role='human').
func (h *UsersHandler) adminActor(r *http.Request) (users.Actor, bool) {
	id, ok := auth.UserIDFrom(r.Context())
	if !ok {
		return users.Actor{}, false
	}
	username, _ := auth.UsernameFrom(r.Context())
	if username == "" {
		username = "admin"
	}
	return users.HumanActor(id, username), true
}

// mapStoreErr translates users.* sentinel errors into the
// ProblemDetails-style HTTP response. Returns true when the error was
// recognised; the caller falls through to 500 only when this returns
// false.
func (h *UsersHandler) mapStoreErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		writeProblem(w, http.StatusUnprocessableEntity, "user_not_found", "user not found", nil)
		return true
	case errors.Is(err, users.ErrEmailTaken):
		writeProblem(w, http.StatusUnprocessableEntity, "email_taken", "email already in use", nil)
		return true
	case errors.Is(err, users.ErrUsernameTaken):
		writeProblem(w, http.StatusUnprocessableEntity, "username_taken", "username already in use", nil)
		return true
	case errors.Is(err, spaces.ErrConcurrentChange):
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeConcurrentChange,
			"a concurrent membership change won; retry", nil)
		return true
	case errors.Is(err, users.ErrInvalidPassword):
		writeProblem(w, http.StatusBadRequest, "invalid_password",
			"password must be at least 8 characters", nil)
		return true
	case errors.Is(err, users.ErrInvalidRole):
		writeProblem(w, http.StatusBadRequest, "invalid_role",
			"role must be 'admin' or 'user'", nil)
		return true
	case errors.Is(err, users.ErrLastAdmin):
		writeProblem(w, http.StatusUnprocessableEntity, "last_admin_cannot_demote",
			"would leave zero active admins", nil)
		return true
	case errors.Is(err, users.ErrInvalidCredentials):
		// 422 (not 401) per the project guidelines — 401 is reserved for
		// api-key auth failures. Login uses 422 invalid_credentials for
		// the same logical condition, so ChangePassword aligns with that.
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_credentials",
			"current password is incorrect", nil)
		return true
	case errors.Is(err, users.ErrValidation):
		writeProblem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return true
	case errors.Is(err, users.ErrAdminRoleDisabled):
		// sk-674.18 T037, INV-20: no path assigns the admin role while
		// SKEIN_INSTANCE_ADMIN_ENABLED=false.
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeAdminRoleDisabled,
			"the instance admin role is disabled on this installation", nil)
		return true
	case writeSpacesRefusal(w, err):
		// The spaces named at creation (T037): space_not_found,
		// personal_space, external_user, no_seats (spaceId), …
		return true
	case errors.Is(err, users.ErrExternalCannotBeAdmin):
		// sk-163 S3 — external user can never be admin. Business rule → 422.
		writeProblem(w, http.StatusUnprocessableEntity, "external_cannot_be_admin",
			"external users cannot be assigned the admin role", nil)
		return true
	case errors.Is(err, users.ErrPasswordPwned):
		// sk-303.20 — new password's hash was found in the Pwned Passwords
		// k-anonymity corpus. Business rule (NIST SP 800-63B rev. 4 SHALL
		// compare against a list of unacceptable passwords), not a syntax
		// error → 422, same family as invalid_credentials above.
		writeProblem(w, http.StatusUnprocessableEntity, "password_pwned",
			"this password has appeared in a known data breach; choose a different one", nil)
		return true
	case errors.Is(err, users.ErrPasswordTooLong):
		// sk-303.20 — bcrypt's structural 72-byte ceiling, checked before
		// any bcrypt call. Reachable with as few as 19 emoji characters, so
		// this is a real client-facing case, not a theoretical one → 400,
		// same family as invalid_password below (both are input shape
		// failures, not business rules).
		writeProblem(w, http.StatusBadRequest, "password_too_long",
			"password must not exceed 72 bytes once UTF-8 encoded", nil)
		return true
	case errors.Is(err, users.ErrLocalPasswordDisabled):
		// sk-163 S4a — local password management is closed for an internal
		// account while OIDC is active. Authorization denial (the operation
		// is not permitted for this account class), so 403 — parallels the
		// not_admin 403 from RequireAdmin, not a 422 business-validation.
		writeProblem(w, http.StatusForbidden, "local_password_disabled",
			"local password management is disabled for this account; this user authenticates via SSO", nil)
		return true
	case errors.Is(err, users.ErrExternalSSOConflict):
		// sk-163 S5 — external account requested for an SSO-bound email.
		// Business rule (not a syntax error) → 422.
		writeProblem(w, http.StatusUnprocessableEntity, "external_sso_conflict",
			"this email is already bound to an SSO identity and cannot be used for an external account", nil)
		return true
	case errors.Is(err, users.ErrPasswordChangedConcurrently):
		// sk-303.20 review remediation — ChangePassword's compare-and-swap
		// UPDATE found password_hash already different from the hash it
		// verified `current` against: another request rotated the password
		// in the window between the read and the write. A genuine race, not
		// a caller mistake and not an internal failure → 422 (business
		// condition, retryable), never the generic 500 a bare "0 rows
		// affected" would otherwise fall through to.
		writeProblem(w, http.StatusUnprocessableEntity, "password_change_conflict",
			"your password was changed by another request; please try again", nil)
		return true
	}
	return false
}

// ----------------- endpoints -----------------

// Create handles POST /api/v1/users. Admin-only via the RequireAdmin
// middleware on the route group. Accepts {email, username?, name, password,
// role} and returns the created UserDTO with status 201. Errors:
//
//   - 400 invalid_password / invalid_role / invalid JSON.
//   - 422 email_taken / username_taken.
//   - 422 concurrent_change — the new account joins the common space under
//     its lock (K-7); a conflict with a concurrent space operation is
//     retried, and this code means the retries ran out (retry the request).
//   - 422 admin_role_disabled — role admin while the instance admin role is
//     disabled (INV-20).
//   - spaceIds (T037): each space joined at once, its seats checked — 404
//     space_not_found, 422 personal_space, external_user, no_seats (spaceId).
//     Omitted — the common space by the flag; given — exactly these.
//
// @Summary Create a user (admin)
// @Description Creates an account that can sign in at once with the password given; the email counts as confirmed. A non-external account gets its personal space; spaceIds omitted — it also joins the common space when SKEIN_COMMON_SPACE_ENABLED; spaceIds given (even []) — it joins exactly those spaces, each at once without consent (the admin's direct addition: seats checked, signal spaces added). An external account gets no space. Role admin is refused while the instance admin role is disabled (INV-20). Side effects: user.created event, the spaces' memberships and signals. Admin only.
// @Tags Users
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body createUserRequest true "email, username, name, password, role, isExternal, spaceIds"
// @Success 201 {object} users.UserDTO "The new user"
// @Failure 400 {object} map[string]any "invalid_json, invalid_password, invalid_role, validation_error, password_too_long"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 404 {object} map[string]any "space_not_found (a spaceId)"
// @Failure 422 {object} map[string]any "admin_role_disabled, email_taken, username_taken, external_cannot_be_admin, external_user (spaceIds for an external account), personal_space, no_seats (spaceId), concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users [post]
func (h *UsersHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := h.adminActor(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	dto, err := h.store.CreateUser(ctx, users.CreateUserInput{
		Email:      strings.TrimSpace(strings.ToLower(req.Email)),
		Username:   strings.TrimSpace(strings.ToLower(req.Username)),
		Name:       strings.TrimSpace(req.Name),
		Password:   req.Password,
		Role:       strings.TrimSpace(req.Role),
		IsExternal: req.IsExternal,
		SpaceIDs:   req.SpaceIDs,
	}, actor)
	if err != nil {
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "create user failed",
			"actorUserId", actor.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "user created",
		"actorUserId", actor.UserID.String(),
		"createdUserId", dto.ID.String(),
		"username", dto.Username,
		"role", dto.Role,
	)
	writeJSON(w, http.StatusCreated, dto)
}

// List handles GET /api/v1/users. Admin-only. Cursor pagination with
// optional ?archived=false|true|all, ?search=substring (ILIKE on
// email/username/name), and ?external=true|false (sk-163 — filter on the
// is_external flag; absent = both classes). Default archived=false.
func (h *UsersHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	in := users.ListUsersInput{
		Archived: q.Get("archived"),
		Search:   q.Get("search"),
	}
	// sk-163 S8 — optional ?external=true|false filter (admin-only route).
	// Absent → nil → no filter. Any other value is rejected so a typo'd
	// param can't silently fall through to "all".
	if ext := q.Get("external"); ext != "" {
		switch ext {
		case "true":
			v := true
			in.External = &v
		case "false":
			v := false
			in.External = &v
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_external",
				"external must be 'true' or 'false'", nil)
			return
		}
	}
	if cursor := q.Get("cursor"); cursor != "" {
		t, id, err := users.DecodeCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", err.Error(), nil)
			return
		}
		in.CursorTime = &t
		in.CursorID = &id
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 100 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be an integer in [1, 100]", nil)
			return
		}
		in.Limit = n
	}

	res, err := h.store.ListUsers(ctx, in)
	if err != nil {
		slog.ErrorContext(ctx, "list users failed", "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Get handles GET /api/v1/users/{id}. Admin-only. 422 user_not_found.
func (h *UsersHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	dto, err := h.store.GetUser(ctx, id)
	if err != nil {
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "get user failed", "userId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Update handles PATCH /api/v1/users/{id}. Admin-only. Partial update of
// name / role / password. Last-admin protection on role demotion is
// enforced inside the store via SERIALIZABLE tx + recheck.
//
// @Summary Update a user (admin)
// @Description Patches name, role and password of the user. Promotion to admin is refused while the instance admin role is disabled (INV-20) and for an external user; demoting the last active admin is refused. A demotion takes away at once the projects the user saw only as the admin — with his open assignments, claims, watches and reminders there (INV-12, reason admin_demoted) — and the admin routes from his next request; his spaces, the common one he may own included, stay. A change that loses a serialization race is retried (three attempts, K-7); after that 422 concurrent_change — nothing is changed, repeat the request. Admin only.
// @Tags Users
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "User UUID"
// @Param body body updateUserRequest true "Fields to change"
// @Success 200 {object} users.UserDTO "The updated user"
// @Failure 400 {object} map[string]any "invalid_id, invalid_json, invalid_password, invalid_role, password_too_long"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin, local_password_disabled"
// @Failure 422 {object} map[string]any "admin_role_disabled, concurrent_change, external_cannot_be_admin, last_admin_cannot_demote, user_not_found, username_taken"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/{id} [patch]
func (h *UsersHandler) Update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := h.adminActor(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	var req updateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	dto, err := h.store.UpdateUser(ctx, id, users.UpdateUserInput{
		Name:     req.Name,
		Role:     req.Role,
		Password: req.Password,
	}, actor)
	if err != nil {
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "update user failed",
			"actorUserId", actor.UserID.String(),
			"userId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "user updated",
		"actorUserId", actor.UserID.String(),
		"userId", id.String(),
		"changedRole", req.Role != nil,
		"changedPassword", req.Password != nil,
	)
	writeJSON(w, http.StatusOK, dto)
}

// Deactivate handles POST /api/v1/users/{id}/deactivate. Admin-only.
// Returns 204 on success; cascades into apikeys soft-revocation in the
// same tx. 422 last_admin_cannot_demote when target is the only active
// admin.
//
// @Summary Deactivate a user (admin)
// @Description Blocks the account: one SERIALIZABLE transaction sets deactivated_at, raises the session epoch (every browser session is refused from its next request) and revokes every active API key; memberships, assignments and spaces stay (INV-12: deactivation is not a loss of access). Deactivating an already deactivated user is a no-op answered 204.
// @Description Side effects: the user.deactivated event (skein-web drops the user's session tickets and hub groups) and personal signals.
// @Description Refusals: the only active admin → 422 last_admin_cannot_demote; a serialization conflict with a concurrent change is retried (three attempts, K-7), after that 422 concurrent_change — nothing is changed, repeat the request. Admin only.
// @Tags Users
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "User UUID"
// @Success 204 "Deactivated (or already deactivated)"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 422 {object} map[string]any "concurrent_change, last_admin_cannot_demote, user_not_found"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/{id}/deactivate [post]
func (h *UsersHandler) Deactivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := h.adminActor(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if err := h.store.Deactivate(ctx, id, actor); err != nil {
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "deactivate user failed",
			"actorUserId", actor.UserID.String(),
			"userId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "user deactivated",
		"actorUserId", actor.UserID.String(),
		"userId", id.String(),
	)
	w.WriteHeader(http.StatusNoContent)
}

// Reactivate handles POST /api/v1/users/{id}/reactivate. Admin-only.
// Clears deactivated_at; does NOT re-arm previously revoked API keys.
//
// @Summary Reactivate a user (admin)
// @Description Lets a deactivated account sign in again: deactivated_at is cleared. Sessions issued before the deactivation stay refused (the session epoch is not lowered) and revoked API keys stay revoked. Reactivating an active user is a no-op answered 204.
// @Description Side effects: the user.reactivated event and personal signals.
// @Description Refusals: a serialization conflict with a concurrent change is retried (three attempts, K-7), after that 422 concurrent_change — nothing is changed, repeat the request. Admin only.
// @Tags Users
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "User UUID"
// @Success 204 "Reactivated (or already active)"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 422 {object} map[string]any "concurrent_change, user_not_found"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/{id}/reactivate [post]
func (h *UsersHandler) Reactivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := h.adminActor(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if err := h.store.Reactivate(ctx, id, actor); err != nil {
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "reactivate user failed",
			"actorUserId", actor.UserID.String(),
			"userId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "user reactivated",
		"actorUserId", actor.UserID.String(),
		"userId", id.String(),
	)
	w.WriteHeader(http.StatusNoContent)
}

// @Summary Change the caller's own password
// @Description Rotates the CALLER's own password (self-service, not admin-gated — any authenticated user, including external collaborators, may call this for themselves). Algorithm: (1) validate the new password's length is within [8, 72] bytes once UTF-8 encoded; (2) load the caller's row with a plain read (no open transaction); (3) if OIDC is active (SKEIN_AUTH_MODE=keycloak/hybrid) and the account is internal and not the break-glass admin, reject — local password management is disabled for that account class (it must rotate via SSO); (4) if the account is deactivated, or has no local password hash (Keycloak-only), or the supplied current password does not verify against the stored bcrypt hash, reject with the SAME invalid_credentials code in all three cases (deliberate — a caller must not be able to distinguish "deactivated" / "SSO-only" / "wrong password" from the response); (5) ONLY once the current password is verified, and only when SKEIN_PWNED_PASSWORDS_ENABLED=true, check the NEW password's SHA-1 hash against the Pwned Passwords k-anonymity range API (only a 5-character hash prefix ever leaves the process) — a match rejects the change without writing anything; a network/timeout failure on that check is logged and does NOT block the change; none of this touches the database — no connection or transaction is held across the outbound call; (6) bcrypt-hash the new password (cost from SKEIN_BCRYPT_COST); (7) commit with a short compare-and-swap UPDATE (WHERE password_hash = the hash verified in step 4) — if another request rotated the password in the meantime, this affects 0 rows and the call rejects with password_change_conflict instead of silently clobbering an unverified hash.
// @Description
// @Description Side effects: writes a users.password_changed audit event in the same short transaction as the compare-and-swap UPDATE (attributed to the caller). No webhook, no other system is notified.
// @Description
// @Description There is no rule on password COMPOSITION (no required uppercase/digit/symbol) per NIST SP 800-63B rev. 4 — only length is enforced server-side. A live strength estimate (zxcvbn) and an optional client-side breach hint are shown by the frontend before submit, but neither is enforced here; this endpoint is the actual point of enforcement for the breach check.
// @Tags Users
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body changePasswordRequest true "Current and new password"
// @Success 204 "Password changed"
// @Failure 400 {object} map[string]any "invalid_json — malformed body; invalid_password — new password shorter than 8 bytes; password_too_long — new password exceeds 72 bytes once UTF-8 encoded (bcrypt's structural ceiling)"
// @Failure 401 {object} map[string]any "unauthenticated — missing/invalid caller identity"
// @Failure 403 {object} map[string]any "local_password_disabled — account authenticates via SSO and has no local password to rotate"
// @Failure 422 {object} map[string]any "invalid_credentials — current password did not verify (or the account is deactivated / SSO-only, collapsed into the same code to avoid account-state enumeration); password_pwned — new password found in the Pwned Passwords breach corpus (only when SKEIN_PWNED_PASSWORDS_ENABLED=true); password_change_conflict — another request rotated the password between the read and the write (retry with the new current password)"
// @Failure 429 {object} map[string]any "rate_limited — defense-in-depth throttle (sk-350.2): more than 5 calls from this caller within the current 1-minute window. Consulted before the body is even read, so a throttled caller never pays for JSON decode, bcrypt, or the pwned-passwords network call. Retry after the window elapses."
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/password [post]
func (h *UsersHandler) ChangeMyPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	// sk-350.2 (round 2, claude review 2026-09-05) — check-and-RESERVE in
	// ONE atomic step, consulted before the body is even read (let alone
	// the store's bcrypt/network work) so a throttled caller costs
	// ~nothing. Keyed on the caller's own user id — see
	// passwordChangeLimiter* above for why this differs from the login
	// limiter's email/IP pair.
	//
	// This USED to be blocked() here and recordAttempt() only after
	// h.store.ChangePassword returned — two separate lock acquisitions
	// with the full bcrypt-cost verify+hash (and, with
	// SKEIN_PWNED_PASSWORDS_ENABLED, an outbound network call) running
	// in between. Every concurrent caller in that window observed the
	// bucket still under threshold, because none of them had recorded
	// yet — a fixed-window cap bounds a caller looping SEQUENTIALLY, but
	// not N simultaneous requests from one identity, which is exactly
	// the "hold pool connections cheaply by hammering the endpoint"
	// vector this limiter exists to bound (see the type doc above), just
	// via parallelism instead of a loop. tryAcquire folds the check and
	// the reservation into the SAME lock acquisition — see its own doc
	// comment — so at most passwordChangeLimiterThreshold callers can
	// ever be admitted per window regardless of how many arrive at once.
	//
	// What counts is unchanged from before (every call that gets this
	// far, success or failure — see tryAcquire's doc for why this is not
	// the login limiter's failures-only policy); reserving before the
	// body is even decoded now also counts a malformed-JSON body toward
	// the same budget, which was previously free. That is a strictly
	// SMALLER surface than before (a slot spent on garbage is a slot an
	// attacker cannot also spend on a real bcrypt-cost attempt), not a
	// new one, and no acceptance criterion or test in this file relies
	// on invalid JSON being exempt from the count.
	callerKey := userID.String()
	if !h.pwChangeLimiter.tryAcquire(callerKey) {
		slog.WarnContext(ctx, "password change throttled by defense-in-depth limiter",
			"userId", callerKey)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, changePasswordBodyMaxBytes)
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	err := h.store.ChangePassword(ctx, userID, req.CurrentPassword, req.NewPassword)
	if err != nil {
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "change password failed",
			"userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "user password changed",
		"userId", userID.String(),
	)
	w.WriteHeader(http.StatusNoContent)
}

// passwordPolicyResponse is the GET /me/password/policy payload.
type passwordPolicyResponse struct {
	// PwnedCheckEnabled reports whether the server will run the Pwned
	// Passwords k-anonymity check against a new password on
	// POST /me/password (SKEIN_PWNED_PASSWORDS_ENABLED). The frontend uses
	// this to decide whether to run its OWN client-side breach hint lookup
	// — it must not dial out to a third-party breach-check service when
	// the operator has not opted in server-side, even just for a hint.
	PwnedCheckEnabled bool `json:"pwnedCheckEnabled"`
}

// @Summary Self-service password change policy
// @Description Reports whether the server-side Pwned Passwords k-anonymity check is enabled (SKEIN_PWNED_PASSWORDS_ENABLED) — the SINGLE source of truth for "does this deployment call out to a breach-check service". The frontend's password-change page reads this once on mount and, ONLY when true, wires up its own client-side hint lookup (a browser fetch to the breach-check API on blur of the new-password field); when false, no such lookup is attempted at all, so a deployment that has not opted in server-side never makes an unexpected outbound call from the browser either. Mirrors the existing GET /push/config pattern: always 200, a plain feature-detection flag, no PII, no secrets. Available to every authenticated user (not admin-only) — same audience as the endpoint it gates.
// @Description
// @Description sk-350.2 — deliberately NOT rate-limited, unlike POST /me/password: this handler does exactly one nil-check (Store.PwnedCheckEnabled) with no DB round trip, no bcrypt, and no outbound call, so there is no expensive-work budget to protect. Throttling it would add complexity without a corresponding cost to bound.
// @Tags Users
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} passwordPolicyResponse "Current password-change policy"
// @Failure 401 {object} map[string]any "unauthenticated — missing/invalid caller identity"
// @Router /api/v1/me/password/policy [get]
func (h *UsersHandler) PasswordPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFrom(r.Context()); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, passwordPolicyResponse{
		PwnedCheckEnabled: h.store.PwnedCheckEnabled(),
	})
}

// PublicProfileDTO is the wire shape returned by GET
// /api/v1/users/profile/{username}. R4.3 — supports the user profile page
// wired up to mention links. Available to any authenticated user who may
// see the person (INV-9).
//
// sk-674.18 T027, INV-27: the email is the viewer's own only — another
// person's email lives on the admin screens (GET /users, /users/{id}), and
// so does the deactivation stamp; a profile is not an admin screen (the
// decision of 2026-10-14).
type PublicProfileDTO struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	// Email is present on the viewer's own profile only.
	Email *string `json:"email,omitempty"`
	// The person marks (spaces/contract.go): a deleted account is marked
	// deleted with its pseudonym.
	person.Status
}

// GetUserProfile handles GET /api/v1/users/profile/{username}. Public
// (RequireUser-only — no admin gate). Returns the canonical username,
// display name, role and person marks of a user the caller may see by
// INV-9 (sk-674.18 T027): a participant of one of the caller's spaces, or
// an external member of a project the caller can access; an external caller
// sees the co-members of his projects. An instance admin follows the same
// rule (US-6).
//
// Errors:
//   - 422 user_not_found — internal caller, username matches no row OR the
//     caller may not see the person (via mapStoreErr)
//   - 404 user_not_found — external caller, the same two cases
//
// The two cases are one statement and one answer per caller class
// (users.Store.GetVisibleProfile), so neither the body nor the number of
// queries tells a missing username from an invisible person.
//
// @Summary Public profile of a person the caller may see
// @Description Returns id, username, display name, role and the person marks (`deleted` + `pseudonym` of a deleted account) of a user visible to the caller by INV-9: a participant of one of the caller's spaces, or an external member of a project the caller can access; an external caller sees only the co-members of his projects. The instance admin follows the same rule — the full list of people is the admin screen GET /api/v1/users.
// @Description
// @Description `email` is present on the caller's own profile only (INV-27). A username that does not exist and a person the caller may not see answer the same way: 422 `user_not_found` to an internal caller, 404 `user_not_found` to an external one.
// @Tags Users
// @Produce json
// @Security ApiKeyAuth
// @Param username path string true "Canonical username"
// @Success 200 {object} PublicProfileDTO
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "user_not_found — external caller: no such user, or not visible"
// @Failure 422 {object} map[string]any "user_not_found — internal caller: no such user, or not visible"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/profile/{username} [get]
func (h *UsersHandler) GetUserProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := strings.TrimSpace(chi.URLParam(r, "username"))
	if username == "" {
		// chi/path: empty path-segment shouldn't reach here, but guard.
		writeProblem(w, http.StatusBadRequest, "invalid_username",
			"username path segment is required", nil)
		return
	}
	callerID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	p, err := h.store.GetVisibleProfile(ctx, callerID, username)
	if err != nil {
		if auth.IsExternalFrom(ctx) && errors.Is(err, users.ErrUserNotFound) {
			writeProblem(w, http.StatusNotFound, "user_not_found", "user not found", nil)
			return
		}
		if h.mapStoreErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "get user profile failed",
			"username", username, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	out := PublicProfileDTO{
		ID:       p.ID.String(),
		Username: p.Username,
		Name:     p.Name,
		Role:     p.Role,
		Status:   p.Status,
	}
	if p.ID == callerID {
		email := p.Email
		out.Email = &email
	}
	writeJSON(w, http.StatusOK, out)
}

// ListDirectory handles GET /api/v1/users/directory. Non-admin
// (RequireUser-only — no admin gate): returns a lean id/username/name
// PAGE of ACTIVE users, ordered by username. Powers the assignee picker
// on the cross-project "All projects" Tasks surface, where there is no
// single project membership to source the picker from.
//
// No admin gate by design: usernames + display names are already public
// across the product (mentions, the assignee column, the watchers list),
// so the directory exposes only that already-public triple. Email, role,
// and auth-method stay on the admin-only GET /api/v1/users endpoint.
//
// sk-285.5.5 — CONTRACT CHANGE, deliberate, in two parts:
//
//  1. The response is no longer a bare array. It is
//     `{"items":[…],"hasMore":bool}` — the envelope every other bounded
//     listing in this service uses. A bounded page with no completeness
//     flag is a silent truncation, and there is nowhere to put that flag
//     in a bare array. Clients that did `res.map(...)` must read
//     `res.items`.
//  2. The page is bounded (default users.DirectoryDefaultLimit, ceiling
//     users.DirectoryMaxLimit) and searchable (?q=). A call with NO
//     parameters is still valid and still returns the alphabetically
//     first users — it just no longer returns ALL of them. Before this
//     change the endpoint streamed every active row on every picker
//     mount: with tens of thousands of active users that was
//     a multi-megabyte JSON body per request, produced by a full seq scan + sort.
//     Callers that need a specific person now search for them.
//
// @Summary List active users (directory) — searchable, bounded page
// @Description Lean id + username + display-name page of ACTIVE (non-deactivated) users. Available to ANY authenticated caller (NOT admin-gated) — it backs assignee pickers, and usernames/display names are already public via mentions. Email, role, and auth-method are deliberately omitted (admin-only via GET /api/v1/users).
// @Description
// @Description Response is an envelope: `{"items":[{id,username,name}], "hasMore":bool}`. `hasMore` is true when more rows matched than the page returned — refine `q` or raise `limit`. There is no cursor: the directory is a typeahead source, you narrow it, you do not page through it.
// @Description
// @Description `q` (optional): case-insensitive match against username AND display name. Absent/blank = no filter. Wildcards are literal — `%` and `_` in `q` match themselves. At most 128 characters; longer is rejected with 400 `invalid_search` rather than truncated.
// @Description
// @Description `q` matching mode depends on the term's LENGTH. Three characters or more = SUBSTRING match, ranked exact-username > username-prefix > name-prefix > any other substring hit, then alphabetically by username. One or two characters = PREFIX match ("starts with"), ranked exact-username > username-prefix, then username ASC. The split is not cosmetic: the trigram indexes that serve substring search cannot serve a term with no complete trigram in it, and a typeahead's first keystrokes are exactly that case — see migration 000038. Without `q` the order is plain username ASC.
// @Description
// @Description `limit` (optional): page size. Default 50, minimum 1, maximum 100. A non-integer or out-of-range value is rejected with 400 `invalid_limit` rather than silently clamped.
// @Description
// @Description The page holds only the people visible to the caller by INV-9 (sk-674.18): participants of the caller's spaces and external members of projects the caller can access; an EXTERNAL caller (is_external) sees only the co-members of his projects. The instance admin follows the same rule — the full list of people is the admin screen GET /api/v1/users. Each row may carry the person marks `deleted` + `pseudonym`.
// @Description
// @Description BREAKING (sk-285.5.5): this endpoint previously returned a BARE ARRAY of EVERY active user with no limit and no search.
// @Tags Users
// @Produce json
// @Security ApiKeyAuth
// @Param q query string false "Case-insensitive match on username and display name — substring for terms of 3+ characters, prefix for 1-2. Absent = no filter. Max 128 characters."
// @Param limit query int false "Page size, 1..100" default(50)
// @Success 200 {object} users.DirectoryResult "Bounded page of active users"
// @Failure 400 {object} map[string]any "invalid_limit — limit is not an integer in [1, 100]; invalid_search — q is longer than 128 characters"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/directory [get]
func (h *UsersHandler) ListDirectory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	in := users.DirectoryInput{Search: r.URL.Query().Get("q")}
	// sk-285.5.5 review F6 — an over-long `q` is a 400, in the same shape as
	// the `limit` rejection right below (explicit refusal, never a silent
	// truncation: a caller must not get results for a term it did not send).
	// Any authenticated caller reaches this endpoint, and `q` was previously
	// forwarded to the query builder at whatever length it arrived — a
	// multi-kilobyte term is a multi-kilobyte LIKE pattern evaluated per row,
	// on a query whose short-term branch is deliberately not index-served.
	if users.DirectorySearchTooLong(in.Search) {
		writeProblem(w, http.StatusBadRequest, "invalid_search",
			"q must be at most "+strconv.Itoa(users.DirectorySearchMaxRunes)+" characters", nil)
		return
	}
	// Out-of-range / non-numeric limit is a 400, not a silent clamp — same
	// contract as the sibling admin listing (UsersHandler.List) so the two
	// user-listing endpoints cannot disagree about what `limit=0` means.
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > users.DirectoryMaxLimit {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be an integer in [1, "+strconv.Itoa(users.DirectoryMaxLimit)+"]", nil)
			return
		}
		in.Limit = n
	}

	// sk-674.18 T027 (INV-9): one rule for every caller — the people
	// visible to him (users.Store.ListDirectory over authz.PersonVisibleExpr).
	callerID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	res, err := h.store.ListDirectory(ctx, callerID, in)
	if err != nil {
		slog.ErrorContext(ctx, "list user directory failed", "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// MembershipsResponse is the wire shape of GET /api/v1/me/memberships.
// A flat envelope was chosen (not a bare array) so future additive fields
// — e.g. an "isAdmin" or "totalCount" hint — don't break clients.
type MembershipsResponse struct {
	Memberships []projects.MembershipDTO `json:"memberships"`
}

// GetMyMemberships handles GET /api/v1/me/memberships. Returns the
// calling user's project memberships — the per-connection visibility
// hint that skein-web's SignalR Hub uses to compute event filters at
// connect time.
//
// Behaviour:
//   - Non-admin caller: returns rows from project_members where user_id =
//     caller, role ∈ {"owner","member"}.
//   - Admin caller: returns one synthetic row per project with
//     role="admin", addedAt=projects.created_at — mirrors R3.1 admin-
//     sees-all visibility so the Hub doesn't need a separate code path.
//
// Capped at 1000 rows. Errors:
//   - 401 unauthenticated when no user in context (defensive — middleware
//     should have already returned 401).
//   - 500 internal_error on any DB failure.
//
// The response is intentionally lean: no project name / id_prefix here.
// The Hub only needs the project_id set; the SPA fetches richer metadata
// through the regular /api/v1/projects path.
func (h *UsersHandler) GetMyMemberships(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	memberships, err := h.projectsStore.ListMembershipsForUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "list memberships failed",
			"userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, MembershipsResponse{Memberships: memberships})
}

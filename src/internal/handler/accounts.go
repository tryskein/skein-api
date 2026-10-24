package handler

// accounts.go — the public account routes skein-web's BFF calls for a
// visitor without a session (sk-674.18 K-17; contract accounts/contract.go).
// They are mounted outside the /api/v1 user group: the service secret is the
// whole authorization and no user trust-header is read. The client address
// for the rate limits is X-Skein-Client-Ip (loginClientIP).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// accountsBodyMaxBytes is the body cap of the contract («Errors»: 16 KiB).
const accountsBodyMaxBytes = 16 << 10

// AccountsHandler serves the public account routes.
type AccountsHandler struct {
	svc *accounts.Service
}

// NewAccountsHandler builds the handler on the accounts service.
func NewAccountsHandler(svc *accounts.Service) *AccountsHandler {
	return &AccountsHandler{svc: svc}
}

// decodeAccountsBody reads one JSON value of the request's shape into dst:
// at most 16 KiB, unknown fields ignored (additive evolution), nothing after
// the value.
func decodeAccountsBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, accountsBodyMaxBytes))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// writeAccountsError answers err: a refusal with its code and status (and
// the suggestions of username_taken), anything else 500 internal_error.
func writeAccountsError(w http.ResponseWriter, r *http.Request, route string, err error) {
	var ref *accounts.Refusal
	if errors.As(err, &ref) {
		if ref.Code == accounts.CodeUsernameTaken {
			suggestions := ref.Suggestions
			if suggestions == nil {
				suggestions = []string{}
			}
			writeJSON(w, ref.Status(), accounts.UsernameTakenError{
				Error: ref.Code, Message: ref.Message, Suggestions: suggestions,
			})
			return
		}
		writeProblem(w, ref.Status(), ref.Code, ref.Message, nil)
		return
	}
	slog.ErrorContext(r.Context(), "account route failed", "route", route, "error", err.Error())
	writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be served", nil)
}

// Signup handles POST /api/v1/auth/signup.
//
// @Summary Self-service registration (service only, for skein-web)
// @Description Business meaning: a visitor registers an account with email, username and password (US-8, screens 2.6); skein-web then logs them in exactly as after a password login, with the sessionEpoch of the answer. Only while SKEIN_SIGNUP_ENABLED=true.
// @Description
// @Description Algorithm, refusals checked in this order: registration closed → 422 signup_disabled; body not one JSON value, a required field (email, username, password, language) missing or empty, email not of email shape, language not ru|en, body over 16 KiB → 400 invalid_request; the api-side rate limit per email and per client IP (X-Skein-Client-Ip; attempts per window, the backstop of skein-web's limiter) → 429 rate_limited; Turnstile configured and the token missing or not verified by siteverify → 422 captcha_failed; termsAccepted not true → 422 terms_required; username breaking the username rule ^[a-z0-9][a-z0-9_-]{2,29}$ or of the deleted-account pseudonym shape deleted-<8hex> → 422 username_invalid; password breaking the password policy (8..72 bytes) → 422 password_weak; password found in the Pwned Passwords corpus (only with SKEIN_PWNED_PASSWORDS_ENABLED; an unreachable corpus does not block) → 422 password_pwned; email of an account, a deleted one included → 422 email_taken; username of an account, a deleted one included → 422 username_taken with `suggestions`, 2–3 free usernames.
// @Description
// @Description Success: in ONE transaction the account is created (role user, name = username), the consent recorded with the server's date and time (terms_accepted_at), its personal space "Personal" created; the common space is NOT joined whatever SKEIN_COMMON_SPACE_ENABLED says (INV-22: only accounts made by an admin, an admin's agent or SSO join it); the email is NOT confirmed. A confirmation email is queued in the same transaction (delivered by the mail worker). The 200 body is the authentication response with sessionEpoch.
// @Description
// @Description Side effects: the user.created event; personal signals to the users whose issues' assignee text now names the new account. Of two concurrent signups of one email exactly one succeeds, the other gets 422 email_taken (E-25), never a 500.
// @Description
// @Description Authorization: the service secret X-Skein-Internal-Secret only; no user headers are read. Accepted risk: username_taken discloses that a username exists on the instance, mitigated by the captcha and the rate limit.
// @Tags Auth
// @Accept json
// @Produce json
// @Security InternalSecretAuth
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web; keys the per-IP limit (skipped when absent)"
// @Param body body accounts.SignupRequest true "Registration form"
// @Success 200 {object} accounts.AuthResponse "Account created; log the user in with this body"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 422 {object} accounts.UsernameTakenError "signup_disabled, captcha_failed, terms_required, username_invalid, password_weak, password_pwned, email_taken; username_taken carries suggestions"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/signup [post]
func (h *AccountsHandler) Signup(w http.ResponseWriter, r *http.Request) {
	var req accounts.SignupRequest
	if err := decodeAccountsBody(r, &req); err != nil {
		// A closed registration answers the same whatever the body.
		if !h.svc.SignupEnabled() {
			writeProblem(w, http.StatusUnprocessableEntity, accounts.CodeSignupDisabled, "registration is closed", nil)
			return
		}
		writeProblem(w, http.StatusBadRequest, accounts.CodeInvalidRequest, "body: a single JSON object in the signup format, at most 16 KiB", nil)
		return
	}
	resp, err := h.svc.Signup(r.Context(), req, loginClientIP(r))
	if err != nil {
		writeAccountsError(w, r, "signup", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// UsernameSuggestions handles GET /api/v1/auth/username-suggestions.
//
// @Summary Free usernames from an email (service only, for skein-web)
// @Description Business meaning: the "Generate from email" action of the registration form (screens 2.6) — 2–3 usernames free at the moment of the answer, derived from the part of the email before @, best first.
// @Description
// @Description Algorithm: registration closed → 422 signup_disabled; no or malformed email → 400 invalid_request; the api-side rate limit per email and client IP → 429 rate_limited. The stem is the local part lower-cased, every character outside [a-z0-9_-] replaced by '-', cut to 30 characters; the candidates are the stem, stem2, stem-<first domain label>, stem<its initial>, stem3…stem9, then stem10…stem99; a candidate is offered only if it obeys the username rule, is not of the deleted-account pseudonym shape, belongs to no account and is not reserved by a deleted one. A suggestion reserves nothing.
// @Description
// @Description Side effects: none. Authorization: the service secret only. Accepted risk: the answer discloses which usernames are taken, mitigated by the rate limit.
// @Tags Auth
// @Produce json
// @Security InternalSecretAuth
// @Param email query string true "The email typed in the form"
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web"
// @Success 200 {object} accounts.UsernameSuggestionsResponse "Free usernames, best first"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 422 {object} map[string]any "signup_disabled"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/username-suggestions [get]
func (h *AccountsHandler) UsernameSuggestions(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.UsernameSuggestions(r.Context(), r.URL.Query().Get("email"), loginClientIP(r))
	if err != nil {
		writeAccountsError(w, r, "username-suggestions", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// PasswordReset handles POST /api/v1/auth/password-reset.
//
// @Summary Request a password reset email (service only, for skein-web)
// @Description Business meaning: "Forgot password?" (screens 2.8). The answer is NEUTRAL — the same 204 with no body whether or not an account has the email and whether or not it may log in with a password (SC-009), so the route reveals nothing about accounts.
// @Description
// @Description Algorithm: SKEIN_AUTH_MODE=keycloak → 422 password_login_disabled; SKEIN_EMAIL_ENABLED=false → 422 email_disabled; body not one JSON value, email missing or not of email shape, language not ru|en → 400 invalid_request; the api-side rate limit per email and per client IP (X-Skein-Client-Ip) → 429 rate_limited — none of these depends on the email's account. Then, in one transaction: the active (not deleted, not blocked) account with the email is looked up; the email is queued only if that account may log in with a password (local mode: every account; hybrid: an external account or the break-glass admin). Queueing retires the account's earlier reset links and unsent reset emails (the newest link is the only live one; it lives 1 hour from the delivery attempt).
// @Description
// @Description Side effects: a password_reset email in the outgoing queue, in the language of the request. Authorization: the service secret only; no user headers are read.
// @Tags Auth
// @Accept json
// @Produce json
// @Security InternalSecretAuth
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web"
// @Param body body accounts.PasswordResetRequest true "The email and the email language"
// @Success 204 "Accepted (neutral)"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 422 {object} map[string]any "password_login_disabled, email_disabled"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/password-reset [post]
func (h *AccountsHandler) PasswordReset(w http.ResponseWriter, r *http.Request) {
	var req accounts.PasswordResetRequest
	if err := decodeAccountsBody(r, &req); err != nil {
		// The refusals that do not depend on the body come first.
		if err := h.svc.ResetAvailability(); err != nil {
			writeAccountsError(w, r, "password-reset", err)
			return
		}
		writeProblem(w, http.StatusBadRequest, accounts.CodeInvalidRequest, "body: a single JSON object in the reset format, at most 16 KiB", nil)
		return
	}
	if err := h.svc.RequestPasswordReset(r.Context(), req, loginClientIP(r)); err != nil {
		writeAccountsError(w, r, "password-reset", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PasswordResetComplete handles POST /api/v1/auth/password-reset/{token}.
//
// @Summary Set a new password by a reset link (service only, for skein-web)
// @Description Business meaning: the page the reset email links to (screens 2.8). The password is set, every session of the user ends and the API keys keep working (INV-26). The answer is not a login: skein-web re-issues its cookie with the returned sessionEpoch only when the request came with a session of the same user; otherwise the person logs in with the new password.
// @Description
// @Description Algorithm: keycloak mode → 422 password_login_disabled; mail off → 422 email_disabled; body not one JSON value or password missing → 400 invalid_request; the api-side rate limit per client IP → 429 rate_limited; the token unknown, expired, used, superseded by a newer reset email, or of a deleted or blocked account → 422 token_invalid (checked before any password work); the password breaking the policy (8..72 bytes) → 422 password_weak; found in the Pwned Passwords corpus (only with SKEIN_PWNED_PASSWORDS_ENABLED; an unreachable corpus does not block) → 422 password_pwned. Then, in ONE transaction: the token is used up under the user's lock (a concurrent use of the same link loses with token_invalid), the account must still log in with a password in the current mode (else token_invalid), the password hash is replaced, the session epoch is raised, and the authentication response is read after it.
// @Description
// @Description Side effects: the user.password_changed event (via reset); every other reset link of the user stops working and their unsent reset emails are cancelled. A refused request leaves the token usable.
// @Tags Auth
// @Accept json
// @Produce json
// @Security InternalSecretAuth
// @Param token path string true "The token from the reset link"
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web"
// @Param body body accounts.PasswordResetCompleteRequest true "The new password"
// @Success 200 {object} accounts.AuthResponse "Password set; sessionEpoch is the NEW epoch"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 422 {object} map[string]any "password_login_disabled, email_disabled, token_invalid, password_weak, password_pwned"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/password-reset/{token} [post]
func (h *AccountsHandler) PasswordResetComplete(w http.ResponseWriter, r *http.Request) {
	var req accounts.PasswordResetCompleteRequest
	if err := decodeAccountsBody(r, &req); err != nil {
		if err := h.svc.ResetAvailability(); err != nil {
			writeAccountsError(w, r, "password-reset-complete", err)
			return
		}
		writeProblem(w, http.StatusBadRequest, accounts.CodeInvalidRequest, "body: a single JSON object in the reset-completion format, at most 16 KiB", nil)
		return
	}
	resp, err := h.svc.CompletePasswordReset(r.Context(), chi.URLParam(r, "token"), req, loginClientIP(r))
	if err != nil {
		writeAccountsError(w, r, "password-reset-complete", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ConfirmEmail handles POST /api/v1/auth/confirm-email/{token}.
//
// @Summary Confirm an email by its link (service only, for skein-web)
// @Description Business meaning: the page the confirmation email links to (screens 2.9). The account's email becomes confirmed; the open tabs of the user get the personal signal `inbox`, so accepting an invitation from the Inbox becomes available without a reload (US-5). No session is issued.
// @Description
// @Description Algorithm: SKEIN_EMAIL_ENABLED=false → 422 email_disabled; the api-side rate limit per client IP → 429 rate_limited; then in ONE transaction the token is used up under the user's lock and email_confirmed_at is stamped (kept if already set). The token unknown, expired, used, superseded by a newer confirmation email, or of a deleted or blocked account → 422 token_invalid, nothing changes. A live token of an email already confirmed another way is a success too.
// @Description
// @Description Side effects: every other confirmation link of the user stops working, unsent confirmation emails are cancelled; the `inbox` signal is published after the commit.
// @Tags Auth
// @Produce json
// @Security InternalSecretAuth
// @Param token path string true "The token from the confirmation link"
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web"
// @Success 204 "Email confirmed"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 422 {object} map[string]any "email_disabled, token_invalid"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/confirm-email/{token} [post]
func (h *AccountsHandler) ConfirmEmail(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.ConfirmEmail(r.Context(), chi.URLParam(r, "token"), loginClientIP(r)); err != nil {
		writeAccountsError(w, r, "confirm-email", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RequestConfirmationEmail handles POST /api/v1/me/email/confirmation.
//
// @Summary Send the caller a new confirmation email
// @Description Business meaning: "Send the email again" on the account screen (screens 2.9, E-26). The latest link is the one that works.
// @Description
// @Description Algorithm: SKEIN_EMAIL_ENABLED=false → 422 email_disabled; body not one JSON value or language not ru|en → 400 invalid_request; one request per user per minute, whatever the client address → 429 rate_limited. Then, under the user's lock: an email already confirmed → 204 with nothing sent; otherwise every earlier confirmation link of the user stops working, unsent confirmation emails are cancelled, and a new one is queued in the requested language.
// @Description
// @Description Side effects: an email_confirm email in the outgoing queue. Authorization: a logged-in user (session or API key); the email goes to the caller's own address.
// @Tags Me
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body spaces.ConfirmationEmailRequest true "The email language"
// @Success 204 "Queued, or nothing to send (already confirmed)"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Not logged in"
// @Failure 422 {object} map[string]any "email_disabled"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/email/confirmation [post]
func (h *AccountsHandler) RequestConfirmationEmail(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req spaces.ConfirmationEmailRequest
	if err := decodeAccountsBody(r, &req); err != nil {
		if err := h.svc.MailAvailability(); err != nil {
			writeAccountsError(w, r, "me-email-confirmation", err)
			return
		}
		writeProblem(w, http.StatusBadRequest, accounts.CodeInvalidRequest, "body: a single JSON object with language, at most 16 KiB", nil)
		return
	}
	if err := h.svc.RequestConfirmationEmail(r.Context(), userID, req.Language); err != nil {
		writeAccountsError(w, r, "me-email-confirmation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AccountDeleter is what the account deletion routes need from the users
// store (users.Store; the operator's API of T042 deletes through the same
// DeleteAccount).
type AccountDeleter interface {
	AccountDeletionPreview(ctx context.Context, userID uuid.UUID) (spaces.AccountDeletionPreview, error)
	VerifyDeletionPassword(ctx context.Context, userID uuid.UUID, password string) (string, error)
	DeleteAccount(ctx context.Context, userID uuid.UUID, verifiedHash string, actor users.Actor) error
}

// AccountDeletionHandler serves GET /me/deletion-preview and DELETE /me
// (sk-674.18 T041, US-10; contract spaces/contract.go «Account deletion»).
type AccountDeletionHandler struct {
	store AccountDeleter
	// signupEnabled is SKEIN_SIGNUP_ENABLED: only an instance with open
	// registration lets an account delete itself.
	signupEnabled bool
}

// NewAccountDeletionHandler builds the handler.
func NewAccountDeletionHandler(store AccountDeleter, signupEnabled bool) *AccountDeletionHandler {
	return &AccountDeletionHandler{store: store, signupEnabled: signupEnabled}
}

// writeDeletionError answers a refusal with its code (and the blocking
// spaces), anything else 500.
func writeDeletionError(w http.ResponseWriter, r *http.Request, op string, err error) {
	// The account was deleted in between (another tab, the operator): it is
	// gone the way the session gate says it — 401 session_revoked, on which
	// skein-web ends the session.
	if errors.Is(err, users.ErrUserNotFound) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": auth.CodeSessionRevoked})
		return
	}
	var ref *spaces.Refusal
	if errors.As(spaces.AsRefusal(err), &ref) {
		writeProblem(w, ref.Status, ref.Code, ref.Message, ref.Extra)
		return
	}
	slog.ErrorContext(r.Context(), "account deletion failed", "operation", op, "error", err.Error())
	writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
}

// DeletionPreview handles GET /api/v1/me/deletion-preview.
//
// @Summary What deleting my account would do
// @Description Business meaning: the consequences screen of "Delete account" (screens 2.9, 2.13) — whether the caller may delete the account now and what goes with it (US-10).
// @Description
// @Description Algorithm: registration closed (SKEIN_SIGNUP_ENABLED=false) → 422 account_deletion_disabled. Otherwise, from the caller's participations: blocking — the regular spaces he owns with other participants and the common space if he owns it (deletion is refused while any is listed); deleted — his personal space and the regular spaces he owns alone, each with its number of projects; openAssignedIssues — the open issues outside the deleted spaces that name him as assignee and lose him. The refusal of the last active admin is not listed here; DELETE answers it.
// @Description
// @Description Read-only; no side effects.
// @Tags Account
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} spaces.AccountDeletionPreview "Blocking spaces, deleted spaces, open issues losing the caller"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 422 {object} map[string]any "account_deletion_disabled"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/me/deletion-preview [get]
func (h *AccountDeletionHandler) DeletionPreview(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if !h.signupEnabled {
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeAccountDeletionDisabled,
			"accounts are managed by the administrator", nil)
		return
	}
	resp, err := h.store.AccountDeletionPreview(r.Context(), c.UserID)
	if err != nil {
		writeDeletionError(w, r, "deletion_preview", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeleteMe handles DELETE /api/v1/me.
//
// @Summary Delete my account
// @Description Business meaning: the caller deletes his account at once and for good, confirming with his password (US-10, D-8). The account is anonymized, not removed: the history stays and shows a permanent pseudonym deleted-<8 hex>.
// @Description
// @Description Refusals, checked in this order: registration closed (SKEIN_SIGNUP_ENABLED=false) → 422 account_deletion_disabled; body not one JSON object or password missing → 400 invalid_request; the account deleted meanwhile (another tab, the operator) → 401 session_revoked; wrong password → 422 invalid_credentials; the caller owns a regular space with other participants → 422 owns_spaces_with_members; the caller owns the common space → 422 owns_common_space (an admin must change its owner first) — both carry `spaces`, every blocking space; the caller is the last active admin → 422 last_admin_cannot_demote; a concurrent change of the same spaces after three attempts → 422 concurrent_change. Nothing is changed on a refusal.
// @Description
// @Description Algorithm, one SERIALIZABLE transaction: the spaces of the caller and of the pending invitations to or from him are locked, then his row; the account is deactivated (session epoch raised, every API key revoked); his personal space and the regular spaces he owns alone are deleted with their projects, issues and attachments (the space deletion cascade); every other space is left as by leaving (US-7: assignee removed from his open issues, his and his agents' claims released, watches and reminders removed, memberships deleted); the pending invitations to his email and from him are revoked (their links answer «invalid»), their tokens retired and unsent emails cancelled; his tokens, unfinished emails, push subscriptions, agent sessions, monitor credentials, preferences, notifications and themes are deleted; his email, username and name are replaced by the pseudonym in the account, the assignee text, comment authors, commit authors, key labels, invitations and event data, and his email and @mentions in issue and comment text (their search embeddings are cleared and rebuilt); the hashes of his email and username are reserved — neither registers or is assigned again.
// @Description
// @Description After the 204 no session and no API key of the account works (the next request gets 401). Side effects: the user.deactivated event (skein-web drops the sessions and hub groups), the space events and personal signals of the deleted and left spaces, published after the commit.
// @Tags Account
// @Accept json
// @Security ApiKeyAuth
// @Param body body spaces.DeleteAccountRequest true "The caller's password"
// @Success 204 "The account is deleted"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Missing or invalid auth; session_revoked — the account was deleted meanwhile"
// @Failure 422 {object} map[string]any "account_deletion_disabled, invalid_credentials, owns_spaces_with_members (spaces), owns_common_space (spaces), last_admin_cannot_demote, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/me [delete]
func (h *AccountDeletionHandler) DeleteMe(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if !h.signupEnabled {
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeAccountDeletionDisabled,
			"accounts are managed by the administrator", nil)
		return
	}
	var req spaces.DeleteAccountRequest
	if err := decodeAccountsBody(r, &req); err != nil || req.Password == "" {
		writeProblem(w, http.StatusBadRequest, accounts.CodeInvalidRequest, "body: {\"password\": \"…\"}", nil)
		return
	}
	hash, err := h.store.VerifyDeletionPassword(r.Context(), c.UserID, req.Password)
	if err != nil {
		writeDeletionError(w, r, "delete_account", err)
		return
	}
	if err := h.store.DeleteAccount(r.Context(), c.UserID, hash, users.HumanActor(c.UserID, c.Username)); err != nil {
		writeDeletionError(w, r, "delete_account", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Package accounts owns self-service accounts: registration, password reset,
// email confirmation and signup by invitation (sk-674.18, US-8, US-9).
//
// This file is the skein-api → skein-web wire contract of the package (T012).
// Only types, codes and their documentation live here; the handlers are
// T038 (signup, username suggestions), T039 (password reset, email
// confirmation) and T040 (invitation by token), the shared builder of the
// authentication response is T036. Treat every path, JSON name and code
// below as a stable cross-repository contract: additive changes only.
//
// # Callers and authorization
//
// Every route of this contract is called by skein-web (the BFF) and only by
// it: the service secret `X-Skein-Internal-Secret` is the whole
// authorization (RequireInternalSecret, outside the ParseUserHeaders group,
// like `/api/v1/auth/oidc/provision`). No route reads the user trust-headers
// `X-Skein-User-*`: there is no logged-in user yet, or the request must not
// depend on one. A wrong or missing secret is answered
// 401 `{"error":"invalid internal secret"}` by the middleware.
//
// The public routes additionally take `X-Skein-Client-Ip` — the real client
// address, which skein-web stamps after stripping any client-supplied
// `X-Skein-*` (handler/auth.go#loginClientIP has the trust argument). It
// keys the api-side rate limits; when absent the per-IP limit is skipped and
// the per-email one still applies.
//
// # Routes
//
//	GET  /api/v1/internal/instance-config         → 200 InstanceConfig
//	POST /api/v1/auth/signup                      SignupRequest → 200 AuthResponse
//	GET  /api/v1/auth/username-suggestions?email= → 200 UsernameSuggestionsResponse
//	POST /api/v1/auth/password-reset              PasswordResetRequest → 204
//	POST /api/v1/auth/password-reset/{token}      PasswordResetCompleteRequest → 200 AuthResponse
//	POST /api/v1/auth/confirm-email/{token}       (no body) → 204
//	GET  /api/v1/auth/invitations/{token}         → 200 InvitationPreview
//	POST /api/v1/auth/invitations/{token}/signup  InvitationSignupRequest → 200 InvitationSignupResponse
//
// The browser reaches them through skein-web's public BFF routes
// (`/api/auth/signup`, `/api/auth/username-suggestions`,
// `/api/auth/password-reset`, `/api/auth/password-reset/{token}`,
// `/api/auth/confirm-email/{token}`, `/api/auth/invitations/{token}`,
// `/api/auth/invitations/{token}/signup` — plan K-17), never directly.
//
// # Invitation by token: how the spec routes map here
//
// The spec's API surface names, for the invitation link, «`GET
// /invitations/{token}`, `POST /invitations/{token}/accept|decline|signup` —
// no login / addressee». They are served as two route families, split by
// whether the browser has a session:
//
//	spec route                          visitor without a session                 logged-in addressee
//	GET  /invitations/{token}           BFF GET  /api/auth/invitations/{token}     GET  /api/v1/invitations/{token}
//	                                    → api GET /api/v1/auth/invitations/{token}  (spaces/contract.go#TokenInvitationView)
//	                                    (this file, InvitationPreview)
//	POST /invitations/{token}/signup    BFF POST /api/auth/invitations/{token}/signup  — (signup is for a visitor
//	                                    → api POST /api/v1/auth/invitations/{token}/signup   without an account)
//	                                    (this file, InvitationSignupRequest)
//	POST /invitations/{token}/accept    — (needs a session: log in first)      POST /api/v1/invitations/{token}/accept
//	POST /invitations/{token}/decline   — (needs a session: log in first)      POST /api/v1/invitations/{token}/decline
//
// The logged-in routes go through skein-web's ordinary /api/v1 proxy with
// the user's identity and are the REST contract of the spaces package
// (spaces/contract.go, T013); the identity-dependent states ("already a
// member", "for another address") exist only there.
//
// # Errors
//
// One shape for every refusal: `{"error":"<code>","message":"<prose>"}`,
// plus the extra fields named at the code. Clients branch on `error` only;
// `message` is for logs and may change.
//
// The machine code — `code` in the spec's terms — travels in the `error`
// field: that is the single refusal format of the whole REST API (the
// lead's decision for sk-674.18), and no separate `code` key exists. The
// constants below are those codes.
//
//   - 400 [CodeInvalidRequest] — syntax: the body is not one JSON value of
//     the documented shape, a required field is missing or empty, an email is
//     not of email shape, `language` is not a supported value, or the body
//     exceeds 16 KiB. Unknown JSON fields are ignored (additive evolution).
//   - 422 — the business refusals listed at each route.
//   - 429 [CodeRateLimited] — the api-side rate limit of the action. The
//     primary limiter is skein-web's (Redis); this one is the backstop.
//   - 500 `internal_error` — the request could not be served.
//
// # Session epoch
//
// [AuthResponse] is the ONE body of every answer after which skein-web
// issues or re-issues a browser session: password login
// (`POST /api/v1/auth/login`), OIDC provisioning
// (`POST /api/v1/auth/oidc/provision`), signup, signup by invitation and a
// successful password reset (re-issue only — see
// [PasswordResetCompleteRequest]). Its `sessionEpoch` is REQUIRED in all
// five: the current `users.session_epoch` read in the same request, after
// any change the request itself made (a reset raises it first). skein-web puts it into
// the session claims and sends it back on every request as
// `X-Skein-Session-Epoch`; the per-request check and its 401
// `session_revoked` are the contract of auth/headers.go (T014). A response
// without the field is a login failure on the web side — no fallback to an
// older value or a default (K-16).
//
// Status: this is the contract to be built, not today's behaviour. The
// column users.session_epoch will be added by sk-674.18 T015; the field will
// be produced by T036 (password login, OIDC provisioning — the shared
// builder), T038 (signup), T040 (signup by invitation) and T039 (password
// reset); the per-request check will be implemented by T036. Until then the
// login and provisioning responses do not carry `sessionEpoch`, and
// skein-web will start requiring it (T048) only in the same release (D-11).
package accounts

import "time"

// ---------------------------------------------------------------------------
// GET /api/v1/internal/instance-config
// ---------------------------------------------------------------------------

// InstanceConfig is the 200 body of GET /api/v1/internal/instance-config:
// the installation settings skein-web takes from skein-api instead of keeping
// copies of its own (SAAS-PLAN 2.3, D-7, US-18).
//
// skein-web reads it in two ways. `authMode` is read once at startup, before
// the authentication scheme is built; without an answer skein-web does not
// become ready, and changing the mode needs a restart of both services. The
// behaviour flags are read per request of `GET /api/auth/config`, so a
// restart of skein-api alone with a new value changes what the browser sees.
//
// The values are this process's configuration, fixed for its lifetime. Every
// key is always present; null means "not configured".
//
// Literal:
//
//	{"authMode":"local","signupEnabled":true,"emailEnabled":true,
//	 "turnstileSiteKey":"0x4AAAAAAA","projectPrefixMinLength":3,
//	 "termsUrl":"https://skein.example.com/terms","privacyUrl":null,
//	 "instanceAdminEnabled":false}
type InstanceConfig struct {
	// AuthMode is SKEIN_AUTH_MODE: "local" | "keycloak" | "hybrid".
	AuthMode string `json:"authMode"`
	// SignupEnabled is SKEIN_SIGNUP_ENABLED: self-service registration is open.
	SignupEnabled bool `json:"signupEnabled"`
	// EmailEnabled is SKEIN_EMAIL_ENABLED: outgoing mail works (password
	// reset, email confirmation, invitation emails).
	EmailEnabled bool `json:"emailEnabled"`
	// TurnstileSiteKey is SKEIN_TURNSTILE_SITE_KEY, the public Cloudflare
	// Turnstile key for the registration captcha; null = no captcha.
	TurnstileSiteKey *string `json:"turnstileSiteKey"`
	// ProjectPrefixMinLength is SKEIN_PROJECT_PREFIX_MIN_LENGTH; null = no
	// minimum.
	ProjectPrefixMinLength *int `json:"projectPrefixMinLength"`
	// TermsURL is SKEIN_TERMS_URL, the terms of service linked from the
	// consent checkbox; null = the consent text has no link.
	TermsURL *string `json:"termsUrl"`
	// PrivacyURL is SKEIN_PRIVACY_URL, the privacy policy linked from the
	// consent checkbox; null = the consent text has no link.
	PrivacyURL *string `json:"privacyUrl"`
	// InstanceAdminEnabled is SKEIN_INSTANCE_ADMIN_ENABLED: the instance admin
	// role exists.
	InstanceAdminEnabled bool `json:"instanceAdminEnabled"`
}

// ---------------------------------------------------------------------------
// The authentication response
// ---------------------------------------------------------------------------

// AuthResponse is the 200 body of every response after which skein-web
// issues or re-issues a session (see "Session epoch" in the package
// documentation). It never carries a password hash, an SSO subject or raw
// metadata.
//
// Literal:
//
//	{"id":"0199a1b2-0000-7000-8000-000000000001","email":"dev@client.io",
//	 "username":"dev","name":"dev","role":"user","isExternal":false,
//	 "createdAt":"2026-10-12T18:13:03Z","lastLoginAt":"2026-10-12T18:13:03Z",
//	 "sessionEpoch":4}
type AuthResponse struct {
	// ID is the user id (UUID).
	ID string `json:"id"`
	// Email is the login email, lowercased.
	Email string `json:"email"`
	// Username is the @-mention handle.
	Username string `json:"username"`
	// Name is the display name.
	Name string `json:"name"`
	// Role is "user" | "admin".
	Role string `json:"role"`
	// IsExternal marks an external collaborator account.
	IsExternal bool `json:"isExternal"`
	// CreatedAt is when the account was created (RFC 3339, UTC).
	CreatedAt time.Time `json:"createdAt"`
	// LastLoginAt is the last successful login recorded for the account
	// (RFC 3339, UTC); null when none is recorded.
	LastLoginAt *time.Time `json:"lastLoginAt"`
	// SessionEpoch is REQUIRED: the user's current session epoch, read after
	// any change this request made. Sessions carrying an older epoch are
	// revoked (401 session_revoked, auth/headers.go).
	SessionEpoch int64 `json:"sessionEpoch"`
}

// ---------------------------------------------------------------------------
// Languages
// ---------------------------------------------------------------------------

// Language values of the `language` request field: the interface language
// the email is written in. It is not stored on the user (spec Clarifications).
const (
	LanguageRU = "ru"
	LanguageEN = "en"
)

// ---------------------------------------------------------------------------
// Machine codes
// ---------------------------------------------------------------------------

// Codes of this contract (the `error` field). Each route lists which of them
// it returns; a code is never returned by a route that does not list it.
const (
	// CodeInvalidRequest — 400, syntax (see "Errors").
	CodeInvalidRequest = "invalid_request"
	// CodeRateLimited — 429, the action's rate limit was exceeded.
	CodeRateLimited = "rate_limited"

	// CodeSignupDisabled — 422, SKEIN_SIGNUP_ENABLED=false.
	CodeSignupDisabled = "signup_disabled"
	// CodeEmailDisabled — 422, SKEIN_EMAIL_ENABLED=false: there is no mail to
	// send a reset link or a confirmation with.
	CodeEmailDisabled = "email_disabled"
	// CodePasswordLoginDisabled — 422, SKEIN_AUTH_MODE=keycloak: there are no
	// passwords to reset; they are recovered at the SSO provider.
	CodePasswordLoginDisabled = "password_login_disabled"

	// CodeEmailTaken — 422, the email belongs to an account, a deleted one
	// included (emails of deleted accounts are never issued again).
	CodeEmailTaken = "email_taken"
	// CodeUsernameTaken — 422, the username belongs to an account, a deleted
	// one included. Carries `suggestions`: 2–3 free usernames.
	CodeUsernameTaken = "username_taken"
	// CodeUsernameInvalid — 422, the username breaks the username rule.
	CodeUsernameInvalid = "username_invalid"
	// CodePasswordWeak — 422, the password breaks the password policy (the
	// one of the self-service password change, length bounds included).
	CodePasswordWeak = "password_weak"
	// CodePasswordPwned — 422, the password was found in the Pwned Passwords
	// corpus (only while SKEIN_PWNED_PASSWORDS_ENABLED=true).
	CodePasswordPwned = "password_pwned"
	// CodeTermsRequired — 422, `termsAccepted` is not true.
	CodeTermsRequired = "terms_required"
	// CodeCaptchaFailed — 422, Turnstile is configured and the token is
	// missing or did not verify.
	CodeCaptchaFailed = "captcha_failed"

	// CodeTokenInvalid — 422, the token of a password-reset or confirm-email
	// link is unknown, expired or used.
	CodeTokenInvalid = "token_invalid"

	// CodeInvitationInvalid — 422, the invitation token is unknown, expired,
	// revoked or used (screens 2.5 "invalid or expired").
	CodeInvitationInvalid = "invitation_invalid"
	// CodeSpaceDeleted — 422, the space of the invitation was deleted
	// (screens 2.5 "deleted"). Checked before CodeInvitationInvalid.
	CodeSpaceDeleted = "space_deleted"
)

// ---------------------------------------------------------------------------
// POST /api/v1/auth/signup — screens 2.6, T038
// ---------------------------------------------------------------------------

// SignupRequest is the body of POST /api/v1/auth/signup.
//
// Success: the account and its personal space are created, the consent is
// recorded with the server's date and time, a confirmation email is queued,
// and the 200 body is [AuthResponse] — skein-web logs the user in exactly as
// after a password login. Refusals, checked in this order: 422
// [CodeSignupDisabled]; 429 [CodeRateLimited]; 422 [CodeCaptchaFailed]
// (only when Turnstile is configured); 422 [CodeTermsRequired]; 422
// [CodeUsernameInvalid], [CodePasswordWeak], [CodePasswordPwned]; 422
// [CodeEmailTaken], [CodeUsernameTaken]. Of two concurrent signups of one
// email exactly one succeeds, the other gets [CodeEmailTaken] (E-25).
//
// Literal:
//
//	{"email":"dev@client.io","username":"dev","password":"correct horse battery",
//	 "termsAccepted":true,"turnstileToken":"0.AbCd…","language":"en"}
type SignupRequest struct {
	// Email — the login email; required.
	Email string `json:"email"`
	// Username — the @-mention handle; required.
	Username string `json:"username"`
	// Password — the new password; required.
	Password string `json:"password"`
	// TermsAccepted — the consent checkbox; anything but true is
	// [CodeTermsRequired].
	TermsAccepted bool `json:"termsAccepted"`
	// TurnstileToken — the Turnstile widget token; required (non-empty) iff
	// Turnstile is configured, ignored otherwise.
	TurnstileToken string `json:"turnstileToken"`
	// Language — the language of the confirmation email: [LanguageRU] |
	// [LanguageEN]; required.
	Language string `json:"language"`
}

// UsernameTakenError is the 422 body of [CodeUsernameTaken], on signup and
// on signup by invitation.
//
// Literal:
//
//	{"error":"username_taken","message":"username already in use",
//	 "suggestions":["dev2","dev-client","devc"]}
type UsernameTakenError struct {
	// Error is [CodeUsernameTaken].
	Error string `json:"error"`
	// Message is prose for logs.
	Message string `json:"message"`
	// Suggestions are 2–3 usernames free at the moment of the answer.
	Suggestions []string `json:"suggestions"`
}

// ---------------------------------------------------------------------------
// GET /api/v1/auth/username-suggestions?email= — screens 2.6, T038
// ---------------------------------------------------------------------------

// UsernameSuggestionsResponse is the 200 body of
// GET /api/v1/auth/username-suggestions?email=<email> ("Generate from
// email"): free usernames derived from the part of the email before @,
// following the username rule. Refusals: 400 [CodeInvalidRequest] (no or
// malformed email), 422 [CodeSignupDisabled], 429 [CodeRateLimited].
//
// Literal:
//
//	{"suggestions":["dev","dev2","dev-client"]}
type UsernameSuggestionsResponse struct {
	// Suggestions are 2–3 usernames free at the moment of the answer, best
	// first.
	Suggestions []string `json:"suggestions"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/auth/password-reset — screens 2.8, T039
// ---------------------------------------------------------------------------

// PasswordResetRequest is the body of POST /api/v1/auth/password-reset.
//
// Success is 204 with no body and is NEUTRAL: identical whether or not an
// account with this email exists, and whether or not that account may log in
// with a password (in hybrid only external accounts and the break-glass
// admin get the email). The reset email is queued only for an account that
// can log in with a password. Refusals, which do not depend on the email:
// 422 [CodePasswordLoginDisabled] (keycloak mode), 422 [CodeEmailDisabled],
// 429 [CodeRateLimited].
//
// Literal:
//
//	{"email":"dev@client.io","language":"ru"}
type PasswordResetRequest struct {
	// Email — the account email; required.
	Email string `json:"email"`
	// Language — the language of the reset email: [LanguageRU] |
	// [LanguageEN]; required.
	Language string `json:"language"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/auth/password-reset/{token} — screens 2.8, T039
// ---------------------------------------------------------------------------

// PasswordResetCompleteRequest is the body of
// POST /api/v1/auth/password-reset/{token}; {token} is the token from the
// reset link.
//
// Success: the password is set, the token is used up, the session epoch is
// raised (every session of the user ends; API keys keep working), and the
// 200 body is [AuthResponse] with the NEW epoch. The response is not a
// login. skein-web re-issues its cookie with the new epoch ONLY when the
// request came with a session of the SAME user (the tab that reset its own
// password stays logged in); with no session, or a session of another user,
// no session is issued or changed and the person logs in with the new
// password. Refusals: 422
// [CodePasswordLoginDisabled], 422 [CodeEmailDisabled], 429
// [CodeRateLimited], 422 [CodeTokenInvalid], 422 [CodePasswordWeak],
// [CodePasswordPwned].
//
// Literal:
//
//	{"password":"correct horse battery"}
type PasswordResetCompleteRequest struct {
	// Password — the new password; required.
	Password string `json:"password"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/auth/confirm-email/{token} — screens 2.9, T039
// ---------------------------------------------------------------------------

// POST /api/v1/auth/confirm-email/{token} has no body; {token} is the token
// from the confirmation email. Success is 204: the account's email is
// confirmed (the open tabs of the user get the personal signal `inbox`).
// Confirming an already confirmed email with a valid token is a success too.
// Refusals: 422 [CodeEmailDisabled], 429 [CodeRateLimited], 422
// [CodeTokenInvalid]. No session is issued.

// ---------------------------------------------------------------------------
// GET /api/v1/auth/invitations/{token} — screens 2.5, T040
// ---------------------------------------------------------------------------

// Invitation states of [InvitationPreview.Status], in the order they are
// checked (spec US-5, screens 2.5).
const (
	// InvitationSpaceDeleted — the space was deleted.
	InvitationSpaceDeleted = "space_deleted"
	// InvitationInvalid — the token is unknown, expired, revoked or used.
	InvitationInvalid = "invalid"
	// InvitationValid — the invitation can be accepted or signed up for.
	InvitationValid = "valid"
)

// InvitationPreview is the 200 body of GET /api/v1/auth/invitations/{token}
// for a visitor WITHOUT a session. It never changes state: opening the link
// confirms nothing and accepts nothing (that needs a session and the
// logged-in route of spaces/contract.go). Every invitation state is a 200,
// not an error; refusals are only 400 [CodeInvalidRequest] and 429
// [CodeRateLimited].
//
// The fields after `status` are present only when status is "valid" (null
// otherwise): a dead link reveals nothing about whom it was for. A valid
// token proves access to the mailbox, so its own email and the existence of
// an account for it are disclosed to its holder.
//
// Literal (a person without an account, registration open):
//
//	{"status":"valid","space":{"id":"k3x9p2qa","slug":"acme","name":"Acme"},
//	 "inviterUsername":"anna","email":"dev@client.io","hasAccount":false}
type InvitationPreview struct {
	// Status is [InvitationSpaceDeleted] | [InvitationInvalid] |
	// [InvitationValid].
	Status string `json:"status"`
	// Space is the space invited to; null unless valid.
	Space *InvitationSpace `json:"space"`
	// InviterUsername is the username of whoever sent the invitation; null
	// unless valid.
	InviterUsername *string `json:"inviterUsername"`
	// Email is the invited email; null unless valid.
	Email *string `json:"email"`
	// HasAccount reports whether an account with the invited email exists
	// (log in to accept) or not (sign up by invitation, if registration is
	// open); null unless valid.
	HasAccount *bool `json:"hasAccount"`
}

// InvitationSpace names the space of an invitation. ID and Slug are the same
// values GET /me/spaces returns for the space (spaces/contract.go), so the
// client builds the place address `/spaces/<id>-<slug>` from them.
type InvitationSpace struct {
	// ID is the space id as GET /me/spaces returns it.
	ID string `json:"id"`
	// Slug is the address slug of the space name.
	Slug string `json:"slug"`
	// Name is the space name.
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/auth/invitations/{token}/signup — screens 2.7, T040
// ---------------------------------------------------------------------------

// InvitationSignupRequest is the body of
// POST /api/v1/auth/invitations/{token}/signup: registration by invitation
// for an email without an account. The email is the invitation's; there is
// no confirmation email (the link proved the mailbox). Like every
// registration (US-8), it is protected by Turnstile when it is configured.
//
// Success: in ONE transaction the account is created with its email
// confirmed, its personal space created, the consent recorded with the
// server's date and time, and the user joins the invitation's space; the
// 200 body is [InvitationSignupResponse]. Refusals, checked in this order:
// 422 [CodeSignupDisabled]; 429 [CodeRateLimited]; 422 [CodeCaptchaFailed]
// (only when Turnstile is configured); 422 [CodeSpaceDeleted];
// 422 [CodeInvitationInvalid] (expired while the form was filled in — no
// account is created, E-7); 422 [CodeTermsRequired]; 422
// [CodeUsernameInvalid], [CodePasswordWeak], [CodePasswordPwned]; 422
// [CodeEmailTaken] (the invited email belongs to an account, a deleted one
// included), [CodeUsernameTaken].
//
// Literal:
//
//	{"username":"dev","password":"correct horse battery","termsAccepted":true,
//	 "turnstileToken":"0.AbCd…"}
type InvitationSignupRequest struct {
	// Username — the @-mention handle; required.
	Username string `json:"username"`
	// Password — the new password; required.
	Password string `json:"password"`
	// TermsAccepted — the consent checkbox; anything but true is
	// [CodeTermsRequired].
	TermsAccepted bool `json:"termsAccepted"`
	// TurnstileToken — the Turnstile widget token; required (non-empty) iff
	// Turnstile is configured, ignored otherwise.
	TurnstileToken string `json:"turnstileToken"`
}

// InvitationSignupResponse is the 200 body of
// POST /api/v1/auth/invitations/{token}/signup: the [AuthResponse] fields
// (sessionEpoch REQUIRED — skein-web logs the user in with it) plus the
// space joined, for the redirect to `/spaces/<id>-<slug>`.
//
// Literal:
//
//	{"id":"0199a1b2-0000-7000-8000-000000000001","email":"dev@client.io",
//	 "username":"dev","name":"dev","role":"user","isExternal":false,
//	 "createdAt":"2026-10-12T18:13:03Z","lastLoginAt":null,"sessionEpoch":0,
//	 "space":{"id":"k3x9p2qa","slug":"acme","name":"Acme"}}
type InvitationSignupResponse struct {
	AuthResponse
	// Space is the space the new account joined.
	Space InvitationSpace `json:"space"`
}

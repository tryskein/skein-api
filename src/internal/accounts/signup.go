package accounts

// signup.go — self-service registration (sk-674.18 T038, US-8, INV-18,
// E-25; contract SignupRequest / UsernameSuggestionsResponse in
// contract.go).
//
// Service decides everything a signup is answered with — the order of the
// refusals, the limits, the captcha, the username rule — and leaves the
// account itself to the users store (the Users interface, implemented by
// users.Store), because that package owns the user row, the password policy
// and the authentication response, and imports this one.
//
// The account is created in ONE transaction (users.Store.CreateSelfService):
// the user row with the consent stamped by the server (terms_accepted_at =
// now()), its "Personal" (ProvisionInTx, T020 — the email is NOT confirmed;
// NOT the common space: INV-22 enrols only accounts made by an admin, an
// admin's agent or SSO, and SAAS-PLAN 2.6 "those who signed up do not
// enter the common space"), the confirmation email queued
// (QueueConfirmationInTx, mail.Enqueue, T021), and the authentication
// response read inside it (sessionEpoch, K-16). Of two signups of one email
// exactly one commits; the other meets the unique index on lower(email) —
// or, retried after a serialisation failure, the account itself — and is
// answered email_taken (E-25).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/mail"
)

// EmailRegex is the email shape of the installation: something@domain.tld
// without whitespace.
var EmailRegex = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// emailMaxLen bounds an email (RFC 5321 path limit).
const emailMaxLen = 254

// Errors the Users implementation answers with.
var (
	// ErrEmailTaken — the email belongs to an account or is reserved.
	ErrEmailTaken = errors.New("accounts: email taken")
	// ErrUsernameTaken — the username belongs to an account or is reserved.
	ErrUsernameTaken = errors.New("accounts: username taken")
	// ErrPasswordWeak — the password breaks the password policy.
	ErrPasswordWeak = errors.New("accounts: password weak")
	// ErrPasswordPwned — the password is in the Pwned Passwords corpus.
	ErrPasswordPwned = errors.New("accounts: password pwned")
)

// Refusal is a refusal of a public account route: the machine code of the
// contract, prose for logs, and the extra fields of the code.
type Refusal struct {
	Code    string
	Message string
	// Suggestions — the free usernames of CodeUsernameTaken.
	Suggestions []string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Status is the HTTP status of the refusal (contract «Errors»).
func (r *Refusal) Status() int {
	switch r.Code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusUnprocessableEntity
	}
}

func refuse(code, msg string) *Refusal { return &Refusal{Code: code, Message: msg} }

// NewAccount is what the users store creates on signup.
type NewAccount struct {
	// Email is lower-cased and trimmed; Username lower-cased and valid.
	Email    string
	Username string
	Password string
	// Language is the language of the confirmation email.
	Language string
	// DeliveryTTL is SKEIN_EMAIL_DELIVERY_TTL for the confirmation email.
	DeliveryTTL time.Duration
	// EmailConfirmed creates the account with its email confirmed and
	// queues no confirmation email (signup by invitation, T040: the link
	// proved the mailbox); Language and DeliveryTTL are then unused.
	EmailConfirmed bool
	// Step is another package's part of the creating transaction (signup by
	// invitation joins the invitation's space); nil for a plain signup.
	Step AccountStep
}

// Users is what signup needs from the users store.
type Users interface {
	// CheckNewPassword applies the password policy of a new password (the
	// one of the self-service password change): ErrPasswordWeak,
	// ErrPasswordPwned, or nil. An unreachable breach corpus does not
	// block. path names the route for the log (PasswordPathSignup,
	// PasswordPathReset).
	CheckNewPassword(ctx context.Context, password, path string) error
	// CreateSelfService creates the account in one transaction (see the
	// file header) and returns the authentication response read inside it:
	// ErrEmailTaken / ErrUsernameTaken when either is taken or reserved.
	CreateSelfService(ctx context.Context, a NewAccount) (AuthResponse, error)
	// HashPassword hashes a password that passed CheckNewPassword.
	HashPassword(password string) (string, error)
	// CompletePasswordResetInTx sets the user's password hash inside tx —
	// the transaction that used the reset token — raises the session epoch
	// (every session ends, API keys stay; INV-26) and returns the
	// authentication response with the new epoch.
	CompletePasswordResetInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, hash string) (AuthResponse, error)
}

// Paths a new password comes from, for the log of the password check.
const (
	PasswordPathSignup           = "signup"
	PasswordPathReset            = "password_reset"
	PasswordPathInvitationSignup = "invitation_signup"
)

// Captcha verifies a registration captcha token (Turnstile).
type Captcha interface {
	Enabled() bool
	Verify(ctx context.Context, token, remoteIP string) error
}

// ServiceConfig is the configuration the service answers by.
type ServiceConfig struct {
	// SignupEnabled is SKEIN_SIGNUP_ENABLED.
	SignupEnabled bool
	// EmailEnabled is SKEIN_EMAIL_ENABLED.
	EmailEnabled bool
	// EmailDeliveryTTL is SKEIN_EMAIL_DELIVERY_TTL.
	EmailDeliveryTTL time.Duration
	// AuthMode is SKEIN_AUTH_MODE: local | keycloak | hybrid.
	AuthMode string
	// BreakGlassAdmin is the break-glass admin email ("" = none).
	BreakGlassAdmin string
}

// TxBeginner opens the service's transactions (*pgxpool.Pool).
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Service serves the public account routes.
type Service struct {
	cfg     ServiceConfig
	db      TxBeginner
	users   Users
	ids     Identities
	captcha Captcha
	limiter *RateLimiter
	// inv serves the invitation link routes (UseInvitations).
	inv Invitations
}

// NewService builds the service; db opens the transactions of the reset and
// confirmation routes.
func NewService(cfg ServiceConfig, db TxBeginner, users Users, ids Identities, captcha Captcha, limiter *RateLimiter) *Service {
	return &Service{cfg: cfg, db: db, users: users, ids: ids, captcha: captcha, limiter: limiter}
}

// SignupEnabled reports SKEIN_SIGNUP_ENABLED.
func (s *Service) SignupEnabled() bool { return s.cfg.SignupEnabled }

// normaliseEmail trims and lower-cases an email and checks its shape.
func normaliseEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	return e, e != "" && len(e) <= emailMaxLen && EmailRegex.MatchString(e)
}

// Signup registers an account (contract SignupRequest). clientIP is
// X-Skein-Client-Ip ("" when absent). Every refusal is a *Refusal; any
// other error is a failure to serve.
func (s *Service) Signup(ctx context.Context, req SignupRequest, clientIP string) (AuthResponse, error) {
	if !s.cfg.SignupEnabled {
		return AuthResponse{}, refuse(CodeSignupDisabled, "registration is closed")
	}
	email, ok := normaliseEmail(req.Email)
	if !ok {
		return AuthResponse{}, refuse(CodeInvalidRequest, "email: required, must be a valid email address")
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	switch {
	case username == "":
		return AuthResponse{}, refuse(CodeInvalidRequest, "username: required")
	case req.Password == "":
		return AuthResponse{}, refuse(CodeInvalidRequest, "password: required")
	case req.Language != LanguageRU && req.Language != LanguageEN:
		return AuthResponse{}, refuse(CodeInvalidRequest, "language: ru or en")
	}

	if !s.limiter.Allow(ActionSignup, email, clientIP) {
		return AuthResponse{}, refuse(CodeRateLimited, "too many signup attempts")
	}
	if s.captcha.Enabled() {
		if err := s.captcha.Verify(ctx, req.TurnstileToken, clientIP); err != nil {
			return AuthResponse{}, refuse(CodeCaptchaFailed, "captcha not verified")
		}
	}
	if !req.TermsAccepted {
		return AuthResponse{}, refuse(CodeTermsRequired, "the terms must be accepted")
	}
	if !ValidNewUsername(username) {
		return AuthResponse{}, refuse(CodeUsernameInvalid, "username must match "+UsernameRegex.String())
	}
	if err := s.users.CheckNewPassword(ctx, req.Password, PasswordPathSignup); err != nil {
		switch {
		case errors.Is(err, ErrPasswordWeak):
			return AuthResponse{}, refuse(CodePasswordWeak, "password does not meet the password policy")
		case errors.Is(err, ErrPasswordPwned):
			return AuthResponse{}, refuse(CodePasswordPwned, "password appears in a known data breach")
		}
		return AuthResponse{}, err
	}

	// The taken checks before the transaction answer the common case in
	// the contract's order (email first); the transaction checks again and
	// the unique indexes settle a race.
	taken, err := s.ids.EmailTaken(ctx, email)
	if err != nil {
		return AuthResponse{}, err
	}
	if taken {
		return AuthResponse{}, refuse(CodeEmailTaken, "email already in use")
	}
	names, err := s.ids.TakenUsernames(ctx, []string{username})
	if err != nil {
		return AuthResponse{}, err
	}
	if names[username] {
		return AuthResponse{}, s.usernameTaken(ctx, username, email)
	}

	resp, err := s.users.CreateSelfService(ctx, NewAccount{
		Email: email, Username: username, Password: req.Password,
		Language: req.Language, DeliveryTTL: s.cfg.EmailDeliveryTTL,
	})
	switch {
	case errors.Is(err, ErrEmailTaken):
		return AuthResponse{}, refuse(CodeEmailTaken, "email already in use")
	case errors.Is(err, ErrUsernameTaken):
		return AuthResponse{}, s.usernameTaken(ctx, username, email)
	case err != nil:
		return AuthResponse{}, err
	}
	return resp, nil
}

// usernameTaken builds the CodeUsernameTaken refusal with its free
// variants.
func (s *Service) usernameTaken(ctx context.Context, username, email string) error {
	free, err := suggestFree(ctx, s.ids, username, email, username)
	if err != nil {
		return err
	}
	return &Refusal{Code: CodeUsernameTaken, Message: "username already in use", Suggestions: free}
}

// UsernameSuggestions answers GET /auth/username-suggestions?email=: free
// usernames derived from the part of rawEmail before @.
func (s *Service) UsernameSuggestions(ctx context.Context, rawEmail, clientIP string) (UsernameSuggestionsResponse, error) {
	if !s.cfg.SignupEnabled {
		return UsernameSuggestionsResponse{}, refuse(CodeSignupDisabled, "registration is closed")
	}
	email, ok := normaliseEmail(rawEmail)
	if !ok {
		return UsernameSuggestionsResponse{}, refuse(CodeInvalidRequest, "email: required, must be a valid email address")
	}
	if !s.limiter.Allow(ActionUsernameSuggestions, email, clientIP) {
		return UsernameSuggestionsResponse{}, refuse(CodeRateLimited, "too many suggestion requests")
	}
	free, err := suggestFree(ctx, s.ids, usernameBase(email), email, "")
	if err != nil {
		return UsernameSuggestionsResponse{}, err
	}
	return UsernameSuggestionsResponse{Suggestions: free}, nil
}

// TxQuerier is the part of pgx.Tx the in-transaction checks read with.
type TxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Kinds of a reserved identity (reserved_identities.kind).
const (
	ReservedEmail    = "email"
	ReservedUsername = "username"
)

// IdentityReserved reports whether value (an email or a username, compared
// lower-cased and trimmed) is reserved by a deleted account (INV-18) — the
// reservation alone, not a live account holding it. For the SSO
// provisioning (auth/oidc.go), whose live-account collisions are resolved
// by its own upsert.
func IdentityReserved(ctx context.Context, q TxQuerier, kind, value string) (bool, error) {
	var reserved bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM reserved_identities
                WHERE kind = $1 AND value_hash = sha256(convert_to(lower(btrim($2)), 'UTF8')))`,
		kind, value).Scan(&reserved); err != nil {
		return false, fmt.Errorf("checking the reserved %s: %w", kind, err)
	}
	return reserved, nil
}

// CheckIdentityFreeInTx is the check of a new account's email and username
// inside its creating transaction: ErrEmailTaken or ErrUsernameTaken when
// either belongs to an account or is reserved by a deleted one. The unique
// indexes cover live accounts on their own; this adds the reservations.
func CheckIdentityFreeInTx(ctx context.Context, tx TxQuerier, email, username string) error {
	taken, err := emailTakenIn(ctx, tx, email)
	if err != nil {
		return err
	}
	if taken {
		return ErrEmailTaken
	}
	if err := tx.QueryRow(ctx, usernameTakenSQL, username).Scan(&taken); err != nil {
		return fmt.Errorf("checking the username: %w", err)
	}
	if taken {
		return ErrUsernameTaken
	}
	return nil
}

// QueueConfirmationInTx queues the confirmation email of a new account in
// its creating transaction (T021: the email exists exactly when the account
// does). It is the subject's first email, so it is Enqueue, not
// tokens.Replace.
func QueueConfirmationInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, email, lang string, deliveryTTL time.Duration) error {
	if _, err := mail.Enqueue(ctx, tx, mail.Message{
		Kind: mail.KindEmailConfirm, To: email, Lang: lang, UserID: userID,
	}, deliveryTTL); err != nil {
		return fmt.Errorf("queueing the confirmation email: %w", err)
	}
	return nil
}

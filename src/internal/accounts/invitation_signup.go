package accounts

// invitation_signup.go — the anonymous half of an invitation link (sk-674.18
// T040, US-5, US-8, E-7; contract InvitationPreview and
// InvitationSignupRequest in contract.go): what a visitor without a session
// sees when he opens the link, and registration by invitation.
//
// This package decides the registration — the order of the refusals, the
// limits, the captcha, the username and password rules — exactly as a plain
// signup does; the invitation itself is the invitations package's (the
// Invitations interface, wired by cmd/api), because that package imports
// this one. The account is created by users.Store.CreateSelfService in ONE
// transaction, which here also runs the invitation's step (AccountStep):
// the space's lock first, then — once the account and its "Personal" exist —
// the token used (tokens.Use: an expired or used link rolls the whole
// account back, E-7), the invitation accepted and the space joined. The
// email is the invitation's and is confirmed at creation (the link proved
// the mailbox; US-5 "signed up by an invitation link"), so no
// confirmation email is queued.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Rate limits of the anonymous invitation routes (the api-side backstop of
// skein-web's limiter). The subject of both is the link token itself — its
// hash, never the raw value: the email is not known before the token is
// read, and a token is what an attacker would cycle.
var (
	// ActionInvitationPreview is GET /api/v1/auth/invitations/{token}.
	ActionInvitationPreview = Action{Name: "invitation_preview", PerEmail: 30, PerIP: 60, Window: 10 * time.Minute}
	// ActionInvitationSignup is POST /api/v1/auth/invitations/{token}/signup,
	// with the thresholds of a plain signup.
	ActionInvitationSignup = Action{Name: "invitation_signup", PerEmail: 5, PerIP: 20, Window: time.Hour}
)

// tokenSubject is the limiter subject of a link token: its SHA-256.
func tokenSubject(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// AccountStep is another package's part of the transaction that creates an
// account (users.Store.CreateSelfService): signup by invitation joins the
// invitation's space in it.
type AccountStep interface {
	// BeforeInTx runs first in the transaction — the locks the step needs,
	// taken before the account's own writes (K-7: the space's lock first).
	BeforeInTx(ctx context.Context, tx pgx.Tx) error
	// AfterInTx runs once the account and its "Personal" exist. An error rolls
	// the whole account back.
	AfterInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, username string) error
}

// InvitationStep is the step of a signup by invitation; Space is the space
// joined, valid after the transaction committed.
type InvitationStep interface {
	AccountStep
	Space() InvitationSpace
}

// Invitations is what the anonymous invitation routes need from the
// invitations package.
type Invitations interface {
	// Preview is the view of the link for a visitor without a session
	// (InvitationPreview): every state is a 200; it changes nothing.
	Preview(ctx context.Context, raw string) (InvitationPreview, error)
	// SignupStep is the step that, inside the new account's transaction,
	// uses the token of raw for the account with email, accepts the
	// invitation and joins its space. Its refusals are *Refusal with
	// CodeSpaceDeleted or CodeInvitationInvalid.
	SignupStep(raw, email string) InvitationStep
}

// UseInvitations wires the invitations package into the service (cmd/api);
// without it the invitation routes fail to serve.
func (s *Service) UseInvitations(inv Invitations) { s.inv = inv }

var errNoInvitations = errors.New("accounts: the invitation routes are not wired")

// InvitationPreview answers GET /api/v1/auth/invitations/{token}: the rate
// limit, then the link's state. clientIP is X-Skein-Client-Ip.
func (s *Service) InvitationPreview(ctx context.Context, raw, clientIP string) (InvitationPreview, error) {
	if s.inv == nil {
		return InvitationPreview{}, errNoInvitations
	}
	if !s.limiter.Allow(ActionInvitationPreview, tokenSubject(raw), clientIP) {
		return InvitationPreview{}, refuse(CodeRateLimited, "too many invitation link requests")
	}
	return s.inv.Preview(ctx, raw)
}

// InvitationSignup answers POST /api/v1/auth/invitations/{token}/signup
// (contract InvitationSignupRequest), refusals in the contract's order:
// signup_disabled; invalid_request; rate_limited; captcha_failed;
// space_deleted; invitation_invalid; terms_required; username_invalid,
// password_weak, password_pwned; email_taken, username_taken. The
// invitation's state is read again inside the account's transaction: a link
// that expired while the form was filled in creates no account (E-7).
func (s *Service) InvitationSignup(ctx context.Context, raw string, req InvitationSignupRequest, clientIP string) (InvitationSignupResponse, error) {
	if !s.cfg.SignupEnabled {
		return InvitationSignupResponse{}, refuse(CodeSignupDisabled, "registration is closed")
	}
	if s.inv == nil {
		return InvitationSignupResponse{}, errNoInvitations
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	switch {
	case username == "":
		return InvitationSignupResponse{}, refuse(CodeInvalidRequest, "username: required")
	case req.Password == "":
		return InvitationSignupResponse{}, refuse(CodeInvalidRequest, "password: required")
	}
	if !s.limiter.Allow(ActionInvitationSignup, tokenSubject(raw), clientIP) {
		return InvitationSignupResponse{}, refuse(CodeRateLimited, "too many signup attempts")
	}
	if s.captcha.Enabled() {
		if err := s.captcha.Verify(ctx, req.TurnstileToken, clientIP); err != nil {
			return InvitationSignupResponse{}, refuse(CodeCaptchaFailed, "captcha not verified")
		}
	}
	view, err := s.inv.Preview(ctx, raw)
	if err != nil {
		return InvitationSignupResponse{}, err
	}
	switch {
	case view.Status == InvitationSpaceDeleted:
		return InvitationSignupResponse{}, refuse(CodeSpaceDeleted, "the space of the invitation was deleted")
	case view.Status != InvitationValid || view.Email == nil:
		return InvitationSignupResponse{}, refuse(CodeInvitationInvalid, "the invitation is invalid or has expired")
	}
	email := *view.Email
	if !req.TermsAccepted {
		return InvitationSignupResponse{}, refuse(CodeTermsRequired, "the terms must be accepted")
	}
	if !ValidNewUsername(username) {
		return InvitationSignupResponse{}, refuse(CodeUsernameInvalid, "username must match "+UsernameRegex.String())
	}
	if err := s.users.CheckNewPassword(ctx, req.Password, PasswordPathInvitationSignup); err != nil {
		switch {
		case errors.Is(err, ErrPasswordWeak):
			return InvitationSignupResponse{}, refuse(CodePasswordWeak, "password does not meet the password policy")
		case errors.Is(err, ErrPasswordPwned):
			return InvitationSignupResponse{}, refuse(CodePasswordPwned, "password appears in a known data breach")
		}
		return InvitationSignupResponse{}, err
	}
	taken, err := s.ids.EmailTaken(ctx, email)
	if err != nil {
		return InvitationSignupResponse{}, err
	}
	if taken {
		return InvitationSignupResponse{}, refuse(CodeEmailTaken, "email already in use")
	}
	names, err := s.ids.TakenUsernames(ctx, []string{username})
	if err != nil {
		return InvitationSignupResponse{}, err
	}
	if names[username] {
		return InvitationSignupResponse{}, s.usernameTaken(ctx, username, email)
	}

	step := s.inv.SignupStep(raw, email)
	resp, err := s.users.CreateSelfService(ctx, NewAccount{
		Email: email, Username: username, Password: req.Password,
		EmailConfirmed: true, Step: step,
	})
	var ref *Refusal
	switch {
	case errors.Is(err, ErrEmailTaken):
		return InvitationSignupResponse{}, refuse(CodeEmailTaken, "email already in use")
	case errors.Is(err, ErrUsernameTaken):
		return InvitationSignupResponse{}, s.usernameTaken(ctx, username, email)
	case errors.As(err, &ref):
		return InvitationSignupResponse{}, ref
	case err != nil:
		return InvitationSignupResponse{}, err
	}
	return InvitationSignupResponse{AuthResponse: resp, Space: step.Space()}, nil
}

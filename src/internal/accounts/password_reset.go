package accounts

// password_reset.go — password reset by email (sk-674.18 T039, US-9,
// INV-26, SC-009; contract PasswordResetRequest /
// PasswordResetCompleteRequest in contract.go).
//
// The request is NEUTRAL: its answer (204) does not depend on whether an
// account has the email, nor on whether that account may log in with a
// password. The email is queued only for an account that can: in `local`
// every active account; in `hybrid` an external account or the break-glass
// admin (the others recover their password at the SSO provider); `keycloak`
// refuses the route outright. Queueing is tokens.Replace (T021): the earlier
// reset links of the account stop working, the new email carries the only
// live one.
//
// Completing the reset sets the password, uses the token up and raises the
// session epoch (users.BumpSessionEpochInTx, T036) in one transaction with
// tokens.Use, so a reset that fails leaves the token usable: every session
// of the user ends, the API keys keep working (INV-26). The answer is the
// authentication response with the NEW epoch; skein-web re-issues the
// cookie of the tab that reset its own password with it (K-16).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/mail"
	"skein-api/internal/tokens"
)

// Login modes of SKEIN_AUTH_MODE the reset distinguishes.
const (
	authModeLocal    = "local"
	authModeKeycloak = "keycloak"
)

// ActionPasswordReset is POST /api/v1/auth/password-reset.
var ActionPasswordReset = Action{Name: "password_reset", PerEmail: 5, PerIP: 20, Window: time.Hour}

// ActionPasswordResetComplete is POST /api/v1/auth/password-reset/{token}:
// the link carries no email, so the client IP is the key.
var ActionPasswordResetComplete = Action{Name: "password_reset_complete", PerIP: 20, Window: time.Hour}

// ResetAvailability answers the refusals of both reset routes that depend
// on neither the email nor the body: keycloak mode, mail off. nil = the
// routes are open.
func (s *Service) ResetAvailability() error {
	if s.cfg.AuthMode == authModeKeycloak {
		return refuse(CodePasswordLoginDisabled, "passwords are managed by the SSO provider")
	}
	return s.MailAvailability()
}

// MailAvailability is CodeEmailDisabled while SKEIN_EMAIL_ENABLED=false.
func (s *Service) MailAvailability() error {
	if !s.cfg.EmailEnabled {
		return refuse(CodeEmailDisabled, "outgoing mail is disabled")
	}
	return nil
}

// RequestPasswordReset serves POST /api/v1/auth/password-reset. Success is
// nil whatever the email (see the file header).
func (s *Service) RequestPasswordReset(ctx context.Context, req PasswordResetRequest, clientIP string) error {
	if err := s.ResetAvailability(); err != nil {
		return err
	}
	email, ok := normaliseEmail(req.Email)
	if !ok {
		return refuse(CodeInvalidRequest, "email: required, must be a valid email address")
	}
	if req.Language != LanguageRU && req.Language != LanguageEN {
		return refuse(CodeInvalidRequest, "language: ru or en")
	}
	if !s.limiter.Allow(ActionPasswordReset, email, clientIP) {
		return refuse(CodeRateLimited, "too many reset requests")
	}
	return pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var (
			userID   uuid.UUID
			external bool
		)
		err := tx.QueryRow(ctx, `
SELECT id, is_external FROM users
 WHERE lower(email) = $1 AND deleted_at IS NULL AND deactivated_at IS NULL`, email).Scan(&userID, &external)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // neutral: no account
		}
		if err != nil {
			return fmt.Errorf("looking up the reset email: %w", err)
		}
		if !s.passwordLoginAllowed(email, external) {
			return nil // neutral: this account signs in at the SSO provider
		}
		_, err = tokens.Replace(ctx, tx, mail.Message{
			Kind: mail.KindPasswordReset, To: email, Lang: req.Language, UserID: userID,
		}, s.cfg.EmailDeliveryTTL)
		if errors.Is(err, tokens.ErrSubjectNotFound) {
			return nil // closed between the read and the lock: neutral
		}
		return err
	})
}

// passwordLoginAllowed mirrors handler/auth.go#passwordLoginAllowed: in
// local mode every account signs in with a password; otherwise only an
// external account or the break-glass admin.
func (s *Service) passwordLoginAllowed(email string, external bool) bool {
	if s.cfg.AuthMode == authModeLocal {
		return true
	}
	return external || (s.cfg.BreakGlassAdmin != "" && strings.EqualFold(s.cfg.BreakGlassAdmin, email))
}

// CompletePasswordReset serves POST /api/v1/auth/password-reset/{token}.
func (s *Service) CompletePasswordReset(ctx context.Context, rawToken string, req PasswordResetCompleteRequest, clientIP string) (AuthResponse, error) {
	if err := s.ResetAvailability(); err != nil {
		return AuthResponse{}, err
	}
	if req.Password == "" {
		return AuthResponse{}, refuse(CodeInvalidRequest, "password: required")
	}
	if !s.limiter.Allow(ActionPasswordResetComplete, "", clientIP) {
		return AuthResponse{}, refuse(CodeRateLimited, "too many reset attempts")
	}
	invalid := refuse(CodeTokenInvalid, "the link is invalid or has expired")
	// The cheap check first: the breach-corpus lookup and the hash are not
	// spent on a dead link, and neither runs inside the transaction.
	if _, err := tokens.Peek(ctx, s.db, tokens.PurposePasswordReset, rawToken); err != nil {
		if errors.Is(err, tokens.ErrInvalid) {
			return AuthResponse{}, invalid
		}
		return AuthResponse{}, err
	}
	if err := s.users.CheckNewPassword(ctx, req.Password, PasswordPathReset); err != nil {
		switch {
		case errors.Is(err, ErrPasswordWeak):
			return AuthResponse{}, refuse(CodePasswordWeak, "password does not meet the password policy")
		case errors.Is(err, ErrPasswordPwned):
			return AuthResponse{}, refuse(CodePasswordPwned, "password appears in a known data breach")
		}
		return AuthResponse{}, err
	}
	hash, err := s.users.HashPassword(req.Password)
	if err != nil {
		return AuthResponse{}, err
	}
	var resp AuthResponse
	err = pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		used, err := tokens.Use(ctx, tx, tokens.PurposePasswordReset, rawToken, "")
		if err != nil {
			return err
		}
		// The mode may have changed since the link was sent: an account
		// that no longer signs in with a password gets no password.
		var (
			email    string
			external bool
		)
		if err := tx.QueryRow(ctx, `SELECT lower(email), is_external FROM users WHERE id = $1`,
			used.UserID).Scan(&email, &external); err != nil {
			return fmt.Errorf("reading the reset account: %w", err)
		}
		if !s.passwordLoginAllowed(email, external) {
			return tokens.ErrInvalid
		}
		resp, err = s.users.CompletePasswordResetInTx(ctx, tx, used.UserID, hash)
		return err
	})
	if errors.Is(err, tokens.ErrInvalid) {
		return AuthResponse{}, invalid
	}
	if err != nil {
		return AuthResponse{}, err
	}
	return resp, nil
}

package accounts

// email_confirm.go — email confirmation (sk-674.18 T039, US-9, US-5, E-26;
// contracts: POST /api/v1/auth/confirm-email/{token} in contract.go, POST
// /api/v1/me/email/confirmation in spaces/contract.go).
//
// Confirming by the link uses the token up and stamps email_confirmed_at in
// one transaction (tokens.Use, K-11), and the user's open tabs get the
// personal signal `inbox` once it commits — "Accept" of an invitation
// becomes active without a reload (US-5). A live token of an email already
// confirmed another way is a success too (the contract).
//
// Asking for the email again is a logged-in action: tokens.Replace queues a
// new email and retires the earlier links, so the latest one is the one
// that works (E-26). It is limited per user — once a minute, whatever the
// client address — and is a 204 with nothing sent when the email is already
// confirmed.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/mail"
	"skein-api/internal/signals"
	"skein-api/internal/tokens"
)

// ActionConfirmEmail is POST /api/v1/auth/confirm-email/{token}: the link
// carries no email, so the client IP is the key.
var ActionConfirmEmail = Action{Name: "confirm_email", PerIP: 30, Window: time.Hour}

// ActionConfirmationEmail is POST /api/v1/me/email/confirmation: one
// request per user per minute (E-26 "the button is inactive for a minute"), keyed on
// the user alone.
var ActionConfirmationEmail = Action{Name: "confirmation_email", PerEmail: 1, Window: time.Minute}

// ConfirmEmail serves POST /api/v1/auth/confirm-email/{token}.
func (s *Service) ConfirmEmail(ctx context.Context, rawToken, clientIP string) error {
	if err := s.MailAvailability(); err != nil {
		return err
	}
	if !s.limiter.Allow(ActionConfirmEmail, "", clientIP) {
		return refuse(CodeRateLimited, "too many confirmation attempts")
	}
	err := signals.BeginTxFunc(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		used, err := tokens.Use(ctx, tx, tokens.PurposeEmailConfirm, rawToken, "")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE users SET email_confirmed_at = COALESCE(email_confirmed_at, now()) WHERE id = $1`, used.UserID); err != nil {
			return fmt.Errorf("confirming the email: %w", err)
		}
		signals.Add(tx, used.UserID, signals.AreaInbox)
		return nil
	})
	if errors.Is(err, tokens.ErrInvalid) {
		return refuse(CodeTokenInvalid, "the link is invalid or has expired")
	}
	return err
}

// RequestConfirmationEmail serves POST /api/v1/me/email/confirmation for the
// logged-in userID.
func (s *Service) RequestConfirmationEmail(ctx context.Context, userID uuid.UUID, language string) error {
	if err := s.MailAvailability(); err != nil {
		return err
	}
	if language != LanguageRU && language != LanguageEN {
		return refuse(CodeInvalidRequest, "language: ru or en")
	}
	if !s.limiter.Allow(ActionConfirmationEmail, userID.String(), "") {
		return refuse(CodeRateLimited, "a confirmation email was requested less than a minute ago")
	}
	return pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var email string
		err := tx.QueryRow(ctx, `SELECT lower(email) FROM users WHERE id = $1`, userID).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // the session check already refuses a vanished user
		}
		if err != nil {
			return fmt.Errorf("reading the caller's email: %w", err)
		}
		// Replace re-reads the user under its lock and decides there.
		_, err = tokens.Replace(ctx, tx, mail.Message{
			Kind: mail.KindEmailConfirm, To: email, Lang: language, UserID: userID,
		}, s.cfg.EmailDeliveryTTL)
		if errors.Is(err, tokens.ErrCompleted) || errors.Is(err, tokens.ErrSubjectNotFound) {
			return nil // already confirmed: nothing sent
		}
		return err
	})
}

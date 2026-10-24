// Package mail owns outgoing email (sk-674.18, K-9): the queue
// outgoing_emails and, with T022, the worker that delivers it over SMTP.
//
// A queued email stores who it goes to and what it is about — its kind,
// recipient, language and SUBJECT (the user of an email confirmation or a
// password reset, the invitation of a space invite) — never a token or a
// rendered text. The link token is issued by tokens.IssueForEmail at the
// start of every delivery attempt (K-11).
package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Kind is what an email is for. The values are the kinds of
// outgoing_emails and the purposes of auth_tokens alike.
type Kind string

const (
	KindEmailConfirm  Kind = "email_confirm"
	KindPasswordReset Kind = "password_reset"
	KindSpaceInvite   Kind = "space_invite"
)

// Languages an email is rendered in (K-9: templates ru/en).
const (
	LangRU = "ru"
	LangEN = "en"
)

// Message is one email to queue.
type Message struct {
	Kind Kind
	// To is the recipient's address; it is stored trimmed and lower-cased.
	To string
	// Lang is LangRU or LangEN.
	Lang string
	// UserID is the subject of KindEmailConfirm and KindPasswordReset.
	UserID uuid.UUID
	// InvitationID is the subject of KindSpaceInvite.
	InvitationID uuid.UUID
}

// ErrInvalidMessage is returned for a message whose kind, language or
// subject is not one of the accepted combinations. Nothing is written.
var ErrInvalidMessage = errors.New("mail: invalid message")

// Validate reports whether m names a known kind and language and exactly
// the subject its kind takes.
func (m Message) Validate() error {
	switch m.Kind {
	case KindEmailConfirm, KindPasswordReset:
		if m.UserID == uuid.Nil || m.InvitationID != uuid.Nil {
			return fmt.Errorf("%w: kind %s takes a user subject", ErrInvalidMessage, m.Kind)
		}
	case KindSpaceInvite:
		if m.InvitationID == uuid.Nil || m.UserID != uuid.Nil {
			return fmt.Errorf("%w: kind %s takes an invitation subject", ErrInvalidMessage, m.Kind)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidMessage, m.Kind)
	}
	if m.Lang != LangRU && m.Lang != LangEN {
		return fmt.Errorf("%w: unknown language %q", ErrInvalidMessage, m.Lang)
	}
	if strings.TrimSpace(m.To) == "" {
		return fmt.Errorf("%w: empty recipient", ErrInvalidMessage)
	}
	return nil
}

// enqueueSQL queues a message as pending and ready at once; its delivery
// deadline is now() + the delivery TTL (SKEIN_EMAIL_DELIVERY_TTL).
const enqueueSQL = `
INSERT INTO outgoing_emails (kind, recipient, lang, user_id, invitation_id, state, deadline_at)
VALUES ($1, lower(btrim($2)), $3, $4, $5, 'pending', now() + ($6::bigint * interval '1 microsecond'))
RETURNING id`

// Enqueue queues m inside tx — the transaction of the action that produces
// the email — and returns the email's id. The row exists exactly when the
// action commits (INV-25): a rolled-back action leaves no email behind.
//
// Enqueue is the FIRST queueing of an email for its subject (an invitation
// just created, a signup's confirmation). Asking again for an email the
// subject may already have pending — a repeated confirmation request, a
// resent invitation, a new password reset — goes through tokens.Replace,
// which cancels the earlier ones under the subject's lock and calls Enqueue.
func Enqueue(ctx context.Context, tx pgx.Tx, m Message, deliveryTTL time.Duration) (uuid.UUID, error) {
	if err := m.Validate(); err != nil {
		return uuid.Nil, err
	}
	if deliveryTTL <= 0 {
		return uuid.Nil, fmt.Errorf("%w: delivery TTL must be positive, got %s", ErrInvalidMessage, deliveryTTL)
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, enqueueSQL,
		string(m.Kind), m.To, m.Lang, nullUUID(m.UserID), nullUUID(m.InvitationID),
		deliveryTTL.Microseconds(),
	).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("queueing %s email: %w", m.Kind, err)
	}
	return id, nil
}

func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

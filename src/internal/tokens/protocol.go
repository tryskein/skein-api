// Package tokens owns the one-time link tokens of email confirmation,
// password reset and space invitations (sk-674.18, D-6, K-11): the table
// auth_tokens and the protocol that issues, replaces, cancels and uses them.
//
// # Only the hash is stored
//
// A raw token exists in exactly one place: the memory of the mail worker
// that got it from [IssueForEmail] to put into the link of the email it is
// sending. auth_tokens keeps its SHA-256; no column of auth_tokens,
// outgoing_emails or space_invitations ever holds the raw value. A token
// is LIVE while it is neither used nor superseded and its expiry has not
// passed.
//
// # One point of serialisation
//
// Every token and every queued email belongs to a SUBJECT: the user of an
// email confirmation or a password reset, the invitation of a space
// invite. All four operations take the subject's row lock FIRST — users
// for email_confirm / password_reset, space_invitations for space_invite —
// and only then touch emails and tokens. Two operations on one subject
// therefore run one after the other, whatever the interleaving; operations
// on different subjects never wait for each other. Two purposes of one
// user share the users row and so wait for each other, but each touches
// only its own purpose's tokens and emails.
//
// The lock is FOR NO KEY UPDATE: it excludes the other protocol operations
// and writers of the row, but not inserts elsewhere that merely reference
// the user by foreign key.
//
// # The operations
//
//   - [Replace] — the subject asks again (a new confirmation or reset
//     request, a resent invitation): every live token of the subject and
//     purpose is superseded, every unfinished email of theirs is cancelled,
//     a new email is queued.
//   - [Cancel] — the action lost its force (invitation revoked or expired,
//     space or account deleted): tokens superseded, emails cancelled.
//   - [Use] — the action succeeded: the token is marked used, every other
//     live token of the subject and purpose superseded, unfinished emails
//     cancelled. It runs in the action's transaction, so an action that
//     rolls back (a weak password, email_taken) leaves the token unused.
//   - [IssueForEmail] — the mail worker, at the start of every attempt:
//     issues a fresh token for its email only if that email is still the
//     worker's lease and not cancelled, and the action is not completed;
//     supersedes the previous token OF THIS EMAIL only, and commits.
//
// Consequences: an email cancelled or replaced can never be issued a token
// again; a worker holding an outdated lease generation cannot touch the
// token of a newer email; a retry after a crash between issue and
// finalisation issues a new token only while the action is still open —
// a duplicate delivery is possible, a resurrected action is not.
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/mail"
)

// Purpose is what a token is for — the kind of the email it goes out in.
type Purpose = mail.Kind

const (
	PurposeEmailConfirm  = mail.KindEmailConfirm
	PurposePasswordReset = mail.KindPasswordReset
	PurposeSpaceInvite   = mail.KindSpaceInvite
)

// Lifetimes of the tokens whose expiry is not their subject's. A space
// invitation's token lives as long as the invitation (its expires_at,
// SKEIN_SPACE_INVITE_TTL); these two are constants of the package (K-11).
const (
	EmailConfirmTTL  = 24 * time.Hour
	PasswordResetTTL = time.Hour
)

// rawTokenBytes is the entropy of a token: 32 random bytes, sent as 43
// characters of unpadded base64url.
const rawTokenBytes = 32

var (
	// ErrInvalid: the token is unknown, of another purpose, used,
	// superseded or expired, or its action is no longer open (invitation
	// not pending, email already confirmed, account deleted). One error
	// for all of them: the caller answers "invalid or expired".
	ErrInvalid = errors.New("tokens: token is not valid")
	// ErrNotAddressee: a valid invitation token presented by an account
	// whose email is not the invitation's. Nothing is changed.
	ErrNotAddressee = errors.New("tokens: token belongs to another addressee")
	// ErrCompleted: Replace asked for a new email for an action that is
	// already completed (email confirmed, invitation no longer pending).
	ErrCompleted = errors.New("tokens: the action is already completed")
	// ErrSubjectNotFound: the user or invitation does not exist (for
	// Replace: or the account is deleted).
	ErrSubjectNotFound = errors.New("tokens: subject not found")
	// ErrNotIssuable: IssueForEmail refused — the email is not leased under
	// this generation, was cancelled or finished, or its action is closed.
	// Nothing was written; the worker finalises the email as cancelled. It
	// is mail.ErrNotIssuable itself: the worker sees the protocol through
	// mail.TokenIssuer (EmailIssuer), since this package imports mail.
	ErrNotIssuable = mail.ErrNotIssuable
)

// Subject names whose tokens and emails an operation touches.
type Subject struct {
	Purpose Purpose
	// UserID is the subject of PurposeEmailConfirm and PurposePasswordReset.
	UserID uuid.UUID
	// InvitationID is the subject of PurposeSpaceInvite.
	InvitationID uuid.UUID
}

// UserSubject is the subject of a user's email confirmation or password
// reset.
func UserSubject(p Purpose, userID uuid.UUID) Subject {
	return Subject{Purpose: p, UserID: userID}
}

// InvitationSubject is the subject of a space invitation.
func InvitationSubject(invitationID uuid.UUID) Subject {
	return Subject{Purpose: PurposeSpaceInvite, InvitationID: invitationID}
}

func (s Subject) validate() error {
	switch s.Purpose {
	case PurposeEmailConfirm, PurposePasswordReset:
		if s.UserID == uuid.Nil || s.InvitationID != uuid.Nil {
			return fmt.Errorf("tokens: purpose %s takes a user subject", s.Purpose)
		}
	case PurposeSpaceInvite:
		if s.InvitationID == uuid.Nil || s.UserID != uuid.Nil {
			return fmt.Errorf("tokens: purpose %s takes an invitation subject", s.Purpose)
		}
	default:
		return fmt.Errorf("tokens: unknown purpose %q", s.Purpose)
	}
	return nil
}

// column is the subject column of auth_tokens and outgoing_emails, and id
// its value. The column name is one of two literals, never input.
func (s Subject) column() (string, uuid.UUID) {
	if s.Purpose == PurposeSpaceInvite {
		return "invitation_id", s.InvitationID
	}
	return "user_id", s.UserID
}

// subjectState is what the subject's locked row says about its action.
type subjectState struct {
	// open: the action can still be completed — the invitation is pending
	// and not expired; the user is not deleted and, for a confirmation,
	// has not confirmed yet.
	open bool
	// deleted: the user's account is closed — deleted, or blocked
	// (deactivated: US-11 forbids sign-in, US-9 sends a reset letter only to
	// an account that can sign in). User subjects only.
	deleted bool
	// confirmed: the user's email is already confirmed (user subjects only).
	confirmed bool
	// email: the subject's address, lower-cased — the user's email, the
	// invitation's addressee.
	email string
	// expiresAt: the invitation's expiry (invitation subjects only).
	expiresAt time.Time
}

// lockSubject takes the subject's row lock — the protocol's single point
// of serialisation — and reads its state. ErrSubjectNotFound when the row
// does not exist.
func lockSubject(ctx context.Context, tx pgx.Tx, s Subject) (subjectState, error) {
	var st subjectState
	var err error
	if s.Purpose == PurposeSpaceInvite {
		err = tx.QueryRow(ctx, `
SELECT status = 'pending' AND expires_at > now(), lower(email), expires_at
  FROM space_invitations
 WHERE id = $1
   FOR NO KEY UPDATE`, s.InvitationID).Scan(&st.open, &st.email, &st.expiresAt)
	} else {
		err = tx.QueryRow(ctx, `
SELECT email_confirmed_at IS NOT NULL, deleted_at IS NOT NULL OR deactivated_at IS NOT NULL, lower(email)
  FROM users
 WHERE id = $1
   FOR NO KEY UPDATE`, s.UserID).Scan(&st.confirmed, &st.deleted, &st.email)
		st.open = !st.deleted && !(s.Purpose == PurposeEmailConfirm && st.confirmed)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrSubjectNotFound
	}
	if err != nil {
		return st, fmt.Errorf("locking %s subject: %w", s.Purpose, err)
	}
	return st, nil
}

// retire supersedes every live token of the subject and purpose except
// keep (uuid.Nil keeps none), cancels every unfinished email of the
// subject and purpose, and, for an invitation, clears its current token.
// The caller holds the subject's lock.
func retire(ctx context.Context, tx pgx.Tx, s Subject, keep uuid.UUID) error {
	col, id := s.column()
	if _, err := tx.Exec(ctx, `
UPDATE auth_tokens
   SET superseded_at = now()
 WHERE purpose = $1 AND `+col+` = $2
   AND used_at IS NULL AND superseded_at IS NULL
   AND id <> $3`, string(s.Purpose), id, keep); err != nil {
		return fmt.Errorf("superseding %s tokens: %w", s.Purpose, err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE outgoing_emails
   SET state = 'final', outcome = 'cancelled', finalized_at = now(),
       leased_until = NULL, next_attempt_at = NULL
 WHERE kind = $1 AND `+col+` = $2
   AND state <> 'final'`, string(s.Purpose), id); err != nil {
		return fmt.Errorf("cancelling %s emails: %w", s.Purpose, err)
	}
	if s.Purpose == PurposeSpaceInvite {
		if _, err := tx.Exec(ctx,
			`UPDATE space_invitations SET current_token_id = NULL WHERE id = $1`, id); err != nil {
			return fmt.Errorf("clearing the invitation's current token: %w", err)
		}
	}
	return nil
}

// Replace queues a new email for m's subject inside tx, retiring what the
// subject had before: every live token of the subject and purpose is
// superseded and every unfinished email of theirs cancelled, so the links
// already sent stop working and only the new email will carry one. It
// returns the new email's id.
//
// ErrCompleted when the action is already completed (the email is
// confirmed, the invitation is no longer pending or has expired) — a link
// for it would be dead on arrival; ErrSubjectNotFound when the subject does
// not exist or its account is deleted. Nothing is written on either.
func Replace(ctx context.Context, tx pgx.Tx, m mail.Message, deliveryTTL time.Duration) (uuid.UUID, error) {
	if err := m.Validate(); err != nil {
		return uuid.Nil, err
	}
	s := Subject{Purpose: m.Kind, UserID: m.UserID, InvitationID: m.InvitationID}
	st, err := lockSubject(ctx, tx, s)
	if err != nil {
		return uuid.Nil, err
	}
	if st.deleted {
		return uuid.Nil, ErrSubjectNotFound
	}
	if !st.open {
		return uuid.Nil, ErrCompleted
	}
	if err := retire(ctx, tx, s, uuid.Nil); err != nil {
		return uuid.Nil, err
	}
	return mail.Enqueue(ctx, tx, m, deliveryTTL)
}

// Cancel retires the subject inside tx: every live token of the subject
// and purpose is superseded and every unfinished email of theirs
// cancelled. It is how an action loses its force — an invitation revoked
// or expired, its space deleted, an account deleted. ErrSubjectNotFound
// when the subject does not exist.
func Cancel(ctx context.Context, tx pgx.Tx, s Subject) error {
	if err := s.validate(); err != nil {
		return err
	}
	if _, err := lockSubject(ctx, tx, s); err != nil {
		return err
	}
	return retire(ctx, tx, s, uuid.Nil)
}

// Used is the token [Use] accepted and the subject it belongs to.
type Used struct {
	TokenID      uuid.UUID
	UserID       uuid.UUID
	InvitationID uuid.UUID
}

// Use accepts a raw token for purpose inside tx — the transaction of the
// action the token authorises — and consumes it: the token is marked
// used, every other live token of its subject and purpose superseded,
// their unfinished emails cancelled. The caller performs the action in the
// same transaction; if it rolls back, so does the consumption and the
// token still works.
//
// addressee is the email of the account presenting a space-invite token;
// it must be the invitation's (case-insensitive), else ErrNotAddressee and
// nothing changes. It is ignored for the other purposes.
//
// ErrInvalid when the token is unknown, of another purpose, used,
// superseded, expired, or its action is no longer open — the account
// deleted or blocked, the invitation no longer pending. A live
// confirmation token of an email already confirmed another way succeeds
// (the confirm-email contract). Of two concurrent
// Use calls of one token exactly one succeeds: both take the subject's
// lock, and the second then finds the token used.
func Use(ctx context.Context, tx pgx.Tx, purpose Purpose, raw, addressee string) (Used, error) {
	hash := hashToken(raw)
	var (
		got Used
		p   string
		uid *uuid.UUID
		iid *uuid.UUID
	)
	// Which subject to lock: read without a lock — the decision is taken
	// again on the locked row below.
	err := tx.QueryRow(ctx,
		`SELECT purpose, user_id, invitation_id FROM auth_tokens WHERE token_hash = $1`,
		hash).Scan(&p, &uid, &iid)
	if errors.Is(err, pgx.ErrNoRows) {
		return got, ErrInvalid
	}
	if err != nil {
		return got, fmt.Errorf("looking up a %s token: %w", purpose, err)
	}
	if Purpose(p) != purpose {
		return got, ErrInvalid
	}
	s := Subject{Purpose: purpose}
	if uid != nil {
		s.UserID = *uid
	}
	if iid != nil {
		s.InvitationID = *iid
	}
	st, err := lockSubject(ctx, tx, s)
	if errors.Is(err, ErrSubjectNotFound) {
		return got, ErrInvalid
	}
	if err != nil {
		return got, err
	}
	err = tx.QueryRow(ctx, `
SELECT id FROM auth_tokens
 WHERE token_hash = $1
   AND used_at IS NULL AND superseded_at IS NULL AND expires_at > now()
   FOR UPDATE`, hash).Scan(&got.TokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return got, ErrInvalid
	}
	if err != nil {
		return got, fmt.Errorf("locking a %s token: %w", purpose, err)
	}
	// A live confirmation token of an email confirmed another way (an
	// invitation link, US-5) is a success too — the contract of
	// POST /api/v1/auth/confirm-email/{token} (accounts/contract.go): the
	// token is consumed and the action is a no-op for the caller (review
	// api/4 #15). Any other closed action refuses the token.
	alreadyConfirmed := purpose == PurposeEmailConfirm && st.confirmed && !st.deleted
	if !st.open && !alreadyConfirmed {
		return got, ErrInvalid
	}
	if purpose == PurposeSpaceInvite && normEmail(addressee) != st.email {
		return Used{}, ErrNotAddressee
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_tokens SET used_at = now() WHERE id = $1`, got.TokenID); err != nil {
		return Used{}, fmt.Errorf("using a %s token: %w", purpose, err)
	}
	if err := retire(ctx, tx, s, got.TokenID); err != nil {
		return Used{}, err
	}
	got.UserID, got.InvitationID = s.UserID, s.InvitationID
	return got, nil
}

// RowQuerier is the part of *pgxpool.Pool / pgx.Tx [Peek] reads with.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Peek reports, without a lock and without writing anything, whether raw is
// a live token of a user purpose (email confirmation, password reset) whose
// account is neither deleted nor blocked. It is the cheap check an action
// makes before spending work it would throw away on a dead link — a
// password reset checks the token before the breach-corpus lookup and the
// password hash, which must not run inside the action's transaction. It is
// advisory only: [Use] decides again under the subject's lock. ErrInvalid
// otherwise; on success the token's user.
func Peek(ctx context.Context, q RowQuerier, purpose Purpose, raw string) (uuid.UUID, error) {
	if purpose != PurposeEmailConfirm && purpose != PurposePasswordReset {
		return uuid.Nil, fmt.Errorf("tokens: Peek takes a user purpose, got %q", purpose)
	}
	var userID uuid.UUID
	err := q.QueryRow(ctx, `
SELECT t.user_id
  FROM auth_tokens t
  JOIN users u ON u.id = t.user_id
 WHERE t.token_hash = $1 AND t.purpose = $2
   AND t.used_at IS NULL AND t.superseded_at IS NULL AND t.expires_at > now()
   AND u.deleted_at IS NULL AND u.deactivated_at IS NULL`, hashToken(raw), string(purpose)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("peeking a %s token: %w", purpose, err)
	}
	return userID, nil
}

// InvitationToken is what [PeekInvitation] reads about a space-invite token.
type InvitationToken struct {
	// InvitationID is the invitation the token was issued for.
	InvitationID uuid.UUID
	// Live reports that the token itself is neither used nor superseded and
	// has not expired. It says nothing about the invitation's own state.
	Live bool
}

// PeekInvitation reads, without a lock and without writing anything, which
// invitation the raw space-invite token belongs to and whether the token is
// live. A dead token still names its invitation: the link of an invitation
// whose space was deleted answers "deleted", not "invalid" (spec
// US-5, screens 2.5 — the states are checked in that order), and the
// deletion supersedes its tokens. It is the read of the link's states and
// the precheck before work an action would throw away; [Use] decides again
// under the subject's lock. ErrInvalid for an unknown token or one of
// another purpose.
func PeekInvitation(ctx context.Context, q RowQuerier, raw string) (InvitationToken, error) {
	var t InvitationToken
	err := q.QueryRow(ctx, `
SELECT invitation_id, used_at IS NULL AND superseded_at IS NULL AND expires_at > now()
  FROM auth_tokens
 WHERE token_hash = $1 AND purpose = $2`, hashToken(raw), string(PurposeSpaceInvite)).Scan(&t.InvitationID, &t.Live)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrInvalid
	}
	if err != nil {
		return t, fmt.Errorf("peeking a %s token: %w", PurposeSpaceInvite, err)
	}
	return t, nil
}

// TxBeginner is the part of *pgxpool.Pool IssueForEmail opens its own
// transaction with.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// IssueForEmail issues a fresh token for the email emailID and commits it,
// for the mail worker at the start of every delivery attempt; the raw
// token is returned to the worker's memory and stored nowhere.
//
// It issues only if, under the subject's lock, the email is leased with
// exactly this generation (outgoing_emails.attempts of the worker's
// lease) — so not cancelled, not finished, not re-leased since — and its
// action is still open: the invitation pending and unexpired, the email
// not yet confirmed, the account not deleted and its address still the
// email's recipient. It then supersedes the previous live token OF THIS
// EMAIL only — never another email's — writes the new token's hash, and
// for an invitation makes it the current token. Otherwise ErrNotIssuable
// and nothing is written.
//
// The token of an invitation expires with the invitation; a confirmation
// or reset token after [EmailConfirmTTL] / [PasswordResetTTL].
func IssueForEmail(ctx context.Context, db TxBeginner, emailID uuid.UUID, generation int) (string, error) {
	issued, err := issueForEmail(ctx, db, emailID, generation)
	return issued.Token, err
}

// EmailIssuer is [IssueForEmail] as the mail worker consumes it
// (mail.TokenIssuer): the same protocol, also answering when the issued
// token expires, for the email's text.
func EmailIssuer(db TxBeginner) mail.TokenIssuer {
	return emailIssuer{db: db}
}

type emailIssuer struct{ db TxBeginner }

func (e emailIssuer) IssueForEmail(ctx context.Context, emailID uuid.UUID, generation int) (mail.Issued, error) {
	return issueForEmail(ctx, e.db, emailID, generation)
}

// issueForEmail is IssueForEmail with the issued token's expiry.
func issueForEmail(ctx context.Context, db TxBeginner, emailID uuid.UUID, generation int) (mail.Issued, error) {
	raw, hash, err := newToken()
	if err != nil {
		return mail.Issued{}, err
	}
	var expiresAt time.Time
	err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var (
			kind string
			uid  *uuid.UUID
			iid  *uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT kind, user_id, invitation_id FROM outgoing_emails WHERE id = $1`,
			emailID).Scan(&kind, &uid, &iid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotIssuable
		}
		if err != nil {
			return fmt.Errorf("reading email %s: %w", emailID, err)
		}
		s := Subject{Purpose: Purpose(kind)}
		if uid != nil {
			s.UserID = *uid
		}
		if iid != nil {
			s.InvitationID = *iid
		}
		st, err := lockSubject(ctx, tx, s)
		if errors.Is(err, ErrSubjectNotFound) {
			return ErrNotIssuable
		}
		if err != nil {
			return err
		}
		var recipient string
		err = tx.QueryRow(ctx, `
SELECT recipient FROM outgoing_emails
 WHERE id = $1 AND state = 'leased' AND attempts = $2
   FOR UPDATE`, emailID, generation).Scan(&recipient)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotIssuable
		}
		if err != nil {
			return fmt.Errorf("locking email %s: %w", emailID, err)
		}
		if !st.open || recipient != st.email {
			return ErrNotIssuable
		}
		if _, err := tx.Exec(ctx, `
UPDATE auth_tokens
   SET superseded_at = now()
 WHERE email_id = $1 AND used_at IS NULL AND superseded_at IS NULL`, emailID); err != nil {
			return fmt.Errorf("superseding the previous token of email %s: %w", emailID, err)
		}
		// An invitation's token expires with the invitation; the others
		// after their package lifetime.
		col, id := s.column()
		var (
			inviteExpiry *time.Time
			ttl          time.Duration
		)
		switch s.Purpose {
		case PurposeSpaceInvite:
			inviteExpiry = &st.expiresAt
		case PurposeEmailConfirm:
			ttl = EmailConfirmTTL
		default:
			ttl = PasswordResetTTL
		}
		var tokenID uuid.UUID
		if err := tx.QueryRow(ctx, `
INSERT INTO auth_tokens (purpose, token_hash, `+col+`, email_id, expires_at)
VALUES ($1, $2, $3, $4, COALESCE($5::timestamptz, now() + ($6::bigint * interval '1 microsecond')))
RETURNING id, expires_at`, string(s.Purpose), hash, id, emailID, inviteExpiry, ttl.Microseconds()).Scan(&tokenID, &expiresAt); err != nil {
			return fmt.Errorf("issuing a %s token for email %s: %w", s.Purpose, emailID, err)
		}
		if s.Purpose == PurposeSpaceInvite {
			if _, err := tx.Exec(ctx,
				`UPDATE space_invitations SET current_token_id = $2 WHERE id = $1`, id, tokenID); err != nil {
				return fmt.Errorf("setting the invitation's current token: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return mail.Issued{}, err
	}
	return mail.Issued{Token: raw, ExpiresAt: expiresAt}, nil
}

// newToken draws a raw token and its hash.
func newToken() (raw string, hash []byte, err error) {
	b := make([]byte, rawTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("drawing a token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashToken(raw), nil
}

// hashToken is the stored form of a raw token: SHA-256 of its text.
func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func normEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

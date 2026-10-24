package invitations

// store.go — the reads and writes of space invitations the operations share:
// the invitation row (locked or not), the person behind an email, the
// sender's journal of sends (space_invitation_sends, migration 000083) and
// the owner's view of an invitation.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/spaces"
)

// querier is the part of *pgxpool.Pool and pgx.Tx the reads use.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// invitation is one space_invitations row.
type invitation struct {
	ID uuid.UUID
	// SpaceID is nil once the space was deleted (000077: "deleted").
	SpaceID   *uuid.UUID
	Email     string
	InvitedBy uuid.UUID
	Status    string
	Lang      string
	ExpiresAt time.Time
	CreatedAt time.Time
	// Live: pending and not expired — the predicate of the token protocol
	// and of the seats (spaces/store.go#validPendingExpr), read by the
	// database's clock.
	Live bool
}

const invitationCols = `id, space_id, email, invited_by, status, lang, expires_at, created_at,
       status = 'pending' AND expires_at > now()`

func scanInvitation(row pgx.Row) (invitation, error) {
	var i invitation
	err := row.Scan(&i.ID, &i.SpaceID, &i.Email, &i.InvitedBy, &i.Status, &i.Lang, &i.ExpiresAt, &i.CreatedAt, &i.Live)
	return i, err
}

// errNoInvitation — no such invitation (in that space).
var errNoInvitation = errors.New("invitations: no such invitation")

// readInvitation reads invitation id without a lock.
func readInvitation(ctx context.Context, q querier, id uuid.UUID) (invitation, error) {
	i, err := scanInvitation(q.QueryRow(ctx, `SELECT `+invitationCols+` FROM space_invitations WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return i, errNoInvitation
	}
	if err != nil {
		return i, fmt.Errorf("reading invitation %s: %w", id, err)
	}
	return i, nil
}

// lockInvitation takes the invitation's row lock — the token protocol's
// point of serialisation for its subject (tokens/protocol.go) — and reads
// it. With spaceID set, an invitation of another space is errNoInvitation:
// an invitation id is looked up only within the space of the route (INV-3).
// The caller holds the space's lock first (K-7).
func lockInvitation(ctx context.Context, tx pgx.Tx, id uuid.UUID, spaceID *uuid.UUID) (invitation, error) {
	i, err := scanInvitation(tx.QueryRow(ctx, `
SELECT `+invitationCols+` FROM space_invitations
 WHERE id = $1 AND ($2::uuid IS NULL OR space_id = $2)
   FOR NO KEY UPDATE`, id, spaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return i, errNoInvitation
	}
	if err != nil {
		return i, fmt.Errorf("locking invitation %s: %w", id, err)
	}
	return i, nil
}

// resolve sets the final status of a pending invitation.
func resolve(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string) error {
	if _, err := tx.Exec(ctx, `
UPDATE space_invitations SET status = $2, resolved_at = now(), updated_at = now() WHERE id = $1`, id, status); err != nil {
		return fmt.Errorf("marking invitation %s %s: %w", id, status, err)
	}
	return nil
}

// person is the account behind an email or an id, as an invitation sees it.
type person struct {
	ID       uuid.UUID
	Username string
	Email    string
	External bool
	// Active: not blocked (deactivated). A deleted account is never a
	// person here: its email belongs to nobody (K-21).
	Active    bool
	Confirmed bool
}

const personCols = `id, username, lower(btrim(email)), is_external, deactivated_at IS NULL, email_confirmed_at IS NOT NULL`

func scanPerson(row pgx.Row) (person, bool, error) {
	var p person
	err := row.Scan(&p.ID, &p.Username, &p.Email, &p.External, &p.Active, &p.Confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("reading the account: %w", err)
	}
	return p, true, nil
}

// personByEmail is the account (not deleted) with the email, if any.
func personByEmail(ctx context.Context, q querier, email string) (person, bool, error) {
	return scanPerson(q.QueryRow(ctx, `
SELECT `+personCols+` FROM users WHERE lower(btrim(email)) = $1 AND deleted_at IS NULL`, email))
}

// personByID is the account (not deleted) id, if any.
func personByID(ctx context.Context, q querier, id uuid.UUID) (person, bool, error) {
	return scanPerson(q.QueryRow(ctx, `
SELECT `+personCols+` FROM users WHERE id = $1 AND deleted_at IS NULL`, id))
}

// isMember reports whether user is a participant of space.
func isMember(ctx context.Context, q querier, spaceID, user uuid.UUID) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`,
		spaceID, user).Scan(&ok); err != nil {
		return false, fmt.Errorf("reading the membership: %w", err)
	}
	return ok, nil
}

// lockSender serialises the sends of one sender across spaces, so the count
// of his sends and the new send are one step (the daily limit cannot be
// passed by two concurrent invitations to two spaces). A transaction-level
// advisory lock, taken after the space's lock — the same order on every
// path that sends — and released by the commit.
func lockSender(ctx context.Context, tx pgx.Tx, sender uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('skein.invitation_sender:' || $1::text, 0))`,
		sender); err != nil {
		return fmt.Errorf("locking the sender's sends: %w", err)
	}
	return nil
}

// sendCounts are the sender's sends in the daily window and in the captcha
// window (sliding, by the database's clock).
func sendCounts(ctx context.Context, q querier, sender uuid.UUID) (day, recent int, err error) {
	err = q.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE sent_at > now() - $3::bigint * interval '1 second')
  FROM space_invitation_sends
 WHERE sender_id = $1 AND sent_at > now() - $2::bigint * interval '1 second'`,
		sender, int64(dailyWindow/time.Second), int64(captchaWindow/time.Second)).Scan(&day, &recent)
	if err != nil {
		return 0, 0, fmt.Errorf("counting the sender's invitations: %w", err)
	}
	return day, recent, nil
}

// recordSend journals one send (creation or resend) of the invitation by
// sender.
func recordSend(ctx context.Context, tx pgx.Tx, sender, spaceID, invitationID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO space_invitation_sends (sender_id, space_id, invitation_id) VALUES ($1, $2, $3)`,
		sender, spaceID, invitationID); err != nil {
		return fmt.Errorf("journalling the send: %w", err)
	}
	return nil
}

// ownerView is the invitation as the space owner and the admin see it
// (spaces.SpaceInvitation): a pending one.
func ownerView(ctx context.Context, q querier, id uuid.UUID) (spaces.SpaceInvitation, error) {
	var v spaces.SpaceInvitation
	var expires time.Time
	err := q.QueryRow(ctx, `
SELECT i.email, u.username, i.expires_at, i.created_at
  FROM space_invitations i JOIN users u ON u.id = i.invited_by
 WHERE i.id = $1`, id).Scan(&v.Email, &v.InvitedBy, &expires, &v.CreatedAt)
	if err != nil {
		return v, fmt.Errorf("reading invitation %s: %w", id, err)
	}
	v.ID = id.String()
	v.Status = spaces.InvitationPending
	e := expires.UTC()
	v.ExpiresAt = &e
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

// confirmEmailInTx confirms user's email if it is not yet (US-5: the
// invited user opened the link of the invitation email — the link proves
// the mailbox) and reports whether it changed; the caller signals `inbox`
// then ("Accept" becomes active without a reload).
func confirmEmailInTx(ctx context.Context, tx pgx.Tx, user uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `
UPDATE users SET email_confirmed_at = now() WHERE id = $1 AND email_confirmed_at IS NULL`, user)
	if err != nil {
		return false, fmt.Errorf("confirming the email: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

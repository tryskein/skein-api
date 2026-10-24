// Package invitations owns the invitations to spaces (sk-674.18 T040, US-5,
// US-14, INV-13, INV-16, INV-17, SC-009, SC-021, E-1, E-3–E-8): sending,
// sending again, revoking, the addressee's answer from his Inbox and by the
// link of the invitation email, signup by invitation (with the accounts
// package) and the expiry of invitations nobody answered.
//
// The REST contract is spaces/contract.go (the routes with a session) and
// accounts/contract.go (the anonymous view and signup by invitation). Who may
// manage a space and what a seat is are the spaces package's (ManageInTx,
// SeatsRefusalInTx); the link tokens and the invitation emails are the token
// protocol's (tokens.Replace / Cancel / Use, K-11): a new token is issued
// only by the mail worker, and every operation here retires the old ones.
//
// # Life cycle
//
//	pending → accepted | declined | revoked | expired
//
// One pending invitation per (space, email) (the unique index of 000069).
// A pending invitation holds a seat until it expires (INV-13); it is
// accepted even if the limit was lowered meanwhile (K14). Sending again
// works only on a pending one: a new expiry, a new email with a new link;
// the old link stops working. Inviting the same email again while one is
// pending is that same invitation, not a second one.
//
// # Locks (K-7)
//
// Every operation on an invitation takes the space's row lock first, then —
// for a send — the sender's advisory lock (the daily limit is one count per
// sender across spaces), then the invitation's row (the token protocol's
// subject). The link routes read which space the token's invitation is in
// without a lock and take the space's lock before tokens.Use. The expiry
// worker locks invitation rows only and skips locked ones.
package invitations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/mail"
	"skein-api/internal/notifications"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/tokens"
)

// The windows of the limits of sending (both sliding, per sender, across
// every space; counted in space_invitation_sends — creation and resend).
const (
	// dailyWindow is the "day" of SKEIN_SPACE_INVITES_PER_DAY: the last 24
	// hours, not the calendar day (US-14 [spec]).
	dailyWindow = 24 * time.Hour
	// captchaWindow and captchaThreshold are the "suspicious frequency"
	// of the spec's "Restrictions" ("exceeding the suspicious frequency of
	// invitations — a Turnstile requirement (captcha_required), if the key is
	// set"; matrix rows 165, 165a): a sender who has already sent
	// captchaThreshold invitations in the last captchaWindow must pass the
	// captcha for the next one — while Turnstile is configured; without its
	// key there is no requirement. The spec names no number; this one is a
	// product decision (2026-10-14): ten, not five, so that an admin inviting
	// his team when connecting does not hit the captcha on the sixth email.
	// The key is the sender alone — not the space: another space does not
	// reset it, another sender is not affected.
	captchaWindow    = 10 * time.Minute
	captchaThreshold = 10
)

// Options are the installation settings the invitations answer by.
type Options struct {
	// SignupEnabled is SKEIN_SIGNUP_ENABLED: with registration open an
	// invitation to an email without an account is sent like any other, and
	// the answer never tells the two apart (SC-021).
	SignupEnabled bool
	// EmailEnabled is SKEIN_EMAIL_ENABLED: an invitation email is queued,
	// and accepting from the Inbox needs a confirmed email.
	EmailEnabled bool
	// InviteTTL is SKEIN_SPACE_INVITE_TTL.
	InviteTTL time.Duration
	// InvitesPerDay is SKEIN_SPACE_INVITES_PER_DAY; nil = no limit.
	InvitesPerDay *int
	// DeliveryTTL is SKEIN_EMAIL_DELIVERY_TTL for the invitation email.
	DeliveryTTL time.Duration
}

// Service runs the invitations.
type Service struct {
	pool    *pgxpool.Pool
	spaces  *spaces.Service
	captcha accounts.Captcha
	opts    Options
}

// NewService builds the service; sp decides the rights over a space and its
// seats, captcha is the Turnstile verifier of the installation.
func NewService(pool *pgxpool.Pool, sp *spaces.Service, captcha accounts.Captcha, opts Options) *Service {
	return &Service{pool: pool, spaces: sp, captcha: captcha, opts: opts}
}

// Refusals of the code registry (spaces/contract.go) the invitations answer
// with.
var (
	refusedInvitationNotFound = spaces.Refuse(http.StatusNotFound, "invitation_not_found", "invitation not found")
	refusedUserNotFound       = spaces.Refuse(http.StatusNotFound, "user_not_found", "user not found")
	refusedInvalid            = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeInvitationInvalid,
		"the invitation is no longer valid")
	refusedSpaceDeleted = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeSpaceDeleted,
		"the space of the invitation was deleted")
	refusedUnconfirmed = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeEmailUnconfirmed,
		"confirm your email to accept the invitation")
	refusedOtherEmail = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeInvitationOtherEmail,
		"the invitation is for another address")
	refusedNotPending = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeInvitationNotPending,
		"the invitation is no longer pending")
	refusedDailyLimit = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeDailyLimit,
		"the daily invitation limit is reached")
	refusedCaptchaRequired = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeCaptchaRequired,
		"too many invitations in a short time: complete the captcha")
	refusedCaptchaFailed = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeCaptchaFailed,
		"captcha not verified")
	refusedNoSuchUser = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeNoSuchUser,
		"no account has this email")
	refusedEmailDisabled = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeEmailDisabled,
		"outgoing mail is disabled and no account has this email")
	// RefusedAgent — INV-17: an agent (an API key) does not send, accept or
	// decline invitations; a person does it in the web UI.
	RefusedAgent = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeInvitationAgentForbidden,
		"invitations are sent, accepted and declined by a person in the web UI")
)

func refusedValidation(msg string) *spaces.Refusal {
	return spaces.Refuse(http.StatusBadRequest, "validation_error", msg)
}

// observe counts one public invitation operation — invite (creation and
// resend), accept, decline — in skein_space_operations_total exactly once,
// at its boundary: its success, every business refusal —
// a space or an invitation the caller may not see included — and every
// failure. A malformed request (400) never reached the operation and is not
// counted. err is returned mapped to the code registry.
func observe(operation string, err error) error {
	err = spaces.AsRefusal(err)
	var ref *spaces.Refusal
	if errors.As(err, &ref) && ref.Status == http.StatusBadRequest {
		return err
	}
	return spaces.Observe(operation, err)
}

// Outcomes of CreateInvitationResponse.
const (
	outcomeSent           = "sent"
	outcomeAlreadyMember  = "already_member"
	outcomeAlreadyPending = "already_pending"
)

// emailMaxLen bounds an invited email (the shape of accounts; the column's
// own bound is 320).
const emailMaxLen = 254

// actorOf names the caller in events.
func actorOf(c spaces.Caller) spaces.Actor {
	if c.Actor.Username == "" {
		return spaces.Actor{UserID: c.UserID, Username: c.Username, Type: "human"}
	}
	return c.Actor
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// Create answers POST /spaces/{id}/invitations (the owner and the admin).
//
// The order of the answers: the body (400 validation_error) → the space
// (404, INV-3) → personal_space → forbidden → the daily limit → a sent
// Turnstile token not verified (captcha_failed) → under the locks again the
// rights, the daily limit and the captcha requirement, then the person:
// external_user; already a member (200 already_member, nothing created); no
// account and registration closed — no_such_user (mail on) or email_disabled
// (mail off); a pending invitation — with registration open the neutral
// «sent» (nothing new), closed already_pending with it; → no_seats → the
// invitation.
//
// With registration open the answer for an email with and without an
// account is the same 200 {"outcome":"sent","invitation":null}, and the
// request takes the same steps — the invitation row, the journal, the email;
// the push and the signals of an existing addressee are rows written in the
// same transaction and leave after it (SC-009, SC-021).
func (s *Service) Create(ctx context.Context, c spaces.Caller, short string, req spaces.CreateInvitationRequest, clientIP string) (spaces.CreateInvitationResponse, error) {
	resp, err := s.create(ctx, c, short, req, clientIP)
	return resp, observe("invite", err)
}

func (s *Service) create(ctx context.Context, c spaces.Caller, short string, req spaces.CreateInvitationRequest, clientIP string) (spaces.CreateInvitationResponse, error) {
	var resp spaces.CreateInvitationResponse
	email, byID, err := validateCreate(req)
	if err != nil {
		return resp, err
	}
	sp, err := s.spaces.SeeManaged(ctx, c, short)
	if err != nil {
		return resp, err
	}
	verified, err := s.preflight(ctx, c.UserID, req.TurnstileToken, clientIP)
	if err != nil {
		return resp, err
	}
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		resp = spaces.CreateInvitationResponse{}
		locked, err := s.spaces.ManageInTx(ctx, tx, c, sp.ID, short)
		if err != nil {
			return err
		}
		if err := lockSender(ctx, tx, c.UserID); err != nil {
			return err
		}
		if err := s.sendLimitsInTx(ctx, tx, c.UserID, verified); err != nil {
			return err
		}
		to, found, err := s.addressee(ctx, tx, email, byID)
		if err != nil {
			return err
		}
		if found {
			if to.External {
				return spaces.RefusedExternal
			}
			member, err := isMember(ctx, tx, locked.ID, to.ID)
			if err != nil {
				return err
			}
			if member {
				resp.Outcome = outcomeAlreadyMember
				return nil
			}
			email = to.Email
		} else if !s.opts.SignupEnabled {
			if s.opts.EmailEnabled {
				return refusedNoSuchUser
			}
			return refusedEmailDisabled
		}
		pending, err := s.pendingFor(ctx, tx, locked.ID, email)
		if err != nil {
			return err
		}
		if pending != uuid.Nil {
			resp.Outcome = outcomeSent
			if !s.opts.SignupEnabled {
				v, err := ownerView(ctx, tx, pending)
				if err != nil {
					return err
				}
				resp.Outcome, resp.Invitation = outcomeAlreadyPending, &v
			}
			return nil
		}
		if err := s.spaces.SeatsRefusalInTx(ctx, tx, locked, ""); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, $4, now() + $5::bigint * interval '1 microsecond')
RETURNING id`, locked.ID, email, c.UserID, req.Language, s.opts.InviteTTL.Microseconds()).Scan(&id); err != nil {
			return fmt.Errorf("creating the invitation: %w", err)
		}
		if err := s.sendInTx(ctx, tx, c, locked, id, email, req.Language, to, found, false); err != nil {
			return err
		}
		resp.Outcome = outcomeSent
		return signalManagers(ctx, tx, locked.ID, locked.ShortID, c.UserID, c.UserID, "")
	})
	if err != nil {
		return spaces.CreateInvitationResponse{}, err
	}
	return resp, nil
}

// validateCreate checks the body: exactly one of email and userId, a
// supported language, an email of email shape.
func validateCreate(req spaces.CreateInvitationRequest) (email string, byID uuid.UUID, err error) {
	if (req.Email == nil) == (req.UserID == nil) {
		return "", uuid.Nil, refusedValidation("exactly one of email and userId is required")
	}
	if req.Language != mail.LangRU && req.Language != mail.LangEN {
		return "", uuid.Nil, refusedValidation("language: ru or en")
	}
	if req.UserID != nil {
		id, err := uuid.Parse(*req.UserID)
		if err != nil {
			return "", uuid.Nil, refusedValidation("userId must be a UUID")
		}
		return "", id, nil
	}
	email = strings.ToLower(strings.TrimSpace(*req.Email))
	if len(email) > emailMaxLen || !accounts.EmailRegex.MatchString(email) {
		return "", uuid.Nil, refusedValidation("email: must be a valid email address")
	}
	return email, uuid.Nil, nil
}

// preflight is the part of the send limits decided before any lock: the
// daily limit (a cheap first answer; decided again under the sender's lock)
// and the verification of a Turnstile token, if one came with the request —
// a call to Turnstile, which must not run while the space's lock is held. A
// token that does not verify is captcha_failed. A sender already at the
// captcha threshold without a token is answered captcha_required at once.
// Whether THIS send needs the captcha is decided under the sender's lock
// (sendLimitsInTx), on every attempt of the transaction: verified carries
// the fact that a token was verified here.
func (s *Service) preflight(ctx context.Context, sender uuid.UUID, token *string, clientIP string) (verified bool, err error) {
	day, recent, err := sendCounts(ctx, s.pool, sender)
	if err != nil {
		return false, err
	}
	if s.opts.InvitesPerDay != nil && day >= *s.opts.InvitesPerDay {
		return false, refusedDailyLimit
	}
	if !s.captcha.Enabled() {
		return false, nil
	}
	if token != nil && strings.TrimSpace(*token) != "" {
		if err := s.captcha.Verify(ctx, *token, clientIP); err != nil {
			return false, refusedCaptchaFailed
		}
		return true, nil
	}
	if recent >= captchaThreshold {
		return false, refusedCaptchaRequired
	}
	return false, nil
}

// sendLimitsInTx decides the limits of one send under the sender's lock
// (lockSender), on every attempt of the transaction — a retry after a
// serialisation failure reads the journal anew: the sends of the last 24
// hours against SKEIN_SPACE_INVITES_PER_DAY, then the sends of the captcha
// window — at the threshold, with Turnstile configured, the send passes only
// if a token was verified before the locks (verified). Concurrent sends of
// one sender are counted one after the other here, so a batch that passed
// preflight below the threshold cannot all go through.
func (s *Service) sendLimitsInTx(ctx context.Context, tx pgx.Tx, sender uuid.UUID, verified bool) error {
	day, recent, err := sendCounts(ctx, tx, sender)
	if err != nil {
		return err
	}
	if s.opts.InvitesPerDay != nil && day >= *s.opts.InvitesPerDay {
		return refusedDailyLimit
	}
	if s.captcha.Enabled() && recent >= captchaThreshold && !verified {
		return refusedCaptchaRequired
	}
	return nil
}

// addressee is the account an invitation goes to: by id (a person picked
// from the directory; a missing or deleted one is user_not_found) or by
// email (none, or a deleted one: not found).
func (s *Service) addressee(ctx context.Context, tx pgx.Tx, email string, byID uuid.UUID) (person, bool, error) {
	if byID == uuid.Nil {
		return personByEmail(ctx, tx, email)
	}
	p, found, err := personByID(ctx, tx, byID)
	if err != nil {
		return p, false, err
	}
	if !found {
		return p, false, refusedUserNotFound
	}
	return p, true, nil
}

// pendingFor is the live pending invitation of email to the space, if any.
// A pending one already past its expiry is closed as expired first — it
// holds no seat and no live link, and would block a new one on the unique
// index of pending invitations.
func (s *Service) pendingFor(ctx context.Context, tx pgx.Tx, spaceID uuid.UUID, email string) (uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
SELECT id, expires_at > now() FROM space_invitations
 WHERE space_id = $1 AND email = $2 AND status = 'pending'
   FOR NO KEY UPDATE`, spaceID, email)
	if err != nil {
		return uuid.Nil, fmt.Errorf("reading the pending invitation: %w", err)
	}
	var id uuid.UUID
	var live bool
	found := false
	for rows.Next() {
		if err := rows.Scan(&id, &live); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
		found = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, nil
	}
	if live {
		return id, nil
	}
	if err := resolve(ctx, tx, id, "expired"); err != nil {
		return uuid.Nil, err
	}
	if err := tokens.Cancel(ctx, tx, tokens.InvitationSubject(id)); err != nil {
		return uuid.Nil, err
	}
	return uuid.Nil, nil
}

// sendInTx is one send of invitation id, created or resent: the journal, the
// email (mail on; resend replaces the earlier ones), and for an existing
// active account the push and the signals.
func (s *Service) sendInTx(ctx context.Context, tx pgx.Tx, c spaces.Caller, sp spaces.ManagedSpace, id uuid.UUID,
	email, lang string, to person, found, resend bool) error {
	if err := recordSend(ctx, tx, c.UserID, sp.ID, id); err != nil {
		return err
	}
	if s.opts.EmailEnabled {
		m := mail.Message{Kind: mail.KindSpaceInvite, To: email, Lang: lang, InvitationID: id}
		var err error
		if resend {
			_, err = tokens.Replace(ctx, tx, m, s.opts.DeliveryTTL)
		} else {
			_, err = mail.Enqueue(ctx, tx, m, s.opts.DeliveryTTL)
		}
		if err != nil {
			return fmt.Errorf("queueing the invitation email: %w", err)
		}
	}
	if !found || !to.Active || to.External {
		return nil
	}
	if err := pushInTx(ctx, tx, to.ID, notifications.EventTypeSpaceInvitationSent, sp.ID, sp.ShortID, id, actorOf(c)); err != nil {
		return err
	}
	signalInvited(tx, to.ID, sp.ShortID, sp.Name)
	return nil
}

// Resend answers POST /spaces/{id}/invitations/{invId}/resend (the owner and
// the admin): a pending invitation gets a new expiry and a new email in the
// language stored at creation; its earlier links stop working (tokens.Replace).
// It is a send of the caller: the daily limit and the captcha rule apply as
// to a creation, keyed on whoever resends.
func (s *Service) Resend(ctx context.Context, c spaces.Caller, short string, invID uuid.UUID, token *string, clientIP string) (spaces.SpaceInvitation, error) {
	view, err := s.resend(ctx, c, short, invID, token, clientIP)
	return view, observe("invite", err)
}

func (s *Service) resend(ctx context.Context, c spaces.Caller, short string, invID uuid.UUID, token *string, clientIP string) (spaces.SpaceInvitation, error) {
	sp, err := s.spaces.SeeManaged(ctx, c, short)
	if err != nil {
		return spaces.SpaceInvitation{}, err
	}
	verified, err := s.preflight(ctx, c.UserID, token, clientIP)
	if err != nil {
		return spaces.SpaceInvitation{}, err
	}
	var view spaces.SpaceInvitation
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := s.spaces.ManageInTx(ctx, tx, c, sp.ID, short)
		if err != nil {
			return err
		}
		if err := lockSender(ctx, tx, c.UserID); err != nil {
			return err
		}
		inv, err := lockInvitation(ctx, tx, invID, &locked.ID)
		if errors.Is(err, errNoInvitation) {
			return refusedInvitationNotFound
		}
		if err != nil {
			return err
		}
		if !inv.Live {
			return refusedNotPending
		}
		if err := s.sendLimitsInTx(ctx, tx, c.UserID, verified); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE space_invitations
   SET expires_at = now() + $2::bigint * interval '1 microsecond', created_at = now(), updated_at = now()
 WHERE id = $1`, inv.ID, s.opts.InviteTTL.Microseconds()); err != nil {
			return fmt.Errorf("renewing the invitation: %w", err)
		}
		to, found, err := personByEmail(ctx, tx, inv.Email)
		if err != nil {
			return err
		}
		if err := s.sendInTx(ctx, tx, c, locked, inv.ID, inv.Email, inv.Lang, to, found, true); err != nil {
			return err
		}
		if err := signalManagers(ctx, tx, locked.ID, locked.ShortID, inv.InvitedBy, c.UserID, ""); err != nil {
			return err
		}
		view, err = ownerView(ctx, tx, inv.ID)
		return err
	})
	if err != nil {
		return spaces.SpaceInvitation{}, err
	}
	return view, nil
}

// Revoke answers DELETE /spaces/{id}/invitations/{invId} (the owner and the
// admin): a pending invitation is revoked — its seat is freed, its links and
// unsent emails retired, the addressee's card leaves his Inbox; a declined
// one still shown as not_accepted is removed from the list (marked revoked).
// Anything else is not found (the list does not show it).
func (s *Service) Revoke(ctx context.Context, c spaces.Caller, short string, invID uuid.UUID) error {
	sp, err := s.spaces.SeeManaged(ctx, c, short)
	if err != nil {
		return err
	}
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := s.spaces.ManageInTx(ctx, tx, c, sp.ID, short)
		if err != nil {
			return err
		}
		inv, err := lockInvitation(ctx, tx, invID, &locked.ID)
		if errors.Is(err, errNoInvitation) {
			return refusedInvitationNotFound
		}
		if err != nil {
			return err
		}
		switch inv.Status {
		case "pending":
			if err := revokePending(ctx, tx, inv); err != nil {
				return err
			}
		case "declined":
			// Only the row of the list goes: the answer stays «declined» in
			// substance, the resolution time is kept.
			if _, err := tx.Exec(ctx, `
UPDATE space_invitations SET status = 'revoked', updated_at = now() WHERE id = $1`, inv.ID); err != nil {
				return fmt.Errorf("removing the declined invitation: %w", err)
			}
		default:
			return refusedInvitationNotFound
		}
		return signalManagers(ctx, tx, locked.ID, locked.ShortID, inv.InvitedBy, c.UserID, "")
	})
	return spaces.AsRefusal(err)
}

// revokePending revokes a pending invitation locked by the caller: the
// status, its tokens retired and unsent emails cancelled (the link answers
// «invalid»), and the addressee's Inbox and spaces re-read if the email is
// an account's. The managers' signal is the caller's.
func revokePending(ctx context.Context, tx pgx.Tx, inv invitation) error {
	if _, err := tx.Exec(ctx, `
UPDATE space_invitations SET status = 'revoked', resolved_at = now(), updated_at = now() WHERE id = $1`, inv.ID); err != nil {
		return fmt.Errorf("revoking the invitation: %w", err)
	}
	if err := tokens.Cancel(ctx, tx, tokens.InvitationSubject(inv.ID)); err != nil {
		return err
	}
	to, found, err := personByEmail(ctx, tx, inv.Email)
	if err != nil {
		return err
	}
	if found {
		signals.Add(tx, to.ID, signals.AreaInbox, signals.AreaSpaces)
	}
	return nil
}

// RevokePendingInTx is the revocation of a pending invitation for an
// operation of which it is one step — the account deletion (sk-674.18
// T041) — inside the caller's transaction (opened by
// signals.BeginTxFunc), with the space of the invitation already locked
// by the caller (K-7). It does what Revoke does to a pending invitation,
// signals included: the addressee's Inbox and spaces, the space's owner and
// the sender (except actor). An invitation no longer pending is left as
// it is.
func RevokePendingInTx(ctx context.Context, tx pgx.Tx, invID, actor uuid.UUID) error {
	inv, err := lockInvitation(ctx, tx, invID, nil)
	if errors.Is(err, errNoInvitation) {
		return nil
	}
	if err != nil {
		return err
	}
	if inv.Status != "pending" || inv.SpaceID == nil {
		return nil
	}
	if err := revokePending(ctx, tx, inv); err != nil {
		return err
	}
	var short string
	if err := tx.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, *inv.SpaceID).Scan(&short); err != nil {
		return fmt.Errorf("reading the space of invitation %s: %w", invID, err)
	}
	return signalManagers(ctx, tx, *inv.SpaceID, short, inv.InvitedBy, actor, "")
}

// ---------------------------------------------------------------------------
// The addressee — from the Inbox
// ---------------------------------------------------------------------------

// Mine answers GET /me/invitations: the pending invitations to the caller,
// newest first, with whether accepting would succeed now (mail on and the
// email not confirmed: no — email_unconfirmed). Never the token or the link.
func (s *Service) Mine(ctx context.Context, c spaces.Caller) (spaces.MyInvitationsResponse, error) {
	resp := spaces.MyInvitationsResponse{Items: []spaces.MyInvitation{}}
	me, found, err := personByID(ctx, s.pool, c.UserID)
	if err != nil || !found {
		return resp, err
	}
	var reason *string
	if s.opts.EmailEnabled && !me.Confirmed {
		code := spaces.CodeEmailUnconfirmed
		reason = &code
	}
	rows, err := s.pool.Query(ctx, `
SELECT si.id, s.short_id, s.name, u.username, si.expires_at
  FROM space_invitations si
  JOIN spaces s ON s.id = si.space_id
  JOIN users u ON u.id = si.invited_by
 WHERE `+spaces.PendingToMeWhere("$1::uuid")+`
 ORDER BY si.created_at DESC, si.id`, c.UserID)
	if err != nil {
		return resp, fmt.Errorf("reading the invitations to %s: %w", c.UserID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var m spaces.MyInvitation
		var id uuid.UUID
		if err := rows.Scan(&id, &m.SpaceID, &m.SpaceName, &m.InvitedBy, &m.ExpiresAt); err != nil {
			return resp, fmt.Errorf("reading the invitations to %s: %w", c.UserID, err)
		}
		m.ID, m.ExpiresAt = id.String(), m.ExpiresAt.UTC()
		m.CanAccept, m.CannotAcceptReason = reason == nil, reason
		resp.Items = append(resp.Items, m)
	}
	return resp, rows.Err()
}

// mineFor reads invitation invID as an invitation to the caller: one to
// another address is not found, exactly like a nonexistent id (INV-3).
func (s *Service) mineFor(ctx context.Context, c spaces.Caller, invID uuid.UUID) (invitation, person, error) {
	me, found, err := personByID(ctx, s.pool, c.UserID)
	if err != nil {
		return invitation{}, me, err
	}
	inv, err := readInvitation(ctx, s.pool, invID)
	if errors.Is(err, errNoInvitation) || (err == nil && (!found || inv.Email != me.Email)) {
		return inv, me, refusedInvitationNotFound
	}
	return inv, me, err
}

// AcceptMine answers POST /me/invitations/{invId}/accept: the invited person
// joins the space. With mail on his email must be confirmed (INV-16; the
// link of the invitation email confirms it instead). The seat is not
// checked again (K14).
func (s *Service) AcceptMine(ctx context.Context, c spaces.Caller, invID uuid.UUID) (spaces.AcceptInvitationResponse, error) {
	spaceID, err := s.acceptMine(ctx, c, invID)
	if err := observe("accept", err); err != nil {
		return spaces.AcceptInvitationResponse{}, err
	}
	return s.joined(ctx, c, spaceID)
}

func (s *Service) acceptMine(ctx context.Context, c spaces.Caller, invID uuid.UUID) (uuid.UUID, error) {
	inv, me, err := s.mineFor(ctx, c, invID)
	if err != nil {
		return uuid.Nil, err
	}
	if s.opts.EmailEnabled && !me.Confirmed {
		return uuid.Nil, refusedUnconfirmed
	}
	return s.answerMine(ctx, c, inv, true)
}

// DeclineMine answers POST /me/invitations/{invId}/decline. Declining needs
// no confirmed email: it gives nobody anything.
func (s *Service) DeclineMine(ctx context.Context, c spaces.Caller, invID uuid.UUID) error {
	inv, _, err := s.mineFor(ctx, c, invID)
	if err == nil {
		_, err = s.answerMine(ctx, c, inv, false)
	}
	return observe("decline", err)
}

// joined is the body of an accepted invitation: the space joined, as
// GET /me/spaces lists it.
func (s *Service) joined(ctx context.Context, c spaces.Caller, spaceID uuid.UUID) (spaces.AcceptInvitationResponse, error) {
	mine, err := s.spaces.MySpaceOf(ctx, c, spaceID)
	if err != nil {
		return spaces.AcceptInvitationResponse{}, err
	}
	return spaces.AcceptInvitationResponse{Space: mine}, nil
}

// answerMine accepts or declines from the Inbox, under the space's lock:
// the invitation still pending and unexpired, still addressed to the
// caller's email (and, to accept, the email still confirmed while mail is
// on); its links and unsent emails retired. It returns the space.
func (s *Service) answerMine(ctx context.Context, c spaces.Caller, inv invitation, accept bool) (uuid.UUID, error) {
	if inv.SpaceID == nil {
		return uuid.Nil, refusedSpaceDeleted
	}
	spaceID := *inv.SpaceID
	err := spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		sp, err := spaces.LockSpace(ctx, tx, spaceID)
		if errors.Is(err, spaces.ErrSpaceNotFound) {
			return refusedSpaceDeleted
		}
		if err != nil {
			return err
		}
		locked, err := lockInvitation(ctx, tx, inv.ID, nil)
		if err != nil {
			return err
		}
		me, found, err := personByID(ctx, tx, c.UserID)
		if err != nil {
			return err
		}
		if !found || locked.Email != me.Email {
			return refusedInvitationNotFound
		}
		if !locked.Live {
			return refusedInvalid
		}
		if accept && s.opts.EmailEnabled && !me.Confirmed {
			return refusedUnconfirmed
		}
		if err := tokens.Cancel(ctx, tx, tokens.InvitationSubject(locked.ID)); err != nil {
			return err
		}
		if accept {
			return acceptInTx(ctx, tx, locked, sp.ShortID, me.ID, actorOf(c))
		}
		return declineInTx(ctx, tx, locked, sp.ShortID, me.ID)
	})
	return spaceID, err
}

// acceptInTx closes the locked pending invitation as accepted and makes the
// user a participant of its space (spaces.JoinInTx: the space's projects,
// the event, `added`); the sender gets `accepted` and the push (US-5), the
// owner a re-read of the list, the user an `inbox` re-read (the card goes).
// The caller holds the space's lock and the invitation's, and has used or
// cancelled its tokens.
func acceptInTx(ctx context.Context, tx pgx.Tx, inv invitation, spaceShort string, user uuid.UUID, actor spaces.Actor) error {
	if err := resolve(ctx, tx, inv.ID, "accepted"); err != nil {
		return err
	}
	if err := spaces.JoinInTx(ctx, tx, *inv.SpaceID, user, actor); err != nil {
		return err
	}
	signals.Add(tx, user, signals.AreaInbox)
	if inv.InvitedBy != user {
		sender, found, err := personByID(ctx, tx, inv.InvitedBy)
		if err != nil {
			return err
		}
		if found && sender.Active && !sender.External {
			if err := pushInTx(ctx, tx, sender.ID, notifications.EventTypeSpaceInvitationAccepted, *inv.SpaceID,
				spaceShort, inv.ID, actor); err != nil {
				return err
			}
		}
		signals.AddSpaceEvent(tx, inv.InvitedBy, signals.SpaceEvent{Reason: signals.SpaceAccepted, SpaceID: spaceShort})
	}
	return signalManagers(ctx, tx, *inv.SpaceID, spaceShort, inv.InvitedBy, user, "")
}

// declineInTx closes the locked pending invitation as declined: it frees its
// seat and stays in the list as a neutral "not accepted" for 7 days; the
// owner and the sender get `declined`, the user an `inbox` re-read.
func declineInTx(ctx context.Context, tx pgx.Tx, inv invitation, spaceShort string, user uuid.UUID) error {
	if err := resolve(ctx, tx, inv.ID, "declined"); err != nil {
		return err
	}
	signals.Add(tx, user, signals.AreaInbox, signals.AreaSpaces)
	return signalManagers(ctx, tx, *inv.SpaceID, spaceShort, inv.InvitedBy, user, signals.SpaceDeclined)
}

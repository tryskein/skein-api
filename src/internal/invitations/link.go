package invitations

// link.go — the link of the invitation email, `/invite/:token` (sk-674.18
// T040, US-5, E-3–E-7; screens 2.5, 2.7):
//
//   - View — GET /invitations/{token} for a logged-in caller: the state, in
//     the order of screens 2.5 (the space deleted → already a member →
//     invalid → another address → valid); opened by the invited person it
//     confirms his email — the link proves the mailbox — and accepts nothing;
//   - AcceptByToken / DeclineByToken — the explicit answer by the link
//     (tokens.Use in the answer's transaction: a refusal after it leaves the
//     link unused);
//   - Preview — the anonymous view (accounts/contract.go#InvitationPreview);
//   - SignupStep — the invitation's part of signup by invitation
//     (accounts.InvitationStep).

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/accounts"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/tokens"
)

// linkFacts is what the link's states are read from.
type linkFacts struct {
	inv        invitation
	tokenLive  bool
	spaceShort string
	spaceName  string
	inviter    string
	// hasAccount: an account (not deleted) has the invited email.
	hasAccount bool
}

// readLink reads the invitation of raw: ok=false for an unknown token or
// one of another purpose — "invalid".
func readLink(ctx context.Context, q querier, raw string) (linkFacts, bool, error) {
	var f linkFacts
	t, err := tokens.PeekInvitation(ctx, q, raw)
	if errors.Is(err, tokens.ErrInvalid) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	f.tokenLive = t.Live
	var short, name *string
	inv, err := scanInvitation(q.QueryRow(ctx, `SELECT `+invitationCols+` FROM space_invitations WHERE id = $1`, t.InvitationID))
	if err != nil {
		return f, false, err
	}
	f.inv = inv
	if err := q.QueryRow(ctx, `
SELECT s.short_id, s.name, u.username,
       EXISTS (SELECT 1 FROM users x WHERE lower(btrim(x.email)) = $3 AND x.deleted_at IS NULL)
  FROM users u LEFT JOIN spaces s ON s.id = $2
 WHERE u.id = $1`, inv.InvitedBy, inv.SpaceID, inv.Email).Scan(&short, &name, &f.inviter, &f.hasAccount); err != nil {
		return f, false, err
	}
	if short != nil {
		f.spaceShort, f.spaceName = *short, *name
	}
	return f, true, nil
}

// valid: the link can still be answered — its token is live and the
// invitation pending and unexpired.
func (f linkFacts) valid() bool { return f.tokenLive && f.inv.Live }

func (f linkFacts) spaceRef() *spaces.SpaceRef {
	return &spaces.SpaceRef{ID: f.spaceShort, Slug: spaces.Slug(f.spaceName), Name: f.spaceName}
}

// View answers GET /invitations/{token} for a logged-in caller (screens 2.5).
// Opening a valid link as the invited person confirms his email (US-5: the
// closed set "the logged-in addressee opened the link from the invitation email") and
// signals `inbox`; nothing is accepted. Another address is not named.
func (s *Service) View(ctx context.Context, c spaces.Caller, raw string) (spaces.TokenInvitationView, error) {
	f, ok, err := readLink(ctx, s.pool, raw)
	if err != nil || !ok {
		return spaces.TokenInvitationView{Status: spaces.TokenInvalid}, err
	}
	if f.inv.SpaceID == nil {
		return spaces.TokenInvitationView{Status: spaces.TokenSpaceDeleted}, nil
	}
	me, found, err := personByID(ctx, s.pool, c.UserID)
	if err != nil {
		return spaces.TokenInvitationView{}, err
	}
	addressee := found && me.Email == f.inv.Email
	if addressee && f.valid() && !me.Confirmed {
		if err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			changed, err := confirmEmailInTx(ctx, tx, c.UserID)
			if changed {
				signals.Add(tx, c.UserID, signals.AreaInbox)
			}
			return err
		}); err != nil {
			return spaces.TokenInvitationView{}, err
		}
	}
	member, err := isMember(ctx, s.pool, *f.inv.SpaceID, c.UserID)
	if err != nil {
		return spaces.TokenInvitationView{}, err
	}
	switch {
	case member:
		return spaces.TokenInvitationView{Status: spaces.TokenAlreadyMember, Space: f.spaceRef()}, nil
	case !f.valid():
		return spaces.TokenInvitationView{Status: spaces.TokenInvalid}, nil
	case !addressee:
		return spaces.TokenInvitationView{Status: spaces.TokenOtherEmail}, nil
	}
	inviter := f.inviter
	expires := f.inv.ExpiresAt.UTC()
	return spaces.TokenInvitationView{Status: spaces.TokenValid, Space: f.spaceRef(),
		InviterUsername: &inviter, ExpiresAt: &expires}, nil
}

// AcceptByToken answers POST /invitations/{token}/accept: the explicit
// acceptance by the link. Refusals: space_deleted, invitation_invalid
// (expired, revoked, used — E-3: of an acceptance and a revocation exactly
// one wins), invitation_other_email. The email is confirmed with it (the
// link proves the mailbox); the seat is not checked again (K14).
func (s *Service) AcceptByToken(ctx context.Context, c spaces.Caller, raw string) (spaces.AcceptInvitationResponse, error) {
	spaceID, err := s.answerByToken(ctx, c, raw, true)
	if err := observe("accept", err); err != nil {
		return spaces.AcceptInvitationResponse{}, err
	}
	return s.joined(ctx, c, spaceID)
}

// DeclineByToken answers POST /invitations/{token}/decline, with the
// refusals of AcceptByToken.
func (s *Service) DeclineByToken(ctx context.Context, c spaces.Caller, raw string) error {
	_, err := s.answerByToken(ctx, c, raw, false)
	return observe("decline", err)
}

// answerByToken reads which space the token's invitation is in, takes that
// space's lock and only then uses the token (tokens.Use takes the
// invitation's lock: the order space → invitation of every operation).
func (s *Service) answerByToken(ctx context.Context, c spaces.Caller, raw string, accept bool) (uuid.UUID, error) {
	f, ok, err := readLink(ctx, s.pool, raw)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, refusedInvalid
	}
	if f.inv.SpaceID == nil {
		return uuid.Nil, refusedSpaceDeleted
	}
	spaceID := *f.inv.SpaceID
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		sp, err := spaces.LockSpace(ctx, tx, spaceID)
		if errors.Is(err, spaces.ErrSpaceNotFound) {
			return refusedSpaceDeleted
		}
		if err != nil {
			return err
		}
		me, found, err := personByID(ctx, tx, c.UserID)
		if err != nil {
			return err
		}
		if !found {
			return refusedOtherEmail
		}
		used, err := tokens.Use(ctx, tx, tokens.PurposeSpaceInvite, raw, me.Email)
		switch {
		case errors.Is(err, tokens.ErrInvalid):
			return refusedInvalid
		case errors.Is(err, tokens.ErrNotAddressee):
			return refusedOtherEmail
		case err != nil:
			return err
		}
		inv, err := lockInvitation(ctx, tx, used.InvitationID, &spaceID)
		if err != nil {
			return err
		}
		changed, err := confirmEmailInTx(ctx, tx, me.ID)
		if err != nil {
			return err
		}
		if changed {
			signals.Add(tx, me.ID, signals.AreaInbox)
		}
		if accept {
			return acceptInTx(ctx, tx, inv, sp.ShortID, me.ID, actorOf(c))
		}
		return declineInTx(ctx, tx, inv, sp.ShortID, me.ID)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return spaceID, nil
}

// Preview implements accounts.Invitations: the view of the link for a
// visitor without a session — the space deleted → invalid → valid, with the
// invited email and whether an account has it only when valid (a dead link
// reveals nothing). It changes nothing.
func (s *Service) Preview(ctx context.Context, raw string) (accounts.InvitationPreview, error) {
	f, ok, err := readLink(ctx, s.pool, raw)
	switch {
	case err != nil:
		return accounts.InvitationPreview{}, err
	case !ok:
		return accounts.InvitationPreview{Status: accounts.InvitationInvalid}, nil
	case f.inv.SpaceID == nil:
		return accounts.InvitationPreview{Status: accounts.InvitationSpaceDeleted}, nil
	case !f.valid():
		return accounts.InvitationPreview{Status: accounts.InvitationInvalid}, nil
	}
	inviter, email, has := f.inviter, f.inv.Email, f.hasAccount
	return accounts.InvitationPreview{
		Status:          accounts.InvitationValid,
		Space:           &accounts.InvitationSpace{ID: f.spaceShort, Slug: spaces.Slug(f.spaceName), Name: f.spaceName},
		InviterUsername: &inviter,
		Email:           &email,
		HasAccount:      &has,
	}, nil
}

// SignupStep implements accounts.Invitations: the invitation's part of the
// new account's transaction.
func (s *Service) SignupStep(raw, email string) accounts.InvitationStep {
	return &signupStep{raw: raw, email: email}
}

// signupStep joins a new account to the space of the invitation of raw
// inside the account's transaction (users.Store.CreateSelfService).
type signupStep struct {
	raw, email string
	spaceID    uuid.UUID
	space      accounts.InvitationSpace
}

// BeforeInTx locks the invitation's space first (K-7). A deleted space is
// space_deleted, an unknown token invitation_invalid.
func (st *signupStep) BeforeInTx(ctx context.Context, tx pgx.Tx) error {
	f, ok, err := readLink(ctx, tx, st.raw)
	if err != nil {
		return err
	}
	if !ok {
		return &accounts.Refusal{Code: accounts.CodeInvitationInvalid, Message: "the invitation is invalid or has expired"}
	}
	if f.inv.SpaceID == nil {
		return &accounts.Refusal{Code: accounts.CodeSpaceDeleted, Message: "the space of the invitation was deleted"}
	}
	sp, err := spaces.LockSpace(ctx, tx, *f.inv.SpaceID)
	if errors.Is(err, spaces.ErrSpaceNotFound) {
		return &accounts.Refusal{Code: accounts.CodeSpaceDeleted, Message: "the space of the invitation was deleted"}
	}
	if err != nil {
		return err
	}
	st.spaceID = sp.ID
	st.space = accounts.InvitationSpace{ID: sp.ShortID, Slug: spaces.Slug(sp.Name), Name: sp.Name}
	return nil
}

// AfterInTx uses the token for the new account (an expired, revoked or used
// link is invitation_invalid and rolls the account back — E-7), accepts the
// invitation and joins the space. The account's email is already confirmed
// by its creation.
func (st *signupStep) AfterInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, username string) error {
	used, err := tokens.Use(ctx, tx, tokens.PurposeSpaceInvite, st.raw, st.email)
	if errors.Is(err, tokens.ErrInvalid) || errors.Is(err, tokens.ErrNotAddressee) {
		return &accounts.Refusal{Code: accounts.CodeInvitationInvalid, Message: "the invitation is invalid or has expired"}
	}
	if err != nil {
		return err
	}
	inv, err := lockInvitation(ctx, tx, used.InvitationID, &st.spaceID)
	if err != nil {
		return err
	}
	return acceptInTx(ctx, tx, inv, st.space.ID, userID, spaces.Actor{UserID: userID, Username: username, Type: "human"})
}

// Space is the space joined, after the transaction committed.
func (st *signupStep) Space() accounts.InvitationSpace { return st.space }

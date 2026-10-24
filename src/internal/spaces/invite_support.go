package spaces

// invite_support.go — the pieces of the spaces surface the invitations
// package decides with (sk-674.18 T040): the right to manage a space and the
// seats (INV-13), decided exactly as this package decides them for its own
// operations, the space as GET /me/spaces lists it, the codes of the
// registry and the operation counter. The invitations package owns the
// invitation's life cycle; who may act on a space and what a seat is stay
// here, in one copy.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Refuse builds a refusal of the code registry (contract.go).
func Refuse(status int, code, msg string) *Refusal { return refuse(status, code, msg) }

// The refusals of the registry the invitations package answers with as
// they are (one value each, compared by code by the handlers).
var (
	RefusedNotFound  = refusedNotFound
	RefusedForbidden = refusedForbidden
	RefusedPersonal  = refusedPersonal
	RefusedExternal  = refusedExternal
)

// AsRefusal maps the primitives' sentinel errors (ErrSpaceNotFound,
// ErrPersonalSpace, ErrExternalUser, ErrConcurrentChange, …) to their codes;
// any other error is returned as is.
func AsRefusal(err error) error { return asRefusal(err) }

// Observe counts one operation of the spaces surface performed by the
// invitations package (invite, accept, decline) in
// skein_space_operations_total — and a limit refusal in
// skein_quota_refusals_total — exactly as the service counts its own, and
// returns err.
func Observe(operation string, err error) error { return observe(operation, err) }

// ManagedSpace is a space whose management the caller was just granted under
// its lock.
type ManagedSpace struct {
	ID      uuid.UUID
	ShortID string
	Kind    string
	Name    string
}

// SeeManaged is the preliminary read of a management request through the
// pool (no lock): the space seen by the caller and whether he may manage it.
// RefusedNotFound for a malformed, nonexistent or unseen id (INV-3),
// RefusedPersonal for a "Personal" (decision No. 6: before the role),
// RefusedForbidden for a member who does not manage it.
func (s *Service) SeeManaged(ctx context.Context, c Caller, short string) (ManagedSpace, error) {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return ManagedSpace{}, err
	}
	switch {
	case a.row.Kind == KindPersonal:
		return ManagedSpace{}, refusedPersonal
	case !a.manages:
		return ManagedSpace{}, refusedForbidden
	}
	return managed(a), nil
}

// ManageInTx takes the space's lock — the first statement of the write
// (K-7) — and decides the caller's right to manage it again inside tx,
// with the caller's role and admin flag read under their own locks (review
// api/6 #1): the refusals of SeeManaged, as they are now.
func (s *Service) ManageInTx(ctx context.Context, tx pgx.Tx, c Caller, spaceID uuid.UUID, short string) (ManagedSpace, error) {
	if _, err := LockSpace(ctx, tx, spaceID); err != nil {
		return ManagedSpace{}, err
	}
	a, err := s.manageInTx(ctx, tx, c, short)
	if err != nil {
		return ManagedSpace{}, err
	}
	return managed(a), nil
}

func managed(a access) ManagedSpace {
	return ManagedSpace{ID: a.row.ID, ShortID: a.row.ShortID, Kind: a.row.Kind, Name: a.row.Name}
}

// MemberLimit is SKEIN_SPACE_MEMBER_LIMIT as the service applies it.
func (s *Service) MemberLimit() *int { return s.opts.MemberLimit }

// SeatsRefusalInTx is the seat check of INV-13 for one more person in the
// space, under the space's lock held by the caller: participants plus valid
// pending invitations (the invitations of exceptEmail not counted — that
// person's own invitation already holds his seat; "" counts all) reach
// SKEIN_SPACE_MEMBER_LIMIT → the CodeNoSeats refusal with its fields; nil
// when a seat is free. The limit applies to regular spaces only.
func (s *Service) SeatsRefusalInTx(ctx context.Context, tx pgx.Tx, sp ManagedSpace, exceptEmail string) error {
	limit := s.opts.MemberLimit
	if sp.Kind != KindRegular || limit == nil {
		return nil
	}
	var count, pending int
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM space_members WHERE space_id = $1),
       (SELECT count(*) FROM space_invitations i
         WHERE i.space_id = $1 AND `+validPendingExpr+` AND lower(i.email) <> $2)`,
		sp.ID, exceptEmail).Scan(&count, &pending); err != nil {
		return fmt.Errorf("reading the seats: %w", err)
	}
	if count+pending < *limit {
		return nil
	}
	r := refuse(http.StatusUnprocessableEntity, CodeNoSeats,
		fmt.Sprintf("no free seats: %d of %d taken", count+pending, *limit))
	r.Extra = map[string]any{"memberCount": count, "pendingInvitations": pending,
		"memberLimit": *limit, "spaceId": sp.ShortID}
	return r
}

// MySpaceOf is the space spaceID as GET /me/spaces lists it for the caller —
// the body of an accepted invitation. ErrSpaceNotFound (as a refusal) when
// he does not see it.
func (s *Service) MySpaceOf(ctx context.Context, c Caller, spaceID uuid.UUID) (MySpace, error) {
	var short string
	if err := s.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, spaceID).Scan(&short); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MySpace{}, refusedNotFound
		}
		return MySpace{}, fmt.Errorf("reading space %s: %w", spaceID, err)
	}
	row, err := seenSpace(ctx, s.pool, c.UserID, short)
	if err != nil {
		return MySpace{}, asRefusal(err)
	}
	return s.mySpace(row), nil
}

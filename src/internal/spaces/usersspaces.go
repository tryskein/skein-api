package spaces

// usersspaces.go — a user's spaces at the admin's hands (sk-674.18 T037,
// US-6, US-20, screens 1.14 "Users", K6): the checkboxes of spaces
// when an account is created and when it is edited — the set saved in one
// operation, with one preview of the consequences of every removal — and
// the spaces a user owns, which the deactivation dialog names ("Owns: …
// — change the owner", E-15).
//
// A checked space makes the user a participant at once, without consent
// (the admin's direct addition, addMemberInTx: seats, `added`); an
// unchecked one is a removal (US-7, LeaveInTx with its INV-12
// consequences). A space the user owns cannot be unchecked until its owner
// is changed (INV-10). Personal spaces are not part of the set. External
// users are in no space (E-16).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
)

// spaceOfSet is one space named in a set.
type spaceOfSet struct {
	ID      uuid.UUID
	ShortID string
	Kind    string
	Name    string
}

// resolveSet resolves the short ids of a wanted set: each must be an
// existing non-personal space (the admin sees every one of them).
// Duplicates collapse. A "Personal" is seen by its owner alone (INV-8): one
// of another user answers as a space that does not exist — no existence
// oracle; the caller's own is refused as personal.
func resolveSet(ctx context.Context, q querier, caller uuid.UUID, shorts []string) (map[uuid.UUID]spaceOfSet, error) {
	out := make(map[uuid.UUID]spaceOfSet, len(shorts))
	for _, short := range shorts {
		if !shortIDPattern.MatchString(short) {
			return nil, refusedNotFound
		}
		var sp spaceOfSet
		var owner uuid.UUID
		err := q.QueryRow(ctx, `SELECT id, short_id, kind, name, owner_id FROM spaces WHERE short_id = $1`, short).
			Scan(&sp.ID, &sp.ShortID, &sp.Kind, &sp.Name, &owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refusedNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("reading space %s: %w", short, err)
		}
		if sp.Kind == KindPersonal && owner != caller {
			return nil, refusedNotFound
		}
		if sp.Kind == KindPersonal {
			return nil, refusedPersonal
		}
		out[sp.ID] = sp
	}
	return out, nil
}

// sortedIDs orders space ids, so a set operation locks its spaces in one
// order and two of them cannot deadlock on each other's order.
func sortedIDs(m map[uuid.UUID]spaceOfSet) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// JoinSpacesInTx makes the new user userID a participant of every space of
// shorts, inside the transaction that created him (POST /users with
// spaceIds, the admin's agent alike): the admin's direct addition per space,
// seats checked. An external user may be given no space (external_user).
func JoinSpacesInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, shorts []string, memberLimit *int, actor Actor) error {
	if len(shorts) == 0 {
		return nil
	}
	set, err := resolveSet(ctx, tx, actor.UserID, shorts)
	if err != nil {
		return err
	}
	for _, id := range sortedIDs(set) {
		if _, err := addMemberInTx(ctx, tx, id, userID, memberLimit, actor); err != nil {
			// A refusal ends the creating transaction — counted once.
			return observeLimitRefusal(asRefusal(err))
		}
	}
	return nil
}

// userTarget reads the user whose spaces are managed: existing and not
// deleted (404 user_not_found otherwise); external tells he may have none.
func userTarget(ctx context.Context, q querier, userID uuid.UUID) (external bool, err error) {
	var deleted bool
	err = q.QueryRow(ctx, `SELECT is_external, deleted_at IS NOT NULL FROM users WHERE id = $1`, userID).
		Scan(&external, &deleted)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && deleted) {
		return false, refusedUserNotFound
	}
	if err != nil {
		return false, fmt.Errorf("reading the user: %w", err)
	}
	return external, nil
}

// userMemberships are the user's participations outside personal spaces,
// in column order.
func userMemberships(ctx context.Context, q querier, userID uuid.UUID) ([]UserSpace, map[uuid.UUID]string, error) {
	rows, err := q.Query(ctx, `
SELECT s.id, s.short_id, s.name, s.kind, sm.role
  FROM space_members sm JOIN spaces s ON s.id = sm.space_id
 WHERE sm.user_id = $1 AND s.kind <> 'personal'
 ORDER BY CASE s.kind WHEN 'common' THEN 1 ELSE 2 END, lower(s.name), s.id`, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the user's spaces: %w", err)
	}
	defer rows.Close()
	items := []UserSpace{}
	roles := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var it UserSpace
		if err := rows.Scan(&id, &it.ID, &it.Name, &it.Kind, &it.Role); err != nil {
			return nil, nil, fmt.Errorf("reading the user's spaces: %w", err)
		}
		it.Slug = Slug(it.Name)
		items = append(items, it)
		roles[id] = it.Role
	}
	return items, roles, rows.Err()
}

// UserSpaces answers GET /users/{id}/spaces (the admin): the user's
// participations outside personal spaces, with the role — the owner rows
// are what the deactivation dialog names.
func (s *Service) UserSpaces(ctx context.Context, c Caller, userID uuid.UUID) (UserSpacesResponse, error) {
	if err := s.admin(ctx, c); err != nil {
		return UserSpacesResponse{}, err
	}
	if _, err := userTarget(ctx, s.pool, userID); err != nil {
		return UserSpacesResponse{}, err
	}
	items, _, err := userMemberships(ctx, s.pool, userID)
	if err != nil {
		return UserSpacesResponse{}, err
	}
	return UserSpacesResponse{Items: items}, nil
}

// setChange is what saving a wanted set changes.
type setChange struct {
	added   []uuid.UUID
	removed []uuid.UUID
	spaces  map[uuid.UUID]spaceOfSet
}

// planSet validates a wanted set against the user's participations: the
// user exists; an external user gets no space; every space is a
// non-personal one; no owned space is left out (owner_cannot_be_removed,
// with spaceId).
func planSet(ctx context.Context, tx pgx.Tx, caller, userID uuid.UUID, shorts []string) (setChange, error) {
	external, err := userTarget(ctx, tx, userID)
	if err != nil {
		return setChange{}, err
	}
	if external && len(shorts) > 0 {
		return setChange{}, refusedExternal
	}
	wanted, err := resolveSet(ctx, tx, caller, shorts)
	if err != nil {
		return setChange{}, err
	}
	rows, err := tx.Query(ctx, `
SELECT s.id, s.short_id, s.kind, s.name, sm.role
  FROM space_members sm JOIN spaces s ON s.id = sm.space_id
 WHERE sm.user_id = $1 AND s.kind <> 'personal'`, userID)
	if err != nil {
		return setChange{}, fmt.Errorf("reading the user's spaces: %w", err)
	}
	current := map[uuid.UUID]spaceOfSet{}
	roles := map[uuid.UUID]string{}
	for rows.Next() {
		var sp spaceOfSet
		var role string
		if err := rows.Scan(&sp.ID, &sp.ShortID, &sp.Kind, &sp.Name, &role); err != nil {
			rows.Close()
			return setChange{}, fmt.Errorf("reading the user's spaces: %w", err)
		}
		current[sp.ID], roles[sp.ID] = sp, role
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return setChange{}, err
	}
	ch := setChange{spaces: map[uuid.UUID]spaceOfSet{}}
	for _, id := range sortedIDs(current) {
		if _, keep := wanted[id]; keep {
			continue
		}
		if roles[id] == RoleOwner {
			r := refuse(http.StatusUnprocessableEntity, CodeOwnerCannotBeRemoved,
				"the user owns this space; change its owner first")
			r.Extra = map[string]any{"spaceId": current[id].ShortID}
			return setChange{}, r
		}
		ch.removed = append(ch.removed, id)
		ch.spaces[id] = current[id]
	}
	for _, id := range sortedIDs(wanted) {
		if _, has := current[id]; has {
			continue
		}
		ch.added = append(ch.added, id)
		ch.spaces[id] = wanted[id]
	}
	return ch, nil
}

// PutUserSpaces answers PUT /users/{id}/spaces (the admin): the user's set
// of spaces becomes exactly shorts, in one transaction — every removed
// space a removal with its consequences (US-7), every added one the
// admin's direct addition (seats). Answers the saved set.
func (s *Service) PutUserSpaces(ctx context.Context, c Caller, userID uuid.UUID, shorts []string) (UserSpacesResponse, error) {
	if err := s.admin(ctx, c); err != nil {
		return UserSpacesResponse{}, err
	}
	err := WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// The admin right as it is in this transaction.
		if err := adminInTx(ctx, tx, c); err != nil {
			return err
		}
		ch, err := planSet(ctx, tx, c.UserID, userID, shorts)
		if err != nil {
			return err
		}
		for _, id := range ch.removed {
			if err := LeaveInTx(ctx, tx, id, userID, authz.RevokeRemovedFromSpace, true, c.Actor); err != nil {
				return err
			}
		}
		for _, id := range ch.added {
			if _, err := addMemberInTx(ctx, tx, id, userID, s.opts.MemberLimit, c.Actor); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return UserSpacesResponse{}, observeLimitRefusal(asRefusal(err))
	}
	items, _, err := userMemberships(ctx, s.pool, userID)
	if err != nil {
		return UserSpacesResponse{}, err
	}
	return UserSpacesResponse{Items: items}, nil
}

// PreviewUserSpaces answers POST /users/{id}/spaces/preview (the admin):
// what PUT with the same set would do, one entry per removed space — the
// numbers obtained by running the removals in a transaction that is rolled
// back (the file header of service.go), so they equal the fact.
func (s *Service) PreviewUserSpaces(ctx context.Context, c Caller, userID uuid.UUID, shorts []string) (UserSpacesPreview, error) {
	if err := s.admin(ctx, c); err != nil {
		return UserSpacesPreview{}, err
	}
	var out UserSpacesPreview
	err := WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		out = UserSpacesPreview{Removed: []UserSpaceRemoval{}}
		ch, err := planSet(ctx, tx, c.UserID, userID, shorts)
		if err != nil {
			return err
		}
		for _, id := range ch.removed {
			sp := ch.spaces[id]
			owned, err := ownedProjects(ctx, tx, id, userID)
			if err != nil {
				return err
			}
			before, err := tallyRemoval(ctx, tx, id, userID)
			if err != nil {
				return err
			}
			if err := LeaveInTx(ctx, tx, id, userID, authz.RevokeRemovedFromSpace, true, c.Actor); err != nil {
				return err
			}
			after, err := tallyRemoval(ctx, tx, id, userID)
			if err != nil {
				return err
			}
			out.Removed = append(out.Removed, UserSpaceRemoval{
				SpaceRef: SpaceRef{ID: sp.ShortID, Slug: Slug(sp.Name), Name: sp.Name},
				MemberRemovalPreview: MemberRemovalPreview{
					OpenAssignedIssues: before.assigned - after.assigned,
					ClaimedIssues:      before.claimed - after.claimed,
					OwnedProjects:      owned,
				},
			})
		}
		return errDryRun
	})
	if errors.Is(err, errDryRun) {
		slices.SortStableFunc(out.Removed, func(a, b UserSpaceRemoval) int {
			switch {
			case a.Name < b.Name:
				return -1
			case a.Name > b.Name:
				return 1
			}
			return 0
		})
		return out, nil
	}
	return UserSpacesPreview{}, asRefusal(err)
}

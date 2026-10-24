package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/authz"
	"skein-api/internal/person"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
)

// ErrMemberViaSpace is returned by RemoveMember for a member who keeps the
// project through the source "from space" (US-17, E-27) — code
// spaces.CodeMemberViaSpace in REST and MCP alike.
var ErrMemberViaSpace = errors.New("member through the space")

// ErrUserNotFound is returned by UpsertMember when the target user_id
// doesn't resolve to a row in users. We surface this as a business 422
// (separate from "project not visible" because the project IS visible
// to the caller — the user is the one missing).
var ErrUserNotFound = errors.New("user not found")

// ErrExternalCannotBeOwner — sk-163 S3 never-owner authz guard. Returned by
// UpsertMember when an external user (users.is_external=true) is requested as
// a project 'owner'. An external collaborator is capped at 'member' (spec §5).
// The app layer returns this BEFORE the INSERT; the cross-table trigger
// installed by migration 000034 (custom SQLSTATE 'SK001') is the DB backstop
// for direct-SQL / import paths, and its error is mapped back to this same
// sentinel below so both layers surface one business error.
var ErrExternalCannotBeOwner = errors.New("external user cannot be a project owner")

// ErrExternalCannotCreateProject — sk-163 S3 corollary of never-owner.
// CreateProject unconditionally bootstraps the creator as project_members
// role='owner', so an external user creating a project would necessarily
// become an owner — forbidden by spec §5. Returned by CreateProject when the
// caller is external (the PRIMARY app-gate, before the owner INSERT) and also
// as the mapped form of the 000034 trigger's SQLSTATE 'SK001' backstop, so the
// trigger can never escape as a raw 500. Surfaced by callers as REST 422 /
// MCP validation_error.
var ErrExternalCannotCreateProject = errors.New("external user cannot create a project")

// ErrPrefixTaken — sk-674.18 T025, US-15, E-17: the prefix is taken by a
// project of the installation (prefixes are global, D-4) or reserved by a
// deleted one. Answered with the neutral spaces.CodePrefixTaken and free
// alternatives, never with the holder or the constraint.
var ErrPrefixTaken = errors.New("project prefix taken")

// ErrNotSpaceMember — sk-674.18 INV-5, US-6: the action needs membership of
// the project's space and the caller (or the user named) is not a
// participant: creating a project there (spaces.CodeNotSpaceMember).
var ErrNotSpaceMember = errors.New("not a participant of the project's space")

// pgExternalOwnerViolation is the custom SQLSTATE raised by the
// skein_enforce_external_not_owner trigger (migration 000034) when a
// project_members owner row references an external user, or a user is flipped
// to external while owning a project. We pattern-match it and translate to
// ErrExternalCannotBeOwner so a backstop hit never leaks as a raw 500.
const pgExternalOwnerViolation = "SK001"

// isExternalOwnerViolation reports whether err is (or wraps) the trigger's
// custom SQLSTATE.
func isExternalOwnerViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgExternalOwnerViolation
}

// UpsertMember creates or updates a project_members row. Behaviour:
//   - new (project_id, user_id) pair → INSERT with the given role and the
//     source "manually" (adding a member is a project operation, INV-6);
//   - existing pair, the same role → an explicit addition of someone who
//     already holds a row (from the space): it sets "manually" (US-17 "add
//     a member to the project — sets the source manually"), so a later switch to
//     `members` keeps him;
//   - existing pair, another role → a role change: becoming owner sets
//     "manually" (US-17, E-28), becoming member leaves the sources as they
//     are.
//
// Both variants write the appropriate event (added vs role_changed). The
// addedBy actor is recorded as project_members.added_by. The transaction
// takes the lock of the project's space first and is retried (K-7,
// spaces.WithRetry): exhausted retries are spaces.ErrConcurrentChange.
func (s *Store) UpsertMember(
	ctx context.Context,
	projectID, userID uuid.UUID,
	role string,
	addedBy uuid.UUID,
	actor Actor,
) (*MemberDTO, error) {
	if role != "owner" && role != "member" {
		return nil, fmt.Errorf("invalid role %q", role)
	}

	var dto MemberDTO
	var eventType string
	var oldDTO *MemberDTO

	err := spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		if err := spaces.LockProjectSpace(ctx, tx, projectID); err != nil {
			return err
		}
		// Whether the user sees the project before the write: a row does not
		// make a gain by itself — an admin or the space's implicit owner
		// already sees it.
		sawBefore, err := seesProject(ctx, tx, userID, projectID)
		if err != nil {
			return err
		}
		// Detect existing membership for old/new diff.
		existing, err := s.fetchMemberInTx(ctx, tx, projectID, userID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}

		// Verify target user exists (without this we'd surface a confusing
		// FK error from the INSERT) and read is_external in the same query so
		// the never-owner guard below needs no extra round trip.
		var targetIsExternal bool
		if err := tx.QueryRow(ctx, `SELECT is_external FROM users WHERE id = $1`, userID).Scan(&targetIsExternal); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUserNotFound
			}
			return err
		}

		// sk-163 S3 never-owner: an external collaborator is capped at
		// 'member'. Reject the owner request with a clean business error
		// before the INSERT; the cross-table trigger (000034) is the backstop
		// for the TOCTOU race (a concurrent flip to external between this read
		// and our COMMIT) and for direct-SQL paths.
		if role == "owner" && targetIsExternal {
			return ErrExternalCannotBeOwner
		}

		// sk-674.18 INV-5, INV-8 (US-12): a member of a project of a
		// non-personal space is a participant of that space or an external
		// user (role member — capped above); a project of a "Personal" has no
		// member but its owner. Anyone else — the admin without membership
		// included — is refused with the answer of a nonexistent user, so
		// the refusal does not tell «no such user» from «not in the space».
		var personal, participant bool
		var spaceOwner uuid.UUID
		if err := tx.QueryRow(ctx, `
SELECT s.kind = 'personal', s.owner_id,
       EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = s.id AND sm.user_id = $2)
  FROM projects p JOIN spaces s ON s.id = p.space_id
 WHERE p.id = $1`, projectID, userID).Scan(&personal, &spaceOwner, &participant); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("reading the project's space: %w", err)
		}
		switch {
		case personal && userID != spaceOwner:
			return ErrUserNotFound
		case !personal && !participant && !targetIsExternal:
			return ErrUserNotFound
		}

		row := tx.QueryRow(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, $3, now(), $4, true, false)
ON CONFLICT (project_id, user_id) DO UPDATE
   SET role = EXCLUDED.role,
       via_manual = project_members.via_manual OR EXCLUDED.role = 'owner'
                    OR project_members.role = EXCLUDED.role
RETURNING project_id, user_id, role, added_at, added_by`,
			projectID, userID, role, addedBy)
		var pid, uid, abID uuid.UUID
		if err := row.Scan(&pid, &uid, &dto.Role, &dto.AddedAt, &abID); err != nil {
			return err
		}
		dto.UserID = uid
		dto.AddedBy = abID

		// Hydrate username/name for the response.
		if err := tx.QueryRow(ctx, `SELECT username, name FROM users WHERE id = $1`, userID).
			Scan(&dto.Username, &dto.Name); err != nil {
			return err
		}

		if existing == nil {
			eventType = EventProjectMemberAdded
			oldDTO = nil
		} else if existing.Role != dto.Role {
			eventType = EventProjectMemberRoleChanged
			oldDTO = existing
		} else {
			// No-op (same role) — write a member.added event anyway? No;
			// it's a true no-op. Don't write an event so callers don't see
			// duplicate noise on idempotent PUTs.
			return nil
		}
		var oldPayload any
		if oldDTO != nil {
			oldPayload = *oldDTO
		}
		if _, werr := WriteProjectEvent(ctx, tx, eventType, projectID, actor, oldPayload, dto); werr != nil {
			return werr
		}
		// sk-622.21 T017 (US-11): "Member added" (or their project
		// role changed).
		signalMembershipChanged(tx, userID)
		if !sawBefore {
			seesNow, err := seesProject(ctx, tx, userID, projectID)
			if err != nil {
				return err
			}
			if seesNow {
				signals.AddAccessChange(tx, userID, nil, []uuid.UUID{projectID}, nil)
			}
		}
		return nil
	})
	if err != nil {
		// Backstop: if the app-level guard above was bypassed (concurrent
		// flip / direct SQL) the trigger raises SQLSTATE 'SK001' — translate
		// it back to the same business sentinel so callers see one error.
		if isExternalOwnerViolation(err) {
			return nil, ErrExternalCannotBeOwner
		}
		return nil, err
	}
	return &dto, nil
}

// RemoveMember takes the user's membership of the project away (US-17
// "remove a member from the project"): it clears the source "manually". A member
// who keeps the project through the source "from space" (a `space`
// project) is refused with ErrMemberViaSpace and nothing changes (E-27) —
// his access is closed by switching the project to `members` or by leaving
// the space. The row is deleted, and if the user no longer sees the project
// he goes through INV-12 (authz.RevokeProjectAccess).
//
// The last explicit owner may be removed: the project stays under the
// implicit owner of its space (INV-4, US-17 — no «last owner» refusal);
// every project has a space since 000080 (T024). The transaction takes the
// lock of the project's space
// first and is retried (K-7): exhausted retries are
// spaces.ErrConcurrentChange.
func (s *Store) RemoveMember(
	ctx context.Context,
	projectID, userID uuid.UUID,
	actor Actor,
) error {
	return spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		if err := spaces.LockProjectSpace(ctx, tx, projectID); err != nil {
			return err
		}
		old, err := s.fetchMemberInTx(ctx, tx, projectID, userID)
		if err != nil {
			return err // ErrNotFound or db error
		}
		var viaSpace bool
		if err := tx.QueryRow(ctx, `
SELECT via_space FROM project_members WHERE project_id = $1 AND user_id = $2`,
			projectID, userID).Scan(&viaSpace); err != nil {
			return fmt.Errorf("reading the membership sources: %w", err)
		}
		if viaSpace {
			return ErrMemberViaSpace
		}

		if _, err := tx.Exec(ctx, `
DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`,
			projectID, userID); err != nil {
			return err
		}

		// INV-12 (sk-62.2 dropped only the reminders): read after the
		// DELETE, so a user who still sees the project (an admin's reach)
		// keeps everything.
		if _, err := authz.RevokeProjectAccess(ctx, tx, userID, []uuid.UUID{projectID},
			authz.RevokeRemovedFromProj, authz.RevokeActor{
				UserID: actor.UserID, Username: actor.Username, SessionID: actor.SessionID, Type: actor.Role,
			}); err != nil {
			return err
		}

		if _, werr := WriteProjectEvent(ctx, tx, EventProjectMemberRemoved, projectID, actor, *old, nil); werr != nil {
			return werr
		}
		// sk-622.21 T017 (US-11): "Member removed"; an orphaned external
		// deactivated below gets the same areas.
		signalMembershipChanged(tx, userID)

		// sk-159 — an external user must not exist without ≥1 project
		// membership. Runs AFTER the DELETE above so the remaining-count check
		// sees the post-removal state, in the SAME transaction so deactivation
		// + key revocation are atomic with the membership drop.
		return s.deactivateExternalIfOrphaned(ctx, tx, userID, actor)
	})
}

// deactivateExternalIfOrphaned enforces the sk-159 invariant: an external
// user (users.is_external=true) removed from their LAST project is
// deactivated (deactivated_at set) and every live API key revoked, all in
// the caller's transaction. It is a no-op for:
//   - internal / OIDC users (is_external=false) — they may legitimately hold
//     zero memberships;
//   - an already-deactivated external (idempotent — no second event);
//   - an external who still has at least one other membership;
//   - a user row that no longer exists.
//
// Mirrors users.Store.Deactivate's cascade
// (deactivated_at + apikeys.RevokeAllForUser + a user.deactivated event) but
// is triggered by membership loss rather than an explicit admin action.
func (s *Store) deactivateExternalIfOrphaned(ctx context.Context, tx pgx.Tx, userID uuid.UUID, actor Actor) error {
	var (
		isExternal         bool
		alreadyDeactivated bool
	)
	err := tx.QueryRow(ctx,
		`SELECT is_external, deactivated_at IS NOT NULL FROM users WHERE id = $1`,
		userID).Scan(&isExternal, &alreadyDeactivated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // user already gone — nothing to deactivate
	}
	if err != nil {
		return fmt.Errorf("reading user for external-orphan check %s: %w", userID, err)
	}
	if !isExternal || alreadyDeactivated {
		return nil
	}

	var remaining int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM project_members WHERE user_id = $1`,
		userID).Scan(&remaining); err != nil {
		return fmt.Errorf("counting remaining memberships for %s: %w", userID, err)
	}
	if remaining > 0 {
		return nil // still a member elsewhere — keep the account active
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`UPDATE users SET deactivated_at = $2 WHERE id = $1`, userID, now); err != nil {
		return fmt.Errorf("deactivating orphaned external %s: %w", userID, err)
	}

	// Cascade — soft-revoke every live key in the same tx (reuses the same
	// helper users.Store.Deactivate uses, so the two deactivation paths stay
	// consistent).
	revoked, err := s.apikeys.RevokeAllForUser(ctx, tx, userID)
	if err != nil {
		return err
	}

	// Audit event mirrors users.EventUserDeactivated ("user.deactivated") so
	// the user's timeline records why the account went inactive. Written via
	// the same-package generic writer (entity_type='user', project_id=NULL).
	_, err = writeEvent(ctx, tx, "user.deactivated", "user", userID.String(), nil, actor,
		nil,
		struct {
			DeactivatedAt      time.Time `json:"deactivatedAt"`
			RevokedAPIKeyCount int64     `json:"revokedApiKeyCount"`
			Reason             string    `json:"reason"`
		}{now, revoked, "external user removed from last project"},
	)
	return err
}

// MembershipDTO is one row returned by ListMembershipsForUser. Mirrors the
// caller's perspective: "which projects am I a member of, and in what
// role". Used by skein-web's SignalR hub at connect time to compute the
// per-connection event filter.
//
// Role is one of:
//   - "owner" — explicit owner row in project_members
//   - "member" — explicit member row in project_members
//   - "admin" — synthetic row for the relation's admin level (one per
//     project the caller sees at that level, NOT a real membership row)
type MembershipDTO struct {
	ProjectID uuid.UUID `json:"projectId"`
	Role      string    `json:"role"`
	AddedAt   time.Time `json:"addedAt"`
}

// listMembershipsCap is the hard ceiling on rows returned by
// ListMembershipsForUser. R3.2c assumes 5-50 memberships for a typical
// user; an admin in a large deployment could approach this cap. R5 will
// revisit (paginate or compute server-side filter expressions) when
// projects.count grows past 1k.
const listMembershipsCap = 1000

// ListMembershipsForUser returns the projects the caller sees, with the
// level, for /me/memberships — the access relation's rows (authz,
// sk-674.18 T009).
//
// A level backed by a membership row carries that row's added_at; a level
// without one — the instance admin's reach ("admin"), the implicit owner of
// the project's space or of a "Personal" ("owner") — is dated by the
// project's created_at. An admin's own member row reads as "member". Archived projects are included; the
// hub doesn't need to filter, the BFF/UI layer does. Newest first.
//
// Capped at listMembershipsCap rows to keep the response bounded.
func (s *Store) ListMembershipsForUser(ctx context.Context, userID uuid.UUID) ([]MembershipDTO, error) {
	rows, err := s.pool.Query(ctx, `
SELECT va.project_id,
       CASE va.access WHEN 'admin_member' THEN 'member' ELSE va.access END,
       COALESCE(pm.added_at, p.created_at) AS added_at
  FROM skein_project_access_set($1::uuid) va
  JOIN projects p ON p.id = va.project_id
  LEFT JOIN project_members pm ON pm.project_id = va.project_id AND pm.user_id = $1
 ORDER BY 3 DESC
 LIMIT $2`, userID, listMembershipsCap)
	if err != nil {
		return nil, fmt.Errorf("listing memberships: %w", err)
	}
	defer rows.Close()

	out := make([]MembershipDTO, 0)
	for rows.Next() {
		var d MembershipDTO
		if err := rows.Scan(&d.ProjectID, &d.Role, &d.AddedAt); err != nil {
			return nil, fmt.Errorf("scanning membership row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListMembers joins project_members with users so the UI gets ready-to-
// render rows (username + name). Sorted by role first (owners on top),
// then by added_at.
func (s *Store) ListMembers(ctx context.Context, projectID uuid.UUID) ([]MemberDTO, error) {
	rows, err := s.pool.Query(ctx, `
SELECT pm.user_id, u.username, u.name, pm.role, pm.added_at, pm.added_by, `+person.StatusExpr("u")+`
  FROM project_members pm
  JOIN users u ON u.id = pm.user_id
 WHERE pm.project_id = $1
 ORDER BY CASE pm.role WHEN 'owner' THEN 0 ELSE 1 END, pm.added_at`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	defer rows.Close()

	out := make([]MemberDTO, 0)
	for rows.Next() {
		var d MemberDTO
		var status *person.Status
		if err := rows.Scan(&d.UserID, &d.Username, &d.Name, &d.Role, &d.AddedAt, &d.AddedBy, &status); err != nil {
			return nil, fmt.Errorf("scanning member row: %w", err)
		}
		if status != nil {
			d.Status = *status
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// fetchMemberInTx loads a single member row in a transaction, used by
// UpsertMember and RemoveMember to compute old/new diffs.
func (s *Store) fetchMemberInTx(ctx context.Context, tx pgx.Tx, projectID, userID uuid.UUID) (*MemberDTO, error) {
	var d MemberDTO
	row := tx.QueryRow(ctx, `
SELECT pm.user_id, u.username, u.name, pm.role, pm.added_at, pm.added_by
  FROM project_members pm
  JOIN users u ON u.id = pm.user_id
 WHERE pm.project_id = $1 AND pm.user_id = $2`, projectID, userID)
	err := row.Scan(&d.UserID, &d.Username, &d.Name, &d.Role, &d.AddedAt, &d.AddedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// seesProject reports whether user sees project by the relation (the
// boolean point form skein_project_visible — index probes, a narrow SSI
// footprint; INV-1): for access_changed, a gain is a change of visibility, not a
// membership row written.
func seesProject(ctx context.Context, tx pgx.Tx, userID, projectID uuid.UUID) (bool, error) {
	var sees bool
	if err := tx.QueryRow(ctx, `SELECT skein_project_visible($1, $2)`, userID, projectID).Scan(&sees); err != nil {
		return false, fmt.Errorf("reading whether %s sees project %s: %w", userID, projectID, err)
	}
	return sees, nil
}

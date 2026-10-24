package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/assignee"
	"skein-api/internal/eventbus"
	"skein-api/internal/signals"
)

// Loss of access to a project (INV-12, sk-674.18 T018).
//
// When a user stops seeing a project — whatever the cause: leaving or being
// removed from its space, account deletion, removal from the project, the
// project switched space → members, a change of the space's owner, an admin
// demoted — the same transaction must leave behind:
//
//   - no issue of that project in a status other than closed with the user
//     as its assignee (the assignee is removed, with an event that names the
//     reason; a closed issue keeps its assignee);
//   - no claim on an issue of the project held by the user or the user's
//     agents (claims are per user: locked_by_user_id);
//   - no watch of an issue of the project nor of the project itself;
//   - no reminder on an issue of the project.
//
// Deactivation is NOT a loss of access (plan 8): it never calls this.
//
// The decision "lost" is the access relation's, read inside the caller's
// transaction AFTER the change that may have caused it: only candidates the
// relation no longer gives the user are revoked, so a user who still sees a
// project through another path (an admin's reach, a manual row on a `space`
// project) keeps everything. The function writes the history events, adds
// the personal signal and the access_changed event to the transaction's
// collector (signals.BeginTxFunc), and returns the projects that were lost.

// RevokeActor is who caused the loss, as the history events name them.
type RevokeActor struct {
	UserID    uuid.UUID
	Username  string
	SessionID string
	// Type is the events.actor_type: "human", "agent" or "system".
	Type string
}

// Reasons of a loss of access, written into the events (events.reason).
const (
	RevokeLeftSpace        = "left_space"
	RevokeRemovedFromSpace = "removed_from_space"
	RevokeRemovedFromProj  = "removed_from_project"
	RevokeVisibilityToMem  = "visibility_members"
	RevokeOwnerChanged     = "space_owner_changed"
	RevokeAccountDeleted   = "account_deleted"
	RevokeAdminDemoted     = "admin_demoted"
	RevokeSpaceDeleted     = "space_deleted"
)

// RevokeProjectAccess applies INV-12 for userID to every project of
// candidates the user no longer sees, inside tx (opened by
// signals.BeginTxFunc), and returns those projects. reason is one of the
// Revoke* constants.
func RevokeProjectAccess(ctx context.Context, tx pgx.Tx, userID uuid.UUID, candidates []uuid.UUID, reason string, actor RevokeActor) ([]uuid.UUID, error) {
	return revokeProjectAccess(ctx, tx, userID, candidates, nil, true, reason, actor)
}

// RevokeProjectAccessSeen is RevokeProjectAccess for a loss whose cleanup
// and whose publication differ: INV-12 is applied to
// every candidate the user does not see now, but access_changed names as
// lost only those of seenBefore — the projects the user actually saw. The
// case is the removal of a deactivated participant from a space: he sees
// nothing (INV-1 — active users only), yet his assignments, claims, watches
// and reminders in the space's projects, kept by the deactivation (E-15),
// must go with the memberships (US-7, SC-010).
func RevokeProjectAccessSeen(ctx context.Context, tx pgx.Tx, userID uuid.UUID, candidates, seenBefore []uuid.UUID, reason string, actor RevokeActor) ([]uuid.UUID, error) {
	return revokeProjectAccess(ctx, tx, userID, candidates, seenBefore, false, reason, actor)
}

func revokeProjectAccess(ctx context.Context, tx pgx.Tx, userID uuid.UUID, candidates, seenBefore []uuid.UUID, publishAll bool, reason string, actor RevokeActor) ([]uuid.UUID, error) {
	if userID == uuid.Nil || len(candidates) == 0 {
		return nil, nil
	}
	lost, err := collectIDs(ctx, tx, `
SELECT c.id FROM unnest($1::uuid[]) AS c(id)
 WHERE EXISTS (SELECT 1 FROM projects p WHERE p.id = c.id)
   AND NOT EXISTS (SELECT 1 FROM skein_project_access_set($2::uuid) va WHERE va.project_id = c.id)`,
		candidates, userID)
	if err != nil {
		return nil, fmt.Errorf("revoke: deciding lost projects: %w", err)
	}
	if len(lost) == 0 {
		return nil, nil
	}

	if err := unassignLost(ctx, tx, userID, lost, reason, actor); err != nil {
		return nil, err
	}
	if err := releaseLostClaims(ctx, tx, userID, lost, reason, actor); err != nil {
		return nil, err
	}
	for _, q := range []string{
		`DELETE FROM issue_watchers w USING issues i
		  WHERE w.issue_id = i.id AND w.user_id = $1 AND i.project_id = ANY($2::uuid[])`,
		`DELETE FROM project_watchers WHERE user_id = $1 AND project_id = ANY($2::uuid[])`,
		`DELETE FROM reminders r USING issues i
		  WHERE r.issue_id = i.id AND r.user_id = $1 AND i.project_id = ANY($2::uuid[])`,
	} {
		if _, err := tx.Exec(ctx, q, userID, lost); err != nil {
			return nil, fmt.Errorf("revoke: dropping watches and reminders: %w", err)
		}
	}

	signals.Add(tx, userID, signals.AreaProjects, signals.AreaLens, signals.AreaTasks,
		signals.AreaWatching, signals.AreaInbox)
	published := lost
	if !publishAll {
		published = nil
		for _, id := range lost {
			if slices.Contains(seenBefore, id) {
				published = append(published, id)
			}
		}
	}
	signals.AddAccessChange(tx, userID, published, nil, nil)
	return lost, nil
}

// unassignLost removes the user as assignee of every non-closed issue of the
// lost projects, with the issue.updated / issue.field_changed events every
// assignee change writes (field "assignee", reason = the loss's reason). The
// user joins previous_assignees, as on any change of assignee (sk-34), and
// the acceptance goes with the assignment.
func unassignLost(ctx context.Context, tx pgx.Tx, userID uuid.UUID, lost []uuid.UUID, reason string, actor RevokeActor) error {
	rows, err := tx.Query(ctx, `
WITH old AS (
    SELECT i.id, i.project_id, i.assignee
      FROM issues i
     WHERE i.project_id = ANY($1::uuid[])
       AND i.status <> 'closed'
       AND i.assignee IS NOT NULL
       AND `+assignee.NamesUser("i", "$2::uuid")+`
       FOR UPDATE
), upd AS (
    UPDATE issues i
       SET assignee = NULL,
           accepted_at = NULL,
           accepted_by = NULL,
           previous_assignees = CASE WHEN $2::uuid = ANY(i.previous_assignees)
                                     THEN i.previous_assignees
                                     ELSE array_append(i.previous_assignees, $2::uuid) END,
           version = i.version + 1,
           updated_at = now()
      FROM old
     WHERE i.id = old.id
    RETURNING i.id
)
SELECT old.id, old.project_id, old.assignee FROM old JOIN upd ON upd.id = old.id
 ORDER BY old.id`, lost, userID)
	if err != nil {
		return fmt.Errorf("revoke: removing the assignee: %w", err)
	}
	type unassigned struct {
		id      string
		project uuid.UUID
		was     string
	}
	var done []unassigned
	for rows.Next() {
		var u unassigned
		if err := rows.Scan(&u.id, &u.project, &u.was); err != nil {
			rows.Close()
			return fmt.Errorf("revoke: scanning unassigned issue: %w", err)
		}
		done = append(done, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("revoke: reading unassigned issues: %w", err)
	}
	ea := eventbus.Actor{UserID: actor.UserID, Username: actor.Username, Role: actor.Type, SessionID: actor.SessionID}
	for _, u := range done {
		project := u.project
		if _, _, err := eventbus.WriteHybridUpdateEvents(ctx, tx,
			"issue.updated", "issue.field_changed", "issue", u.id, &project, ea,
			[]eventbus.FieldChange{{Field: "assignee", OldValue: u.was, NewValue: nil}},
			reason); err != nil {
			return fmt.Errorf("revoke: assignee event of %s: %w", u.id, err)
		}
	}
	return nil
}

// releaseLostClaims releases every claim of the user (and so of the user's
// agents) on an issue of the lost projects, with the issue.unclaimed event a
// release writes (field lockedBy). The status is left as it is — the same
// release the stale-lock reaper does.
func releaseLostClaims(ctx context.Context, tx pgx.Tx, userID uuid.UUID, lost []uuid.UUID, reason string, actor RevokeActor) error {
	rows, err := tx.Query(ctx, `
WITH old AS (
    SELECT i.id, i.project_id, i.locked_by, i.locked_at
      FROM issues i
     WHERE i.project_id = ANY($1::uuid[])
       AND i.locked_by_user_id = $2::uuid
       FOR UPDATE
), upd AS (
    UPDATE issues i
       SET locked_by = NULL, locked_at = NULL, locked_by_user_id = NULL,
           version = i.version + 1, updated_at = now()
      FROM old
     WHERE i.id = old.id
    RETURNING i.id
)
SELECT old.id, old.project_id, old.locked_by, old.locked_at FROM old JOIN upd ON upd.id = old.id
 ORDER BY old.id`, lost, userID)
	if err != nil {
		return fmt.Errorf("revoke: releasing claims: %w", err)
	}
	type released struct {
		id       string
		project  uuid.UUID
		lockedBy *string
		lockedAt *time.Time
	}
	var done []released
	for rows.Next() {
		var r released
		if err := rows.Scan(&r.id, &r.project, &r.lockedBy, &r.lockedAt); err != nil {
			rows.Close()
			return fmt.Errorf("revoke: scanning released claim: %w", err)
		}
		done = append(done, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("revoke: reading released claims: %w", err)
	}
	for _, r := range done {
		oldData, err := json.Marshal(map[string]any{"lockedBy": r.lockedBy, "lockedAt": r.lockedAt})
		if err != nil {
			return err
		}
		var userArg any
		if actor.UserID != uuid.Nil {
			userArg = actor.UserID
		}
		var sessionArg any
		if actor.SessionID != "" {
			sessionArg = actor.SessionID
		}
		actorType := actor.Type
		if actorType == "" {
			actorType = "system"
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_session_id, actor_type,
                    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload)
VALUES ('issue.unclaimed', $1, $2, $3, $4, 'issue', $5, $6, 'lockedBy', $7::jsonb,
        '{"lockedBy":null,"lockedAt":null}'::jsonb, $8, '{}'::jsonb)`,
			actor.Username, userArg, sessionArg, actorType, r.id, r.project, string(oldData), reason); err != nil {
			return fmt.Errorf("revoke: unclaim event of %s: %w", r.id, err)
		}
	}
	return nil
}

func collectIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

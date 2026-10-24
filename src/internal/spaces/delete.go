package spaces

// delete.go — deleting a space (sk-674.18 T019, US-4, K-22, INV-25, E-9).
//
// One transaction, under the space's lock, deletes the projects of the
// space (archived ones included) and everything that belongs to them and
// their issues, in the order of their dependencies and by explicit DELETEs
// (K-22: the cascade is defined here, not inherited from foreign keys):
//
//	notifications → issue and project watches, watch opt-outs → reminders →
//	monitor credentials → dependencies → commits → attachments (rows) →
//	comments → events → labels, components of issues, child counters →
//	issues → components → aliases → project members → project counters →
//	projects (their prefixes reserved) → invitations (revoked, their tokens
//	superseded, their unsent emails cancelled) → participants → the storage
//	counter → the space.
//
// The events of the deleted projects go with them, published or not: the
// outbox dispatcher reads unpublished rows from the table, so after the
// commit nothing of these projects reaches the hub or the agents. The
// objects of the attachments are removed later by the orphan scan
// (attachments/cleanup.go#reconcileOrphans); the search index lives in the
// deleted rows. The invitations stay without a space — the pending ones
// revoked, the finished ones (accepted, declined, expired, revoked) with
// their outcome — and the link of any of them answers «the space was
// deleted» (000077, 000079).
//
// After the commit — and only then — every participant gets the personal
// signal `spaces` with the event `deleted` and access_changed with the
// projects and the space lost; a rolled back deletion publishes nothing.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/signals"
	"skein-api/internal/tokens"
)

// Delete deletes the space with everything in it (see the file header).
// The "Personal" goes only with its account (ErrPersonalSpace here; the
// account deletion calls DeleteInTx). Who may delete — and whether the
// common space may go — is the caller's check (T023).
func (s *Store) Delete(ctx context.Context, spaceID uuid.UUID, actor Actor) error {
	return WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM spaces WHERE id = $1`, spaceID).Scan(&kind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSpaceNotFound
			}
			return fmt.Errorf("reading space %s: %w", spaceID, err)
		}
		if kind == "personal" {
			return ErrPersonalSpace
		}
		return DeleteInTx(ctx, tx, spaceID, actor)
	})
}

// DeleteInTx is the cascade of Delete inside the caller's transaction
// (opened by signals.BeginTxFunc), whatever the kind of the space.
func DeleteInTx(ctx context.Context, tx pgx.Tx, spaceID uuid.UUID, actor Actor) error {
	sp, err := LockSpace(ctx, tx, spaceID)
	if err != nil {
		return err
	}
	projects, err := collect(ctx, tx, `SELECT id FROM projects WHERE space_id = $1 FOR UPDATE`, spaceID)
	if err != nil {
		return fmt.Errorf("reading the space's projects: %w", err)
	}

	// Who loses what, read before anything is deleted: the participants,
	// the external members of the projects and — for a space that is not a
	// "Personal" — the instance admins who see it by adminAccess (INV-1 g,
	// US-6; US-19 drops their hub groups at once too), each
	// with the projects of the space the relation gives them now.
	affected, err := collect(ctx, tx, `
SELECT user_id FROM space_members WHERE space_id = $1
UNION
SELECT pm.user_id FROM project_members pm WHERE pm.project_id = ANY($2::uuid[])
UNION
SELECT u.id FROM users u, instance_settings st
 WHERE $3 AND st.id = 1 AND st.admin_enabled
   AND u.role = 'admin' AND NOT u.is_external AND u.deactivated_at IS NULL AND u.deleted_at IS NULL`,
		spaceID, projects, sp.Kind != "personal")
	if err != nil {
		return fmt.Errorf("reading who loses the space: %w", err)
	}
	// Who sees the space itself and gets the `deleted` event and the space
	// lost: its participants and, for a non-personal space, the admins above.
	participants, err := collect(ctx, tx, `
SELECT user_id FROM space_members WHERE space_id = $1
UNION
SELECT u.id FROM users u, instance_settings st
 WHERE $2 AND st.id = 1 AND st.admin_enabled
   AND u.role = 'admin' AND NOT u.is_external AND u.deactivated_at IS NULL AND u.deleted_at IS NULL`,
		spaceID, sp.Kind != "personal")
	if err != nil {
		return fmt.Errorf("reading who sees the space: %w", err)
	}
	lost := make(map[uuid.UUID][]uuid.UUID, len(affected))
	for _, u := range affected {
		seen, err := visibleIn(ctx, tx, u, spaceID)
		if err != nil {
			return err
		}
		lost[u] = seen
	}

	if len(projects) > 0 {
		if err := deleteProjects(ctx, tx, projects); err != nil {
			return err
		}
	}

	invitations, err := collect(ctx, tx, `
UPDATE space_invitations
   SET status = 'revoked', resolved_at = now(), updated_at = now()
 WHERE space_id = $1 AND status = 'pending'
RETURNING id`, spaceID)
	if err != nil {
		return fmt.Errorf("revoking the invitations: %w", err)
	}
	for _, inv := range invitations {
		if err := tokens.Cancel(ctx, tx, tokens.InvitationSubject(inv)); err != nil {
			return fmt.Errorf("cancelling invitation %s: %w", inv, err)
		}
	}

	for _, q := range []string{
		`DELETE FROM space_members WHERE space_id = $1`,
		`DELETE FROM space_storage_usage WHERE space_id = $1`,
		`DELETE FROM spaces WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, spaceID); err != nil {
			return fmt.Errorf("deleting the space: %w", err)
		}
	}
	if err := writeSpaceEvent(ctx, tx, sp, actor, "deleted", uuid.Nil, projects, ""); err != nil {
		return err
	}

	isParticipant := make(map[uuid.UUID]bool, len(participants))
	for _, u := range participants {
		isParticipant[u] = true
	}
	for _, u := range affected {
		var lostSpaces []string
		if isParticipant[u] {
			lostSpaces = []string{sp.ShortID}
			signals.AddSpaceEvent(tx, u, signals.SpaceEvent{Reason: signals.SpaceDeleted, SpaceID: sp.ShortID, SpaceName: sp.Name})
		}
		signals.Add(tx, u, signals.AreaProjects, signals.AreaLens, signals.AreaTasks,
			signals.AreaInbox, signals.AreaWatching, signals.AreaMentions)
		signals.AddAccessChange(tx, u, lost[u], nil, lostSpaces)
	}
	return nil
}

// deleteProjects is the project half of the cascade (K-22 order). The
// projects' rows are locked by the caller.
func deleteProjects(ctx context.Context, tx pgx.Tx, projects []uuid.UUID) error {
	const inIssues = `(SELECT id FROM issues WHERE project_id = ANY($1::uuid[]))`
	steps := []struct{ what, sql string }{
		{"notifications", `DELETE FROM notifications WHERE project_id = ANY($1::uuid[])`},
		{"issue watches", `DELETE FROM issue_watchers WHERE issue_id IN ` + inIssues},
		{"watch opt-outs", `DELETE FROM issue_watch_optouts WHERE issue_id IN ` + inIssues},
		{"project watches", `DELETE FROM project_watchers WHERE project_id = ANY($1::uuid[])`},
		{"reminders", `DELETE FROM reminders WHERE issue_id IN ` + inIssues},
		{"monitor credentials", `DELETE FROM monitor_credentials WHERE project_id = ANY($1::uuid[])`},
		{"dependencies", `DELETE FROM dependencies WHERE from_id IN ` + inIssues + ` OR to_id IN ` + inIssues},
		{"commits", `DELETE FROM issue_commits WHERE issue_id IN ` + inIssues},
		{"attachments", `DELETE FROM attachments WHERE issue_id IN ` + inIssues},
		{"comments", `DELETE FROM comments WHERE issue_id IN ` + inIssues},
		{"events", `DELETE FROM events WHERE project_id = ANY($1::uuid[])`},
		{"issue labels", `DELETE FROM issue_labels WHERE issue_id IN ` + inIssues},
		{"issue components", `DELETE FROM issue_components WHERE issue_id IN ` + inIssues},
		{"child counters", `DELETE FROM child_counters WHERE parent_id IN ` + inIssues},
		// issues.parent_id is RESTRICT: a child elsewhere (a cross-project
		// tail older than INV-24) loses its parent, and the issues of the
		// set are detached from each other before they go in one statement.
		{"detached children", `UPDATE issues SET parent_id = NULL
		    WHERE parent_id IN ` + inIssues + ` AND NOT (project_id = ANY($1::uuid[]))`},
		{"detached parents", `UPDATE issues SET parent_id = NULL WHERE project_id = ANY($1::uuid[]) AND parent_id IS NOT NULL`},
		{"issues", `DELETE FROM issues WHERE project_id = ANY($1::uuid[])`},
		{"components", `DELETE FROM components WHERE project_id = ANY($1::uuid[])`},
		{"aliases", `DELETE FROM project_aliases WHERE project_id = ANY($1::uuid[])`},
		{"project members", `DELETE FROM project_members WHERE project_id = ANY($1::uuid[])`},
		{"project counters", `DELETE FROM project_counters WHERE project_id = ANY($1::uuid[])`},
		// US-15: the prefixes of deleted projects stay reserved.
		{"reserved prefixes", `INSERT INTO reserved_prefixes (prefix)
		    SELECT lower(id_prefix) FROM projects WHERE id = ANY($1::uuid[])
		    ON CONFLICT (prefix) DO NOTHING`},
		{"projects", `DELETE FROM projects WHERE id = ANY($1::uuid[])`},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, st.sql, projects); err != nil {
			return fmt.Errorf("deleting the space's %s: %w", st.what, err)
		}
	}
	return nil
}

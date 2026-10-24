package projects

// personal_signal.go — sk-622.21 T017 (US-11, K13): the personal signal of
// membership and project lifecycle changes. Named inside the write's
// transaction (opened by signals.BeginTxFunc) and published after it commits.
// Connected internal admins get `projects` on lifecycle events from skein-web
// (T034), not from here.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/signals"
)

// signalMembershipChanged — "Member added / removed in a project": the
// member gets projects, lens, unreviewed and tasks — their project list and
// every personal list scoped by visibility change.
func signalMembershipChanged(tx pgx.Tx, userID uuid.UUID) {
	signals.Add(tx, userID, signals.AreaProjects, signals.AreaLens, signals.AreaUnreviewed, signals.AreaTasks)
}

// signalProjectMembers gives every member of projectID the areas — "Project
// created / changed / archived / unarchived". One statement, whatever
// the number of members.
func signalProjectMembers(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, areas ...signals.Area) error {
	rows, err := tx.Query(ctx, `SELECT user_id FROM project_members WHERE project_id = $1`, projectID)
	if err != nil {
		return fmt.Errorf("reading members of %s for the personal signal: %w", projectID, err)
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("reading members of %s for the personal signal: %w", projectID, err)
	}
	for _, uid := range members {
		signals.Add(tx, uid, areas...)
	}
	return nil
}

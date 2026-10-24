package authz_test

// revoke_test.go — sk-674.18 T018, matrix row 44 (INV-12): what
// RevokeProjectAccess leaves behind for a user who lost a project, written
// from the invariant (spec "Key invariants", INV-12), not from the code:
//
//   - the user is assignee of no issue of the project in a status other than
//     closed, each removal with an event (field assignee) that names the
//     reason; a closed issue keeps its assignee;
//   - no claim of the user or of the user's agents (one user id, any agent
//     session) on an issue of the project;
//   - no watch of an issue of the project, nor of the project; no reminder;
//   - the issues of another project of the same user are not touched, and a
//     project the user still sees is not revoked at all.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
)

type revokeProject struct {
	id                         uuid.UUID
	open, closed, locked, w, r string
}

// seedRevokeProject seeds a project owned by owner with user as a member and,
// for user: an open issue assigned to him, a closed one assigned to him, an
// issue claimed by one of his agents (also assigned), a watch on an issue and
// on the project, and a reminder.
func seedRevokeProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, user uuid.UUID, username string) revokeProject {
	t.Helper()
	ctx := context.Background()
	pid, prefix := testsupport.SeedProject(t, pool, owner, "rvk")
	testsupport.AddProjectMember(t, pool, pid, user, "member")
	p := revokeProject{id: pid, open: prefix + "-1", closed: prefix + "-2", locked: prefix + "-3", w: prefix + "-4", r: prefix + "-5"}
	testsupport.SeedIssue(t, pool, pid, owner, p.open, "rvk open", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, p.closed, "rvk closed", "closed", 2)
	testsupport.SeedIssue(t, pool, pid, owner, p.locked, "rvk locked", "in_progress", 2)
	testsupport.SeedIssue(t, pool, pid, owner, p.w, "rvk watched", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, p.r, "rvk reminded", "open", 2)
	for _, id := range []string{p.open, p.closed, p.locked} {
		testsupport.SetAssignee(t, pool, id, username)
	}
	if _, err := pool.Exec(ctx, `
UPDATE issues SET locked_by = 'agent-session-of-user', locked_by_user_id = $2, locked_at = now() WHERE id = $1`,
		p.locked, user); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')`, p.w, user); err != nil {
		t.Fatalf("watch issue: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, pid, user); err != nil {
		t.Fatalf("watch project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_watchers WHERE project_id = $1`, pid)
	})
	testsupport.SeedReminder(t, pool, p.r, user)
	return p
}

type revokeState struct {
	openAssigned, closedAssigned, lockedHeld bool
	issueWatches, projectWatches, reminders  int
}

func readRevokeState(t *testing.T, pool *pgxpool.Pool, p revokeProject, user uuid.UUID) revokeState {
	t.Helper()
	var s revokeState
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT assignee IS NOT NULL FROM issues WHERE id = $1),
       (SELECT assignee IS NOT NULL FROM issues WHERE id = $2),
       (SELECT locked_by_user_id IS NOT NULL FROM issues WHERE id = $3),
       (SELECT count(*) FROM issue_watchers w JOIN issues i ON i.id = w.issue_id WHERE i.project_id = $4 AND w.user_id = $5),
       (SELECT count(*) FROM project_watchers WHERE project_id = $4 AND user_id = $5),
       (SELECT count(*) FROM reminders r JOIN issues i ON i.id = r.issue_id WHERE i.project_id = $4 AND r.user_id = $5)`,
		p.open, p.closed, p.locked, p.id, user).Scan(&s.openAssigned, &s.closedAssigned, &s.lockedHeld,
		&s.issueWatches, &s.projectWatches, &s.reminders); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return s
}

func TestRevokeProjectAccess_INV12(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	actor, actorName := testsupport.SeedUser(t, pool, "")
	user, username := testsupport.SeedUser(t, pool, "")

	lost := seedRevokeProject(t, pool, owner, user, username)
	kept := seedRevokeProject(t, pool, owner, user, username)
	stillSeen := seedRevokeProject(t, pool, owner, user, username)
	wantKept := readRevokeState(t, pool, kept, user)
	wantSeen := readRevokeState(t, pool, stillSeen, user)

	// The user loses `lost`: his membership row goes, as removal from the
	// project does, then the revocation runs in the same transaction.
	var got []uuid.UUID
	err := signals.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, lost.id, user); err != nil {
			return err
		}
		var err error
		got, err = authz.RevokeProjectAccess(ctx, tx, user, []uuid.UUID{lost.id, stillSeen.id},
			authz.RevokeRemovedFromProj, authz.RevokeActor{UserID: actor, Username: actorName, Type: "human"})
		return err
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(got) != 1 || got[0] != lost.id {
		t.Fatalf("lost = %v, want only %s (the still-seen candidate is not revoked)", got, lost.id)
	}

	s := readRevokeState(t, pool, lost, user)
	if s.openAssigned {
		t.Error("the open issue kept the user as assignee")
	}
	if !s.closedAssigned {
		t.Error("the closed issue lost its assignee — a closed issue keeps it")
	}
	if s.lockedHeld {
		t.Error("the claim of the user's agent survived")
	}
	if s.issueWatches != 0 || s.projectWatches != 0 || s.reminders != 0 {
		t.Errorf("issue watches %d, project watches %d, reminders %d — want none", s.issueWatches, s.projectWatches, s.reminders)
	}

	// The removals are history: an assignee event naming the reason for
	// each unassigned issue, an unclaim event for the released claim.
	for _, c := range []struct {
		typ, field, issue string
	}{
		{"issue.field_changed", "assignee", lost.open},
		{"issue.field_changed", "assignee", lost.locked},
		{"issue.unclaimed", "lockedBy", lost.locked},
	} {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE type = $1 AND field = $2 AND entity_id = $3 AND reason = $4 AND actor_user_id = $5`,
			c.typ, c.field, c.issue, authz.RevokeRemovedFromProj, actor).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s/%s events on %s with the reason = %d, want 1", c.typ, c.field, c.issue, n)
		}
	}
	var closedEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE entity_id = $1 AND field = 'assignee'`, lost.closed).Scan(&closedEvents); err != nil {
		t.Fatal(err)
	}
	if closedEvents != 0 {
		t.Errorf("the closed issue got %d assignee events", closedEvents)
	}

	// Another project of the same user, and the one he still sees: untouched.
	if gotKept := readRevokeState(t, pool, kept, user); gotKept != wantKept {
		t.Errorf("another project was touched: %+v, want %+v", gotKept, wantKept)
	}
	if gotSeen := readRevokeState(t, pool, stillSeen, user); gotSeen != wantSeen {
		t.Errorf("a project the user still sees was revoked: %+v, want %+v", gotSeen, wantSeen)
	}
}

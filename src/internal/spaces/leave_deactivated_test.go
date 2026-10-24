package spaces_test

// leave_deactivated_test.go — US-7, SC-010,
// INV-12, E-15. Reference — the spec, written before the primitive:
// deactivation is not a loss of access (assignments, watching, reminders
// and memberships stay — E-15, row 46); removal from a space IS one: "all
// of his project membership rows in the projects of S are removed in one transaction … and
// the access-loss rule INV-12 is applied to all projects of S". A
// deactivated user sees nothing (INV-1), so «what he saw before» is empty —
// the cleanup must follow the memberships removed, not the visibility; the
// lost projects published in access_changed stay the visible ones (none).

import (
	"context"
	"testing"

	"skein-api/internal/testsupport"
)

func TestLeave_DeactivatedParticipantIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "members")
	w.setRow(t, "manual")

	// u's state in S: the open and the closed assigned issue (newWorld), a
	// claim of the open one, watching of the issue and the project, a
	// reminder.
	if _, err := w.pool.Exec(ctx, `
UPDATE issues SET locked_by = 'sess-deact', locked_at = now(), locked_by_user_id = $2 WHERE id = $1`,
		w.assignedIssue, w.u); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')`,
		w.assignedIssue, w.u); err != nil {
		t.Fatalf("seed issue watcher: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, w.p, w.u); err != nil {
		t.Fatalf("seed project watcher: %v", err)
	}
	testsupport.SeedReminder(t, w.pool, w.assignedIssue, w.u)

	// The same user in a project of another space, assigned there too.
	otherOwner, _ := testsupport.SeedUser(t, w.pool, "")
	otherSpace := testsupport.SeedSpace(t, w.pool, otherOwner)
	testsupport.AddSpaceMember(t, w.pool, otherSpace, w.u)
	other, otherPrefix := testsupport.SeedProjectInSpace(t, w.pool, otherOwner, otherSpace, "dlo")
	testsupport.AddProjectMember(t, w.pool, other, w.u, "member")
	otherIssue := otherPrefix + "-1"
	testsupport.SeedIssue(t, w.pool, other, otherOwner, otherIssue, "deact other", "open", 2)
	testsupport.SetAssignee(t, w.pool, otherIssue, w.uName)

	if _, err := w.pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, w.u); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if err := w.store.Leave(ctx, w.space, w.u, "removed_from_space", true, w.actor); err != nil {
		t.Fatalf("removing the deactivated participant: %v", err)
	}

	var openAssigned, closedAssigned, locked, watchesIssue, watchesProject, hasReminder, otherAssigned bool
	if err := w.pool.QueryRow(ctx, `
SELECT (SELECT assignee IS NOT NULL FROM issues WHERE id = $1),
       (SELECT assignee IS NOT NULL FROM issues WHERE id = $2),
       (SELECT locked_by_user_id IS NOT NULL FROM issues WHERE id = $1),
       EXISTS (SELECT 1 FROM issue_watchers WHERE issue_id = $1 AND user_id = $3),
       EXISTS (SELECT 1 FROM project_watchers WHERE project_id = $4 AND user_id = $3),
       EXISTS (SELECT 1 FROM reminders WHERE issue_id = $1 AND user_id = $3),
       (SELECT assignee IS NOT NULL FROM issues WHERE id = $5)`,
		w.assignedIssue, w.assignedIssueClosed, w.u, w.p, otherIssue).
		Scan(&openAssigned, &closedAssigned, &locked, &watchesIssue, &watchesProject, &hasReminder, &otherAssigned); err != nil {
		t.Fatalf("reading the state after the removal: %v", err)
	}
	for what, left := range map[string]bool{
		"the assignment of the open issue": openAssigned,
		"the claim":                        locked,
		"the watching of the issue":        watchesIssue,
		"the watching of the project":      watchesProject,
		"the reminder":                     hasReminder,
	} {
		if left {
			t.Errorf("the removal of a deactivated participant left %s (US-7, INV-12)", what)
		}
	}
	if !closedAssigned {
		t.Error("the closed issue lost its assignee")
	}
	if !otherAssigned {
		t.Error("an issue of a project in another space was touched")
	}
}

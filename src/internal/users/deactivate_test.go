package users_test

// deactivate_test.go — sk-674.18 T036, matrix row 46 (INV-12 "deactivation —
// not a loss of access", E-15). Reference — the spec, written before the
// primitive: "Deactivation is not a loss of access (plan 8): assignments and
// membership stay"; E-15 — the deactivated participant keeps membership
// and assignments, only sign-in and keys stop working. So a deactivation
// must leave every INV-12 consequence undone: the space and project
// memberships, the assignment of an open issue, the watching of the issue
// and of the project, the reminder.

import (
	"context"
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func TestDeactivate_IsNotALossOfAccess(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	u, uName := testsupport.SeedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, u)
	project, prefix := testsupport.SeedProjectInSpace(t, pool, owner, space, "dct")
	testsupport.AddProjectMember(t, pool, project, u, "member")
	issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "deactivation keeps", "in_progress", 2)
	testsupport.SetAssignee(t, pool, issue, uName)
	testsupport.SeedReminder(t, pool, issue, u)
	if _, err := pool.Exec(ctx, `INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')`, issue, u); err != nil {
		t.Fatalf("seed issue watcher: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, project, u); err != nil {
		t.Fatalf("seed project watcher: %v", err)
	}

	if err := store.Deactivate(ctx, u, users.HumanActor(owner, "owner")); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	var deactivated, inSpace, inProject, assigned, watchesIssue, watchesProject, hasReminder bool
	if err := pool.QueryRow(ctx, `
SELECT (SELECT deactivated_at IS NOT NULL FROM users WHERE id = $1),
       EXISTS (SELECT 1 FROM space_members WHERE space_id = $2 AND user_id = $1),
       EXISTS (SELECT 1 FROM project_members WHERE project_id = $3 AND user_id = $1),
       EXISTS (SELECT 1 FROM issues WHERE id = $4 AND assignee = $5),
       EXISTS (SELECT 1 FROM issue_watchers WHERE issue_id = $4 AND user_id = $1),
       EXISTS (SELECT 1 FROM project_watchers WHERE project_id = $3 AND user_id = $1),
       EXISTS (SELECT 1 FROM reminders WHERE issue_id = $4 AND user_id = $1)`,
		u, space, project, issue, uName).
		Scan(&deactivated, &inSpace, &inProject, &assigned, &watchesIssue, &watchesProject, &hasReminder); err != nil {
		t.Fatalf("reading the state after the deactivation: %v", err)
	}
	if !deactivated {
		t.Fatal("the user is not deactivated — the check below would prove nothing")
	}
	for what, kept := range map[string]bool{
		"the space membership":                 inSpace,
		"the project membership":               inProject,
		"the assignment of the open issue":     assigned,
		"the watching of the issue":            watchesIssue,
		"the watching of the project":          watchesProject,
		"the reminder on the issue of project": hasReminder,
	} {
		if !kept {
			t.Errorf("the deactivation removed %s — deactivation is not a loss of access (INV-12, E-15)", what)
		}
	}
}

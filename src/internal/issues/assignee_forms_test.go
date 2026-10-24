package issues

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// TestAssigneeFormsResolveToTheRightUser is the sk-523 regression gate.
//
// `issues.assignee` stores a handle verbatim in one of three accepted forms —
// username, e-mail, or user UUID. sk-294 fixed the WRITE side to treat all
// three as one person (notifications.ResolveAssigneeUserID). The READ side
// kept its own one-legged `LOWER(ua.username) = LOWER(i.assignee)`, so an
// issue assigned by e-mail or by UUID resolved to NOBODY: `assigneeUserId`
// came back NULL and the assignee was missing from their own "mine" counter.
//
// # Why the fixture uses THREE DIFFERENT users
//
// A single user carrying all three of their own handles would make the legs of
// the join interchangeable: wire the e-mail leg to the id column and every
// assertion still passes, because both point at the same row. The defect class
// this gate exists for — a leg reading the wrong column — would walk straight
// through a symmetric fixture.
//
// Here each form belongs to a DIFFERENT user, so each case asserts not merely
// "something resolved" but "THIS handle resolved to THAT person". Any
// permutation of the three legs misassigns at least one issue and fails by
// uuid comparison.
//
// # Why both halves live on ONE fixture
//
// The card's acceptance forbids the split where a signature names a person
// whom their own counter does not count. Asserting the projection in one test
// and the counter in another would let exactly that split pass: both files
// green, the product incoherent. The same three issues are therefore read
// twice — once through GetIssue (the signature) and once through
// GetProjectWithStats (the counter).
func TestAssigneeFormsResolveToTheRightUser(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	issueStore := NewStore(pool)
	projectStore := projects.NewStore(pool)

	owner, _ := testsupport.SeedUser(t, pool, "")
	projectID, prefix := testsupport.SeedProject(t, pool, owner, "sk523")

	byName, nameUser := testsupport.SeedUser(t, pool, "")
	byMail, mailUser := testsupport.SeedUser(t, pool, "")
	byUUID, _ := testsupport.SeedUser(t, pool, "")

	for _, u := range []uuid.UUID{byName, byMail, byUUID} {
		testsupport.AddProjectMember(t, pool, projectID, u, "member")
	}

	cases := []struct {
		form   string
		handle string
		want   uuid.UUID
		issue  string
	}{
		// SeedUser derives the e-mail as <username>@test.local.
		{"username", nameUser, byName, prefix + "-9001"},
		{"email", mailUser + "@test.local", byMail, prefix + "-9002"},
		{"uuid", byUUID.String(), byUUID, prefix + "-9003"},
	}

	for _, c := range cases {
		testsupport.SeedIssue(t, pool, projectID, owner, c.issue, "sk-523 "+c.form, "open", 2)
		testsupport.SetAssignee(t, pool, c.issue, c.handle)
	}

	// Part 1 — the signature.
	for _, c := range cases {
		dto, err := issueStore.GetIssue(ctx, c.issue)
		if err != nil {
			t.Fatalf("%s: GetIssue(%s): %v", c.form, c.issue, err)
		}
		switch {
		case dto.AssigneeUserID == nil:
			t.Errorf("%s form (%q): assigneeUserId is NULL — the handle did not resolve",
				c.form, c.handle)
		case *dto.AssigneeUserID != c.want:
			t.Errorf("%s form (%q): assigneeUserId = %s, want %s — the leg resolved to the wrong user",
				c.form, c.handle, *dto.AssigneeUserID, c.want)
		}
	}

	// Part 2 — the counter, through the production query.
	for _, c := range cases {
		dto, err := projectStore.GetProjectWithStats(ctx, projectID, c.want)
		if err != nil {
			t.Fatalf("%s: GetProjectWithStats: %v", c.form, err)
		}
		if dto.Stats.MyAssigned != 1 {
			t.Errorf("%s form (%q): myAssigned = %d for user %s, want 1 — "+
				"the signature names them but the counter does not count them",
				c.form, c.handle, dto.Stats.MyAssigned, c.want)
		}
	}
}

// TestAssigneeFormsInOnMeAndDelegatedCounters is the sk-523 gate for the
// SECOND family of "mine" predicates.
//
// Two different mechanisms decide whether an issue is "mine", and widening one
// without the other is its own defect. `myAssigned` (asserted above) resolves
// the handle through the users JOIN. The Delegated / On-me counters never
// joined at all: they compared the raw column against the caller's lowercased
// USERNAME. Fixing only the join would therefore have produced a NEW split —
// the signature naming a person and `myAssigned` counting them, while On-me
// still did not — which is exactly the incoherence the card's acceptance
// forbids. Both families are asserted on the same three handle forms.
//
// The fixture keeps the three forms on three DIFFERENT users for the reason
// given above: symmetric legs hide a leg wired to the wrong column.
func TestAssigneeFormsInOnMeAndDelegatedCounters(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	projectStore := projects.NewStore(pool)

	owner, ownerName := testsupport.SeedUser(t, pool, "")
	projectID, prefix := testsupport.SeedProject(t, pool, owner, "sk523c")

	byName, nameUser := testsupport.SeedUser(t, pool, "")
	byMail, mailUser := testsupport.SeedUser(t, pool, "")
	byUUID, _ := testsupport.SeedUser(t, pool, "")

	for _, u := range []uuid.UUID{byName, byMail, byUUID} {
		testsupport.AddProjectMember(t, pool, projectID, u, "member")
	}

	cases := []struct {
		form   string
		handle string
		want   uuid.UUID
		issue  string
	}{
		{"username", nameUser, byName, prefix + "-9101"},
		{"email", mailUser + "@test.local", byMail, prefix + "-9102"},
		{"uuid", byUUID.String(), byUUID, prefix + "-9103"},
	}

	// Each issue is created by `owner` and accepted by its assignee: that is
	// the shape On-me selects (created by somebody else, accepted by me) and
	// the shape Delegated selects from the creator's side.
	for _, c := range cases {
		testsupport.SeedIssue(t, pool, projectID, owner, c.issue, "sk-523 counters "+c.form, "open", 2)
		testsupport.SetAssignee(t, pool, c.issue, c.handle)
		testsupport.Accept(t, pool, c.issue, c.want, true)
	}

	find := func(t *testing.T, callerID uuid.UUID, callerName string) projects.ProjectListItem {
		t.Helper()
		res, err := projectStore.ListProjects(ctx, projects.ListProjectsArgs{
			CallerID: callerID,
			Archived: "false",
			Limit:    100,
		})
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		for _, it := range res.Items {
			if it.ID == projectID {
				return it
			}
		}
		t.Fatalf("project %s not in listing for caller %s", projectID, callerID)
		return projects.ProjectListItem{}
	}

	// On-me: each assignee must see exactly their own issue, whichever form
	// their handle was written in.
	for _, c := range cases {
		uname := testsupport.Username(t, pool, c.want)
		row := find(t, c.want, uname)
		if row.OnMeOpenCount != 1 {
			t.Errorf("%s form (%q): onMeOpenCount = %d, want 1 — "+
				"the assignee is not counted on their own On-me lens",
				c.form, c.handle, row.OnMeOpenCount)
		}
	}

	// Delegated, from the creator's side: all three are assigned to somebody
	// other than the creator, so all three are delegated away.
	if row := find(t, owner, ownerName); row.MyOutboxOpenCount != len(cases) {
		t.Errorf("myOutboxOpenCount = %d, want %d — a handle form dropped out of Delegated",
			row.MyOutboxOpenCount, len(cases))
	}

	// The differentiating half of Delegated: an issue the creator assigned
	// BACK to themselves is NOT delegated. Written in the creator's e-mail
	// form, this only holds once the predicate resolves that form — before
	// sk-523 the e-mail did not equal the username and the issue counted as
	// delegated to a stranger, inflating the creator's own outbox.
	self := prefix + "-9104"
	testsupport.SeedIssue(t, pool, projectID, owner, self, "sk-523 self by email", "open", 2)
	testsupport.SetAssignee(t, pool, self, ownerName+"@test.local")
	if row := find(t, owner, ownerName); row.MyOutboxOpenCount != len(cases) {
		t.Errorf("myOutboxOpenCount = %d, want %d — an issue assigned to the creator "+
			"by their own e-mail was counted as delegated to somebody else",
			row.MyOutboxOpenCount, len(cases))
	}
}

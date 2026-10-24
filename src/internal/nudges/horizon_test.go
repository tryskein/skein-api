package nudges

// horizon_test.go — sk-303.14, repair A/8.
//
// THE VISIBILITY HORIZON IN EXPRESSION FORM — the most dangerous of the
// review warnings.
//
// # What was not covered
//
// The unreviewed fragment ends with the project visibility filter — the
// access relation for the addressee (authz.RecipientVisibleExpr since sk-674.18
// T010; before it, an admin bypass OR a project_members probe). For the
// background worker the addressee is a join column, not a placeholder. A
// mutation that made that filter true for any existing user fully removed
// it from reminder selection — and it was GREEN on ./internal/nudges/...
// ./internal/handler/... By contrast the same-shaped mutation of INV-6 (the
// mechanic's off switch) went red: it had an assertion.
//
// # What is asserted here
//
// Both sides, because a one-sided half passes on any broken selection:
//
//	not a member and not an administrator → NOT an addressee (the mutation
//	                                        goes red here);
//	administrator, not a member           → addressee (the admin bypass
//	                                        exists and works; without this
//	                                        half "the horizon is honoured"
//	                                        would be reached by dropping
//	                                        the bypass);
//	external user, not a member           → NOT an addressee.
//
// There is no "external ADMINISTRATOR" case here: the schema forbids it with
// the users_external_not_admin constraint, and an observation for the
// `NOT au.is_external` conjunct does not exist in this class.

import (
	"testing"
	"time"

	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// TestUnreviewedSelection_RespectsTheProjectHorizonInExpressionForm — the
// horizon gate.
func TestUnreviewedSelection_RespectsTheProjectHorizonInExpressionForm(t *testing.T) {
	ctx := t.Context()

	for _, tc := range []struct {
		name       string
		role       string
		external   bool
		member     bool
		wantMember bool
	}{
		{name: "project member", role: "user", member: true, wantMember: true},
		{name: "not a member and not an administrator", role: "user", member: false, wantMember: false},
		{name: "administrator, not a member", role: "admin", member: false, wantMember: true},
		// There is NO "external administrator" case here, and that is not
		// a skip: the schema forbids it with the users_external_not_admin
		// constraint (attempting to mark an administrator as external
		// fails with SQLSTATE 23514). The `NOT au.is_external` conjunct
		// in the admin bypass is therefore a second line of defence, not
		// a distinguisher of observable states; there is nothing to check
		// it with in this class, and inventing an observation for it is
		// not allowed.
		{name: "external user, not a member", role: "user", external: true,
			member: false, wantMember: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			author, _ := testsupport.SeedUser(t, e.pool, tc.role)
			if tc.external {
				if _, err := e.pool.Exec(ctx,
					`UPDATE users SET is_external = true WHERE id = $1`, author); err != nil {
					t.Fatalf("marking as external: %v", err)
				}
			}
			if tc.member {
				testsupport.AddProjectMember(t, e.pool, e.project, author, "member")
			}
			_, workerName := e.user(t)

			id := e.seed(t, issue{suffix: "horizon", status: "closed",
				assignee: workerName, createdBy: author, reviewRequired: true})
			// The unreviewed countdown base is the close moment. It must
			// sit past the first-mark lower bound, otherwise the addressee
			// would miss the selection for a completely different reason
			// and the horizon assertion would be checked against emptiness.
			if _, err := e.pool.Exec(ctx,
				`UPDATE issues SET closed_at = now() - interval '2 days' WHERE id = $1`,
				id); err != nil {
				t.Fatalf("shifting the close moment: %v", err)
			}

			_, got := candidateFor(t, ctx, e.store, review.CategoryUnreviewed,
				time.Now().UTC(), author)
			if got == tc.wantMember {
				return
			}
			if tc.wantMember {
				t.Errorf("%s: the author did NOT enter the unreviewed selection, "+
					"although the project is visible to them — they will not receive "+
					"reminders of their own results", tc.name)
				return
			}
			t.Errorf("%s: the author entered the unreviewed selection, although "+
				"the project is NOT visible to them. The project membership filter "+
				"in expression form has been dropped — the worker will list to a "+
				"person the issues of a project they have no access to "+
				"(INV-14, SC-021)", tc.name)
		})
	}
}

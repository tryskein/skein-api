package authz_test

// authz_integration_test.go — sk-674.18 T007: every entry of the package
// answers through the access relation on the T003 corpus
// (testsupport.SeedTenancy), with the rule of spaces (T017, INV-1).
//
// Reference — currentReach below: the target table of matrix row 22
// (testsupport.TenancyVisible), with no gaps left since T017 (an admin
// reaches no "Personal" of others; a deactivated user reaches nothing).
// Levels (INV-1, INV-4, US-6): the owner of a project — its creator, the
// owner of its space (the implicit owner, vPr on A), the owner of the
// "Personal" — is 'owner'; an instance admin's member row is 'admin_member'
// and his reach alone 'admin'; any other member row (and an external
// collaborator) is 'member'.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

func currentReach(f *testsupport.Tenancy, a testsupport.Actor, p testsupport.TenancyProject) bool {
	return f.Sees(a, p)
}

func currentLevel(f *testsupport.Tenancy, a testsupport.Actor, p testsupport.TenancyProject, t *testing.T) authz.Level {
	switch {
	case p.OwnerID == f.User(t, a).ID:
		return authz.LevelOwner
	case a == testsupport.ActorSpaceOwner && p.Tenant == "A":
		return authz.LevelOwner
	case a == testsupport.ActorAdminA && p.Tenant == "A":
		return authz.LevelAdminMember
	case a.IsAdmin():
		return authz.LevelAdmin
	}
	return authz.LevelMember
}

func callerCtx(f *testsupport.Tenancy, t *testing.T, a testsupport.Actor) context.Context {
	u := f.User(t, a)
	return auth.WithIsExternal(auth.WithUserRole(auth.WithUserID(context.Background(), u.ID), u.Role), u.External)
}

func TestAuthz_ThroughTheRelation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	projects := f.Projects()
	missing := uuid.New()

	for _, a := range testsupport.TenancyActors {
		t.Run(string(a), func(t *testing.T) {
			ctx := callerCtx(f, t, a)
			s := authz.FromContext(ctx)
			uArg, visible, err := s.Bind(ctx, pool)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			cArg := s.CallerID()

			for _, p := range projects {
				reach := currentReach(f, a, p)

				// ProjectAccess and Can.
				level, err := authz.ProjectAccess(ctx, pool, s, p.ID)
				switch {
				case reach && err != nil:
					t.Errorf("%s: ProjectAccess: %v, want level %s", p.Key(), err, currentLevel(f, a, p, t))
				case reach && level != currentLevel(f, a, p, t):
					t.Errorf("%s: level %q, want %q", p.Key(), level, currentLevel(f, a, p, t))
				case !reach && !errors.Is(err, authz.ErrNotVisible):
					t.Errorf("%s: ProjectAccess = (%q, %v), want ErrNotVisible", p.Key(), level, err)
				}
				for _, action := range []authz.Action{authz.ActionView, authz.ActionManageMembers, authz.ActionForceUnlock} {
					_, err := authz.Can(ctx, pool, s, action, p.ID)
					switch {
					case !reach:
						if !errors.Is(err, authz.ErrNotVisible) {
							t.Errorf("%s: Can(%s) = %v, want ErrNotVisible", p.Key(), action, err)
						}
					case authz.Allows(currentLevel(f, a, p, t), action):
						if err != nil {
							t.Errorf("%s: Can(%s) = %v, want allowed", p.Key(), action, err)
						}
					default:
						if !errors.Is(err, authz.ErrForbidden) {
							t.Errorf("%s: Can(%s) = %v, want ErrForbidden", p.Key(), action, err)
						}
					}
				}

				// ProjectVisibleExpr, VisibleSetExpr, VisibleJoin and
				// RecipientVisibleExpr on the project's marker issue.
				var byExpr, bySet, byRecipient bool
				var joined []string
				if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM issues i WHERE i.id = $3 AND `+authz.ProjectVisibleExpr("i", "$1", "$4")+`),
       EXISTS (SELECT 1 FROM issues i WHERE i.id = $3 AND `+authz.VisibleSetExpr("i.project_id", "$4")+`),
       EXISTS (SELECT 1 FROM issues i WHERE i.id = $3 AND `+authz.RecipientVisibleExpr("$2::uuid", "i.project_id")+`),
       ARRAY(SELECT va.access FROM issues i `+authz.VisibleJoin("i", "$1", "$2", "va")+` WHERE i.id = $3)`,
					uArg, cArg, p.IssueID, visible).Scan(&byExpr, &bySet, &byRecipient, &joined); err != nil {
					t.Fatalf("%s: SQL fragments: %v", p.Key(), err)
				}
				if byExpr != reach || bySet != reach || byRecipient != reach {
					t.Errorf("%s: ProjectVisibleExpr %v, VisibleSetExpr %v, RecipientVisibleExpr %v, want %v",
						p.Key(), byExpr, bySet, byRecipient, reach)
				}
				wantJoined := []string{}
				if reach {
					wantJoined = []string{string(currentLevel(f, a, p, t))}
				}
				if !slices.Equal(joined, wantJoined) {
					t.Errorf("%s: VisibleJoin rows %v, want %v", p.Key(), joined, wantJoined)
				}
			}
			if _, err := authz.ProjectAccess(ctx, pool, s, missing); !errors.Is(err, authz.ErrNotVisible) {
				t.Errorf("missing project: %v, want ErrNotVisible", err)
			}

			err = authz.CanCreateProject(ctx, pool, s)
			if external := f.User(t, a).External; external != errors.Is(err, authz.ErrForbidden) || (!external && err != nil) {
				t.Errorf("CanCreateProject = %v, external %v", err, external)
			}
		})
	}

	t.Run("unrestricted and undeclared", func(t *testing.T) {
		internal := authz.WithUnrestricted(context.Background())
		for _, p := range projects {
			if level, err := authz.ProjectAccess(internal, pool, authz.FromContext(internal), p.ID); err != nil || level != authz.LevelInternal {
				t.Errorf("unrestricted on %s: (%q, %v), want internal", p.Key(), level, err)
			}
			if _, err := authz.ProjectAccess(context.Background(), pool, authz.FromContext(context.Background()), p.ID); !errors.Is(err, authz.ErrNotVisible) {
				t.Errorf("undeclared on %s: %v, want ErrNotVisible", p.Key(), err)
			}
		}
		if _, err := authz.ProjectAccess(internal, pool, authz.Unrestricted(), missing); !errors.Is(err, authz.ErrNotVisible) {
			t.Errorf("unrestricted on a missing project: %v, want ErrNotVisible", err)
		}
		if err := authz.CanCreateProject(context.Background(), pool, authz.FromContext(context.Background())); !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("undeclared CanCreateProject: %v, want ErrForbidden", err)
		}
	})

	t.Run("batch", func(t *testing.T) {
		ids := make([]uuid.UUID, 0, len(projects)+1)
		for _, p := range projects {
			ids = append(ids, p.ID)
		}
		ids = append(ids, missing, projects[0].ID) // a missing id and a duplicate
		entries := make([]authz.BatchEntry, 0, len(testsupport.TenancyActors)+1)
		for _, a := range testsupport.TenancyActors {
			entries = append(entries, authz.BatchEntry{UserID: f.User(t, a).ID, ProjectIDs: ids, AllVisible: true})
		}
		entries = append(entries, authz.BatchEntry{UserID: uuid.New(), ProjectIDs: ids})

		res, err := authz.ResolveBatch(context.Background(), pool, entries)
		if err != nil {
			t.Fatalf("ResolveBatch: %v", err)
		}
		if len(res) != len(entries) {
			t.Fatalf("%d results for %d entries", len(res), len(entries))
		}
		for i, a := range testsupport.TenancyActors {
			u, r := f.User(t, a), res[i]
			active := !u.Deactivated
			if r.Active != active || r.InternalAdmin != (a.IsAdmin()) || r.External != u.External {
				t.Errorf("%s: active %v internalAdmin %v external %v, want %v %v %v",
					a, r.Active, r.InternalAdmin, r.External, active, a.IsAdmin(), u.External)
			}
			var want []uuid.UUID
			for _, p := range projects { // request order
				if active && currentReach(f, a, p) {
					want = append(want, p.ID)
				}
			}
			if want == nil {
				want = []uuid.UUID{}
			}
			if !slices.Equal(r.Allowed, want) {
				t.Errorf("%s: allowed %v, want %v", a, r.Allowed, want)
			}
			if r.Visible == nil {
				t.Errorf("%s: visible absent, but the entry asked for it", a)
			}
			for _, p := range projects {
				if got := slices.Contains(r.Visible, p.ID); got != (active && currentReach(f, a, p)) {
					t.Errorf("%s: visible contains %s = %v", a, p.Key(), got)
				}
			}
		}
		stranger := res[len(res)-1]
		if stranger.Active || len(stranger.Allowed) != 0 || stranger.Visible != nil {
			t.Errorf("unknown user: %+v, want inactive, nothing allowed, visible absent", stranger)
		}
	})
}

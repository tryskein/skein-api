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
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
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

// visibleIssueCorpus is the input of the P-VIS table (sk-1007 T003): the
// marker issue of every corpus project, an id that never existed and the id
// of an issue that was seeded and deleted. Both foreign ids carry the prefix
// of A.space, a project most readers see, so only the missing row can keep
// them out of the answer.
func visibleIssueCorpus(t *testing.T, pool *pgxpool.Pool, f *testsupport.Tenancy) (ids []string, never, deleted string) {
	t.Helper()
	never = f.ASpace.IDPrefix + "-424242"
	deleted = testsupport.SeedIssue(t, pool, f.ASpace.ID, f.ASpace.OwnerID, f.ASpace.IDPrefix+"-2", "deleted", "open", 2)
	if _, err := pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, deleted); err != nil {
		t.Fatalf("delete %s: %v", deleted, err)
	}
	for _, p := range f.Projects() {
		ids = append(ids, p.IssueID)
	}
	return append(ids, never, deleted), never, deleted
}

// issueIDsOf is the expected answer: the marker issues of the projects whose
// legend key is in keep.
func issueIDsOf(f *testsupport.Tenancy, keep func(testsupport.TenancyProject) bool) []string {
	out := []string{}
	for _, p := range f.Projects() {
		if keep(p) {
			out = append(out, p.IssueID)
		}
	}
	slices.Sort(out)
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Matrix row 1 (sk-1007 ИНВ-1, R-5, K1): every reader of the corpus gets
// exactly the marker issues of the projects it sees by the legend (f.Sees);
// an admin sees what the relation gives him — not the Personal of others; a
// deleted or never-existing id is never returned.
func TestVisibleIssueIDs_PerReader(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	ids, never, deleted := visibleIssueCorpus(t, pool, f)

	for _, a := range testsupport.TenancyActors {
		t.Run(string(a), func(t *testing.T) {
			ctx := callerCtx(f, t, a)
			got, err := authz.VisibleIssueIDs(ctx, pool, authz.FromContext(ctx), ids)
			if err != nil {
				t.Fatalf("VisibleIssueIDs: %v", err)
			}
			want := issueIDsOf(f, func(p testsupport.TenancyProject) bool { return f.Sees(a, p) })
			if g := sortedKeys(got); !slices.Equal(g, want) {
				t.Errorf("visible %v, want %v", g, want)
			}
			for _, id := range []string{never, deleted} {
				if _, ok := got[id]; ok {
					t.Errorf("%s is visible, want an issue without a row never visible", id)
				}
			}
		})
	}
}

// Matrix row 2 (sk-1007 ИНВ-1, edge «узкая область ключа»): the key's scope
// narrows the reader's relation. uAB with a projects key on A.members sees
// only that issue; with a spaces key on B only the issues of B's projects; a
// key id without a grant sees nothing; scope "all" is the relation itself.
func TestVisibleIssueIDs_KeyScope(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	ids, _, _ := visibleIssueCorpus(t, pool, f)
	u := f.User(t, testsupport.ActorBoth)

	keyCtx := func(g *apikeys.Grant) context.Context {
		ctx := auth.WithUserRole(auth.WithUserID(context.Background(), u.ID), u.Role)
		ctx = auth.WithAPIKeyID(ctx, uuid.New())
		if g != nil {
			ctx = auth.WithKeyGrant(ctx, *g)
		}
		return authz.WithRequestCache(ctx)
	}
	grant := func(sc apikeys.Scope) *apikeys.Grant {
		return &apikeys.Grant{Level: apikeys.LevelManage, Scope: sc}
	}
	keys := func(k ...string) func(testsupport.TenancyProject) bool {
		return func(p testsupport.TenancyProject) bool { return slices.Contains(k, p.Key()) }
	}

	cases := []struct {
		name  string
		grant *apikeys.Grant
		keep  func(testsupport.TenancyProject) bool
	}{
		{"projects A.members", grant(apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{f.AMembers.ID}}), keys("A.members")},
		{"spaces B", grant(apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{f.SpaceB}}), keys("B.space", "B.members")},
		{"key without a grant", nil, keys()},
		{"all", grant(apikeys.Scope{Mode: apikeys.ScopeAll}), func(p testsupport.TenancyProject) bool {
			return f.Sees(testsupport.ActorBoth, p)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := keyCtx(c.grant)
			got, err := authz.VisibleIssueIDs(ctx, pool, authz.FromContext(ctx), ids)
			if err != nil {
				t.Fatalf("VisibleIssueIDs: %v", err)
			}
			if g, want := sortedKeys(got), issueIDsOf(f, c.keep); !slices.Equal(g, want) {
				t.Errorf("visible %v, want %v", g, want)
			}
		})
	}
}

// Matrix row 3 (sk-1007 K1, K7): fail-closed. A context without a principal
// and without WithUnrestricted sees none of the existing issues; declared
// internal work sees every existing one and still not the deleted or the
// never-existing id.
func TestVisibleIssueIDs_FailClosed(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	ids, _, _ := visibleIssueCorpus(t, pool, f)

	undeclared := context.Background()
	got, err := authz.VisibleIssueIDs(undeclared, pool, authz.FromContext(undeclared), ids)
	if err != nil {
		t.Fatalf("undeclared: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("undeclared context sees %v, want nothing", sortedKeys(got))
	}

	internal := authz.WithUnrestricted(context.Background())
	got, err = authz.VisibleIssueIDs(internal, pool, authz.FromContext(internal), ids)
	if err != nil {
		t.Fatalf("unrestricted: %v", err)
	}
	all := issueIDsOf(f, func(testsupport.TenancyProject) bool { return true })
	if g := sortedKeys(got); !slices.Equal(g, all) {
		t.Errorf("unrestricted sees %v, want every existing issue %v", g, all)
	}
}

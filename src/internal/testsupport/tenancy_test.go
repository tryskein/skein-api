package testsupport

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The expected membership of every fixture project, written from the
// matrix legend (O-f1) rather than from SeedTenancy's
// member lists: actor → role. The owner is listed with role "owner".
var wantTenantMembers = map[string]map[Actor]string{
	"A.space": {
		ActorSpaceOwner:           "owner",
		ActorOnlyA:                "member",
		ActorBoth:                 "member",
		ActorProjectOwner:         "member",
		ActorParticipantNoMembers: "member",
		ActorAdminA:               "member",
		ActorDeactivated:          "member",
	},
	"A.members": {
		ActorProjectOwner: "owner",
		ActorOnlyA:        "member",
		ActorBoth:         "member",
		ActorExternal:     "member",
		ActorAdminA:       "member",
		ActorDeactivated:  "member",
	},
	"B.space": {
		ActorOnlyB: "owner",
		ActorBoth:  "member",
	},
	"B.members": {
		ActorOnlyB: "owner",
		ActorBoth:  "member",
	},
}

// The participants of the two spaces, owner included (role "owner").
var wantSpaceMembers = map[string]map[Actor]string{
	"A": {
		ActorSpaceOwner:           "owner",
		ActorOnlyA:                "member",
		ActorBoth:                 "member",
		ActorProjectOwner:         "member",
		ActorParticipantNoMembers: "member",
		ActorAdminA:               "member",
		ActorDeactivated:          "member",
	},
	"B": {
		ActorOnlyB: "owner",
		ActorBoth:  "member",
	},
}

// Every non-external actor owns exactly one Personal (INV-8); vnsh has none.
var wantPersonalOwners = []Actor{
	ActorOnlyA, ActorOnlyB, ActorBoth, ActorProjectOwner, ActorSpaceOwner,
	ActorParticipantNoMembers, ActorAdminA, ActorAdmin0, ActorDeactivated,
	ActorOutsider,
}

type wantUserFlags struct {
	role        string
	external    bool
	deactivated bool
}

var wantUsers = map[Actor]wantUserFlags{
	ActorOnlyA:                {"user", false, false},
	ActorOnlyB:                {"user", false, false},
	ActorBoth:                 {"user", false, false},
	ActorProjectOwner:         {"user", false, false},
	ActorSpaceOwner:           {"user", false, false},
	ActorParticipantNoMembers: {"user", false, false},
	ActorExternal:             {"user", true, false},
	ActorAdminA:               {"admin", false, false},
	ActorAdmin0:               {"admin", false, false},
	ActorDeactivated:          {"user", false, true},
	ActorOutsider:             {"user", false, false},
}

// readMembers returns project_members of one project as actor → role,
// mapping user ids back to actors through the fixture handle. A row for a
// user outside the fixture fails the test.
func readMembers(t *testing.T, pool *pgxpool.Pool, f *Tenancy, projectID uuid.UUID) map[Actor]string {
	t.Helper()
	byID := make(map[uuid.UUID]Actor, len(f.Users))
	for a, u := range f.Users {
		byID[u.ID] = a
	}
	rows, err := pool.Query(context.Background(),
		`SELECT user_id, role FROM project_members WHERE project_id = $1`, projectID)
	if err != nil {
		t.Fatalf("read members of %s: %v", projectID, err)
	}
	defer rows.Close()
	got := map[Actor]string{}
	for rows.Next() {
		var uid uuid.UUID
		var role string
		if err := rows.Scan(&uid, &role); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		a, ok := byID[uid]
		if !ok {
			t.Fatalf("project %s has a member %s outside the fixture", projectID, uid)
		}
		got[a] = role
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read members rows: %v", err)
	}
	return got
}

func assertTenancyShape(t *testing.T, pool *pgxpool.Pool, f *Tenancy) {
	t.Helper()
	ctx := context.Background()

	if got := len(f.Users); got != len(wantUsers) {
		t.Fatalf("users: got %d actors, want %d", got, len(wantUsers))
	}
	for a, want := range wantUsers {
		u := f.User(t, a)
		var role string
		var external, deactivated bool
		if err := pool.QueryRow(ctx,
			`SELECT role, is_external, deactivated_at IS NOT NULL FROM users WHERE id = $1`, u.ID,
		).Scan(&role, &external, &deactivated); err != nil {
			t.Fatalf("read user %s: %v", a, err)
		}
		if got := (wantUserFlags{role, external, deactivated}); got != want {
			t.Errorf("user %s: got %+v, want %+v", a, got, want)
		}
	}

	tenant := map[string]TenancyProject{
		"A.space": f.ASpace, "A.members": f.AMembers,
		"B.space": f.BSpace, "B.members": f.BMembers,
	}
	for label, want := range wantTenantMembers {
		got := readMembers(t, pool, f, tenant[label].ID)
		if !maps.Equal(got, want) {
			t.Errorf("%s members: got %v, want %v", label, got, want)
		}
	}

	// Spaces: participants, and every project in its space with the
	// visibility its kind names.
	for label, spaceID := range map[string]uuid.UUID{"A": f.SpaceA, "B": f.SpaceB} {
		got := map[Actor]string{}
		rows, err := pool.Query(ctx, `SELECT user_id, role FROM space_members WHERE space_id = $1`, spaceID)
		if err != nil {
			t.Fatalf("read space %s: %v", label, err)
		}
		for rows.Next() {
			var id uuid.UUID
			var role string
			if err := rows.Scan(&id, &role); err != nil {
				t.Fatalf("scan space %s: %v", label, err)
			}
			for a, u := range f.Users {
				if u.ID == id {
					got[a] = role
				}
			}
		}
		rows.Close()
		if want := wantSpaceMembers[label]; !maps.Equal(got, want) {
			t.Errorf("space %s participants: got %v, want %v", label, got, want)
		}
	}
	wantPlace := map[string]struct {
		space      uuid.UUID
		visibility string
	}{
		"A.space": {f.SpaceA, "space"}, "A.members": {f.SpaceA, "members"},
		"B.space": {f.SpaceB, "space"}, "B.members": {f.SpaceB, "members"},
	}
	for _, p := range f.Projects() {
		want, ok := wantPlace[p.Key()]
		if !ok {
			want.space, want.visibility = f.PersonalSpace[p.PersonalOf], "members"
		}
		var space uuid.UUID
		var visibility, kind string
		if err := pool.QueryRow(ctx, `
SELECT p.space_id, p.visibility, s.kind FROM projects p JOIN spaces s ON s.id = p.space_id WHERE p.id = $1`,
			p.ID).Scan(&space, &visibility, &kind); err != nil {
			t.Fatalf("read placement of %s: %v", p.Key(), err)
		}
		wantKind := "regular"
		if p.Kind == KindPersonal {
			wantKind = "personal"
		}
		if space != want.space || visibility != want.visibility || kind != wantKind {
			t.Errorf("%s: in space %s (%s), visibility %s; want %s (%s), %s",
				p.Key(), space, kind, visibility, want.space, wantKind, want.visibility)
		}
	}

	gotOwners := slices.Sorted(maps.Keys(f.Personal))
	wantOwners := slices.Sorted(slices.Values(wantPersonalOwners))
	if !slices.Equal(gotOwners, wantOwners) {
		t.Errorf("personal owners: got %v, want %v", gotOwners, wantOwners)
	}
	for a, p := range f.Personal {
		got := readMembers(t, pool, f, p.ID)
		if want := map[Actor]string{a: "owner"}; !maps.Equal(got, want) {
			t.Errorf("L(%s) members: got %v, want %v", a, got, want)
		}
	}

	// Key() names every project in the vocabulary of TenancyVisible.
	var gotKeys []string
	for _, p := range f.Projects() {
		gotKeys = append(gotKeys, p.Key())
	}
	wantKeys := []string{"A.space", "A.members", "B.space", "B.members"}
	for _, a := range wantPersonalOwners {
		wantKeys = append(wantKeys, "L:"+string(a))
	}
	slices.Sort(gotKeys)
	slices.Sort(wantKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("project keys: got %v, want %v", gotKeys, wantKeys)
	}

	// Every project resolves by its alias and carries its marker issue.
	for _, p := range f.Projects() {
		var aliasProject uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT project_id FROM project_aliases WHERE alias = $1`, p.Alias,
		).Scan(&aliasProject); err != nil {
			t.Fatalf("alias %s: %v", p.Alias, err)
		}
		if aliasProject != p.ID {
			t.Errorf("alias %s points to %s, want %s", p.Alias, aliasProject, p.ID)
		}
		var issueProject uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT project_id FROM issues WHERE id = $1`, p.IssueID,
		).Scan(&issueProject); err != nil {
			t.Fatalf("marker issue %s: %v", p.IssueID, err)
		}
		if issueProject != p.ID {
			t.Errorf("marker issue %s lives in %s, want %s", p.IssueID, issueProject, p.ID)
		}
	}
}

// TestSeedTenancy_ShapeAndCleanup seeds the fixture twice in a row inside
// one package run — the "-p 1, two runs do not conflict" gate of T003 —
// checks each seeding against the literal legend tables, and checks that
// each seeding's rows are gone once its subtest has finished.
func TestSeedTenancy_ShapeAndCleanup(t *testing.T) {
	pool := NewTestPool(t)

	var seeded []*Tenancy
	for _, run := range []string{"first", "second"} {
		t.Run(run, func(t *testing.T) {
			f := SeedTenancy(t, pool)
			assertTenancyShape(t, pool, f)
			seeded = append(seeded, f)
		})
	}
	if len(seeded) != 2 {
		t.Fatalf("seeded %d fixtures, want 2", len(seeded))
	}

	// The two runs share no project or user.
	firstProjects := map[uuid.UUID]bool{}
	for _, p := range seeded[0].Projects() {
		firstProjects[p.ID] = true
	}
	for _, p := range seeded[1].Projects() {
		if firstProjects[p.ID] {
			t.Errorf("project %s seeded by both runs", p.ID)
		}
	}

	// Cleanup of each subtest removed everything it seeded.
	for i, f := range seeded {
		for _, p := range f.Projects() {
			if n := CountRows(t, pool, "projects", "id", p.ID); n != 0 {
				t.Errorf("run %d: project %s survived cleanup", i, p.Name)
			}
			if n := CountRows(t, pool, "issues", "project_id", p.ID); n != 0 {
				t.Errorf("run %d: %d issues of %s survived cleanup", i, n, p.Name)
			}
			if n := CountRows(t, pool, "project_aliases", "alias", p.Alias); n != 0 {
				t.Errorf("run %d: alias %s survived cleanup", i, p.Alias)
			}
		}
		for _, sp := range append([]uuid.UUID{f.SpaceA, f.SpaceB}, slices.Collect(maps.Values(f.PersonalSpace))...) {
			if n := CountRows(t, pool, "spaces", "id", sp); n != 0 {
				t.Errorf("run %d: space %s survived cleanup", i, sp)
			}
		}
		for a, u := range f.Users {
			if n := CountRows(t, pool, "users", "id", u.ID); n != 0 {
				t.Errorf("run %d: user %s survived cleanup", i, a)
			}
		}
	}
}

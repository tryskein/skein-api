package authz_test

// relation_test.go — sk-674.18 T017, matrix rows 17–20: the access relation
// (skein_project_access_set / skein_project_access) on the two-tenant
// fixture (testsupport.SeedTenancy, O-f1). The reference is the spec,
// written before the relation:
//
//   - row 17 (INV-1 a–d, INV-8, INV-23): the level of every actor on every
//     fixture project equals the literal table below — Personal(x) only for x;
//     a non-personal project for a participant with the space owner's
//     implicit ownership (INV-4) or a membership row; vnsh by its row only,
//     never a "Personal"; an admin (f+) every non-personal project — owner of
//     nothing beyond his own role: `admin` without a row, `admin_member`
//     with a member row (US-6); deakt nothing (active users only); post only
//     his own Pers. The levels follow the rows' roles, not the sources;
//   - row 18 (D-2): with instance_settings.admin_enabled = false the same
//     fixture gives the admins only the paths (a–c) — without restarting
//     anything, the flag is read per call — and nobody else changes;
//   - row 19 (INV-4): the space owner is `owner` of every project of his
//     space without a membership row, the `members` project included, and
//     stays its owner after the last explicit owner leaves the project. The
//     same for the owner of the common space after the transition is
//     migration_000071_spaces_transition_test.go (row 38);
//   - row 20 (INV-3, basis): the point form agrees with the set form for
//     every pair — the same level; `forbidden` for an existing project out
//     of reach, `not_found` for a project that does not exist.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const (
	lvOwner       = "owner"
	lvMember      = "member"
	lvAdmin       = "admin"
	lvAdminMember = "admin_member"
)

// relationTable is row 17: actor → project key → level; a missing key is
// «no access».
var relationTable = map[testsupport.Actor]map[string]string{
	testsupport.ActorOnlyA: {"A.space": lvMember, "A.members": lvMember, "L:onlyA": lvOwner},
	testsupport.ActorOnlyB: {"B.space": lvOwner, "B.members": lvOwner, "L:onlyB": lvOwner},
	testsupport.ActorBoth: {"A.space": lvMember, "A.members": lvMember,
		"B.space": lvMember, "B.members": lvMember, "L:both": lvOwner},
	testsupport.ActorProjectOwner: {"A.space": lvMember, "A.members": lvOwner, "L:projectOwner": lvOwner},
	// vPr: the implicit owner of both A projects (no row on A.members).
	testsupport.ActorSpaceOwner:           {"A.space": lvOwner, "A.members": lvOwner, "L:spaceOwner": lvOwner},
	testsupport.ActorParticipantNoMembers: {"A.space": lvMember, "L:participantNoMembers": lvOwner},
	testsupport.ActorExternal:             {"A.members": lvMember},
	testsupport.ActorAdminA: {"A.space": lvAdminMember, "A.members": lvAdminMember,
		"B.space": lvAdmin, "B.members": lvAdmin, "L:adminA": lvOwner},
	testsupport.ActorAdmin0: {"A.space": lvAdmin, "A.members": lvAdmin,
		"B.space": lvAdmin, "B.members": lvAdmin, "L:admin0": lvOwner},
	testsupport.ActorDeactivated: {},
	testsupport.ActorOutsider:    {"L:outsider": lvOwner},
}

// relationTableAdminOff is row 18: the admins without the branch (d) keep
// only their own paths (a–c); every other actor is as in relationTable.
var relationTableAdminOff = map[testsupport.Actor]map[string]string{
	testsupport.ActorAdminA: {"A.space": lvMember, "A.members": lvMember, "L:adminA": lvOwner},
	testsupport.ActorAdmin0: {"L:admin0": lvOwner},
}

// setLevels reads the set form for user, restricted to the fixture.
func setLevels(t *testing.T, pool *pgxpool.Pool, f *testsupport.Tenancy, user uuid.UUID) map[string]string {
	t.Helper()
	byID := map[uuid.UUID]string{}
	ids := make([]uuid.UUID, 0)
	for _, p := range f.Projects() {
		byID[p.ID] = p.Key()
		ids = append(ids, p.ID)
	}
	rows, err := pool.Query(context.Background(), `
SELECT a.project_id, a.access FROM skein_project_access_set($1) a WHERE a.project_id = ANY($2)`, user, ids)
	if err != nil {
		t.Fatalf("reading the set form: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id uuid.UUID
		var level string
		if err := rows.Scan(&id, &level); err != nil {
			t.Fatal(err)
		}
		if prev, dup := out[byID[id]]; dup {
			t.Errorf("project %s appears twice in the set (%s, %s)", byID[id], prev, level)
		}
		out[byID[id]] = level
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkTable(t *testing.T, pool *pgxpool.Pool, f *testsupport.Tenancy, want func(a testsupport.Actor) map[string]string) {
	t.Helper()
	for _, a := range testsupport.TenancyActors {
		got := setLevels(t, pool, f, f.User(t, a).ID)
		exp := want(a)
		for _, p := range f.Projects() {
			if got[p.Key()] != exp[p.Key()] {
				t.Errorf("%s on %s: level %q, want %q", a, p.Key(), got[p.Key()], exp[p.Key()])
			}
		}
	}
}

func setAdminFlag(t *testing.T, pool *pgxpool.Pool, on bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE instance_settings SET admin_enabled = $1 WHERE id = 1`, on); err != nil {
		t.Fatalf("admin flag: %v", err)
	}
}

func TestRelation_LevelsOnTheTenancyFixture(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	var flagBefore bool
	if err := pool.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&flagBefore); err != nil {
		t.Fatalf("reading the admin flag: %v", err)
	}
	t.Cleanup(func() { setAdminFlag(t, pool, flagBefore) })
	setAdminFlag(t, pool, true)
	f := testsupport.SeedTenancy(t, pool)

	t.Run("row 17: the literal table", func(t *testing.T) {
		checkTable(t, pool, f, func(a testsupport.Actor) map[string]string { return relationTable[a] })
	})

	t.Run("row 20: the point form agrees with the set", func(t *testing.T) {
		missing := uuid.New()
		for _, a := range testsupport.TenancyActors {
			u := f.User(t, a)
			set := setLevels(t, pool, f, u.ID)
			for _, p := range f.Projects() {
				var point string
				if err := pool.QueryRow(ctx, `SELECT skein_project_access($1, $2)`, u.ID, p.ID).Scan(&point); err != nil {
					t.Fatal(err)
				}
				want := set[p.Key()]
				if want == "" {
					want = "forbidden"
				}
				if point != want {
					t.Errorf("%s on %s: point %q, set %q", a, p.Key(), point, set[p.Key()])
				}
			}
			var point string
			if err := pool.QueryRow(ctx, `SELECT skein_project_access($1, $2)`, u.ID, missing).Scan(&point); err != nil {
				t.Fatal(err)
			}
			if point != "not_found" {
				t.Errorf("%s on a nonexistent project: point %q, want not_found", a, point)
			}
		}
	})

	t.Run("row 18: the admin flag is read per call", func(t *testing.T) {
		setAdminFlag(t, pool, false)
		checkTable(t, pool, f, func(a testsupport.Actor) map[string]string {
			if off, ok := relationTableAdminOff[a]; ok {
				return off
			}
			return relationTable[a]
		})
		setAdminFlag(t, pool, true)
		checkTable(t, pool, f, func(a testsupport.Actor) map[string]string { return relationTable[a] })
	})

	// Last: it changes the fixture.
	t.Run("row 19: the implicit owner stays when the explicit one leaves", func(t *testing.T) {
		spaceOwner := f.User(t, testsupport.ActorSpaceOwner).ID
		if n := rowsOf(t, pool, f.AMembers.ID, spaceOwner); n != 0 {
			t.Fatalf("the fixture gave the space owner a row on A.members")
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`,
			f.AMembers.ID, f.User(t, testsupport.ActorProjectOwner).ID); err != nil {
			t.Fatalf("the explicit owner leaves: %v", err)
		}
		var owners int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE project_id = $1 AND role = 'owner'`,
			f.AMembers.ID).Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if owners != 0 {
			t.Fatalf("A.members still has %d explicit owners — the case needs none", owners)
		}
		if got := setLevels(t, pool, f, spaceOwner)["A.members"]; got != lvOwner {
			t.Errorf("the space owner on A.members without explicit owners: %q, want owner", got)
		}
	})
}

func rowsOf(t *testing.T, pool *pgxpool.Pool, project, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`, project, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

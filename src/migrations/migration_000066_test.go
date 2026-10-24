package migrations

// migration_000066_test.go — sk-674.18, T006 (K-1, D-1, INV-2), the gate of
// the access relation. Reference — plan T006 and K-1, written before the
// migration:
//
//   - up/down on a database without the fixture and again on a seeded one:
//     down removes both functions, up brings them back, data is untouched;
//   - a sample query that JOINs the relation is planned without a Function
//     Scan — the SQL function is inlined into the caller's query (K-1);
//   - the relation reproduces the OLD rule (internal admin ∨ a membership
//     row, with the row's role) on the T003 corpus (testsupport.SeedTenancy):
//     for every actor × project its level equals m66OldRule, a literal table
//     written from the fixture's legend (matrix row 13); the point function
//     answers the same level, 'forbidden' for an existing project out of
//     reach and 'not_found' for a missing one. Beyond the fixture, the whole
//     relation over every project of the database is the same rule.
//
// Deactivation is outside the old rule and outside the relation (T036 adds
// it): the deactivated actor keeps the reach of its rows. (Until T008 the
// relation was compared here with the point check, projects.
// EnsureProjectVisible; since T008 that check is a wrapper over this
// relation, so the comparison would have checked the relation against
// itself — the literal table replaces it.)

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// m66OldRule is the expected level of every actor on every fixture project
// under the old rule, written from the T003 legend (testsupport/tenancy.go):
// the owner of a project (its creator) is 'owner', any other membership row
// is 'member', an internal admin (users.role = 'admin', not external) is
// 'admin' on every project — the "Personal" spaces of others included (000073
// narrows that) — and a project absent from an actor's row is out of reach.
// Since T017 the fixture seeds real spaces and the space owner holds no row on
// A.members (he is its implicit owner under the new rule), so under the old
// rule A.members is out of his reach.
//
// The rule is the one of migration 000066, so the cells are asserted on the
// schema at version 66, after the seeded round trip (000073 replaces the
// body).
var m66OldRule = map[testsupport.Actor]map[string]string{
	testsupport.ActorOnlyA: {"A.space": "member", "A.members": "member", "L:onlyA": "owner"},
	testsupport.ActorOnlyB: {"B.space": "owner", "B.members": "owner", "L:onlyB": "owner"},
	testsupport.ActorBoth: {"A.space": "member", "A.members": "member",
		"B.space": "member", "B.members": "member", "L:both": "owner"},
	testsupport.ActorProjectOwner:         {"A.space": "member", "A.members": "owner", "L:projectOwner": "owner"},
	testsupport.ActorSpaceOwner:           {"A.space": "owner", "L:spaceOwner": "owner"},
	testsupport.ActorParticipantNoMembers: {"A.space": "member", "L:participantNoMembers": "owner"},
	testsupport.ActorExternal:             {"A.members": "member"},
	testsupport.ActorDeactivated:          {"A.space": "member", "A.members": "member", "L:deactivated": "owner"},
	testsupport.ActorOutsider:             {"L:outsider": "owner"},
	// adminA and admin0: 'admin' on every project, see m66Expected.
}

// m66Expected is m66OldRule's cell for actor a on the project keyed key.
func m66Expected(a testsupport.Actor, key string) string {
	if a == testsupport.ActorAdminA || a == testsupport.ActorAdmin0 {
		return "admin"
	}
	return m66OldRule[a][key]
}

// m66Functions reports which of the migration's two functions exist.
func m66Functions(t *testing.T, pool *pgxpool.Pool, stage string) (set, point bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT to_regprocedure('skein_project_access_set(uuid)') IS NOT NULL,
       to_regprocedure('skein_project_access(uuid, uuid)') IS NOT NULL`).Scan(&set, &point); err != nil {
		t.Fatalf("%s: read functions: %v", stage, err)
	}
	return set, point
}

func m66Migrate(t *testing.T, m interface{ Migrate(uint) error }, pool *pgxpool.Pool, stage string, to uint, want bool) {
	t.Helper()
	if err := m.Migrate(to); err != nil {
		t.Fatalf("%s: Migrate(%d): %v", stage, to, err)
	}
	set, point := m66Functions(t, pool, stage)
	if set != want || point != want {
		t.Fatalf("%s: skein_project_access_set present %v, skein_project_access present %v, want both %v", stage, set, point, want)
	}
}

// m66Relation reads the caller's level on project from the relation; "" when
// the relation has no row for it.
func m66Relation(t *testing.T, pool *pgxpool.Pool, caller, project uuid.UUID) string {
	t.Helper()
	var access string
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*), COALESCE(max(access), '')
  FROM skein_project_access_set($1) WHERE project_id = $2`, caller, project).Scan(&n, &access); err != nil {
		t.Fatalf("relation for %s on %s: %v", caller, project, err)
	}
	if n > 1 {
		t.Fatalf("relation for %s has %d rows for project %s, want at most one", caller, n, project)
	}
	return access
}

func m66Point(t *testing.T, pool *pgxpool.Pool, caller, project uuid.UUID) string {
	t.Helper()
	var access string
	if err := pool.QueryRow(context.Background(), `SELECT skein_project_access($1, $2)`, caller, project).Scan(&access); err != nil {
		t.Fatalf("point access for %s on %s: %v", caller, project, err)
	}
	return access
}

// m66HasFunctionScan walks an EXPLAIN (FORMAT JSON) plan for a Function Scan
// node.
func m66HasFunctionScan(node map[string]any) bool {
	if node["Node Type"] == "Function Scan" {
		return true
	}
	children, _ := node["Plans"].([]any)
	for _, c := range children {
		if child, ok := c.(map[string]any); ok && m66HasFunctionScan(child) {
			return true
		}
	}
	return false
}

// m66DropTransitionSpaces removes what the transition re-run by the round
// trip created for the fixture — its projects' common space and the
// fixture users' "Personal" spaces — so the fixture's own cleanup can delete its
// projects and users (spaces.owner_id and space_members are ON DELETE
// RESTRICT). It runs before the fixture's cleanup.
func m66DropTransitionSpaces(t *testing.T, pool *pgxpool.Pool, f *testsupport.Tenancy) {
	t.Helper()
	var projects, users []uuid.UUID
	for _, p := range f.Projects() {
		projects = append(projects, p.ID)
	}
	for _, a := range testsupport.TenancyActors {
		users = append(users, f.User(t, a).ID)
	}
	migDropTransitionSpaces(t, pool, users, projects)
}

// migDropTransitionSpaces removes, for a migration test that seeded on an
// old version and migrated up through the transition (000071), what the
// transition made for its seeds — the projects leave their space, and the
// seeds' participations and the spaces they own (common, "Personal") with
// no project left go — so the seeds' own cleanups can delete projects and
// users. A no-op below the spaces schema.
func migDropTransitionSpaces(t *testing.T, pool *pgxpool.Pool, users, projects []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('spaces') IS NOT NULL`).Scan(&present); err != nil {
		t.Errorf("probing spaces: %v", err)
		return
	}
	if !present {
		return
	}
	// projects.space_id is NOT NULL since 000080 (T024): the projects move
	// into the shared test space, so the transition's spaces empty out; the
	// projects themselves go with their own cleanups.
	if _, err := pool.Exec(ctx, `UPDATE projects SET space_id = $2 WHERE id = ANY($1)`,
		projects, testsupport.TestSpace(t, pool)); err != nil {
		t.Errorf("taking the projects out of spaces: %v", err)
	}
	for _, q := range []string{
		`DELETE FROM space_members WHERE user_id = ANY($1)`,
		`DELETE FROM space_members sm USING spaces s
		  WHERE sm.space_id = s.id AND s.owner_id = ANY($1)
		    AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.space_id = s.id)`,
		`DELETE FROM spaces s WHERE s.owner_id = ANY($1)
		    AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.space_id = s.id)`,
	} {
		if _, err := pool.Exec(ctx, q, users); err != nil {
			t.Errorf("dropping the transition's spaces: %v", err)
		}
	}
}

// TestMigration000066_AccessRelation — see the file header.
func TestMigration000066_AccessRelation(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	ctx := context.Background()

	// --- up/down before the fixture exists ---
	m66Migrate(t, m, pool, "down, no fixture", 65, false)
	m66Migrate(t, m, pool, "up, no fixture", 66, true)

	// The fixture is seeded on the head schema — projects live in spaces
	// since T016 — and the round trip below takes it down through the
	// spaces migrations and back up through the transition (000071).
	m59Up(t, m, "head before the fixture")
	f := testsupport.SeedTenancy(t, pool)
	t.Cleanup(func() { m66DropTransitionSpaces(t, pool, f) })

	// --- up/down on the seeded database ---
	m66Migrate(t, m, pool, "down, seeded", 65, false)
	m66Migrate(t, m, pool, "up, seeded", 66, true)
	// The assertions below run at version 66 — the rule this migration
	// introduced; the migrator's cleanup returns the suite to head.
	if got := m59Count(t, pool, "fixture after round trip",
		`SELECT count(*) FROM project_members WHERE project_id = $1`, f.ASpace.ID); got == 0 {
		t.Fatalf("fixture after round trip: A.space has no members — down/up touched data")
	}

	t.Run("plan inlines the relation", func(t *testing.T) {
		const sample = `
SELECT i.id, va.access
  FROM issues i
  JOIN skein_project_access_set($1) va ON va.project_id = i.project_id
 WHERE i.status <> 'closed'`
		for _, a := range []testsupport.Actor{testsupport.ActorOnlyA, testsupport.ActorAdminA} {
			var raw []byte
			if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sample, f.User(t, a).ID).Scan(&raw); err != nil {
				t.Fatalf("%s: explain: %v", a, err)
			}
			var plans []struct {
				Plan map[string]any `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("%s: decode plan (%d plans): %v\n%s", a, len(plans), err, raw)
			}
			if m66HasFunctionScan(plans[0].Plan) {
				t.Errorf("%s: the sample JOIN is planned with a Function Scan — the relation is not inlined:\n%s", a, raw)
			}
		}
	})

	missing := uuid.New()
	for _, a := range testsupport.TenancyActors {
		u := f.User(t, a)
		unrestricted := a == testsupport.ActorAdminA || a == testsupport.ActorAdmin0

		t.Run(string(a), func(t *testing.T) {
			for _, p := range f.Projects() {
				rel := m66Relation(t, pool, u.ID, p.ID)
				if want := m66Expected(a, p.Key()); rel != want {
					t.Errorf("%s: relation level %q, want %q (old rule)", p.Key(), rel, want)
				}

				// The point function — the same level, else 'forbidden'.
				wantPoint := rel
				if wantPoint == "" {
					wantPoint = "forbidden"
				}
				if got := m66Point(t, pool, u.ID, p.ID); got != wantPoint {
					t.Errorf("%s: skein_project_access %q, want %q", p.Key(), got, wantPoint)
				}
			}

			// Beyond the fixture: the whole relation, over every project of
			// the database, is "every project as admin" for the internal
			// admin and exactly the membership rows (with their role) for
			// everyone else.
			var want, got, mismatched int
			if unrestricted {
				want = m59Count(t, pool, "projects", `SELECT count(*) FROM projects`)
				got = m59Count(t, pool, "relation", `SELECT count(*) FROM skein_project_access_set($1)`, u.ID)
				mismatched = m59Count(t, pool, "relation levels",
					`SELECT count(*) FROM skein_project_access_set($1) WHERE access <> 'admin'`, u.ID)
			} else {
				want = m59Count(t, pool, "members", `SELECT count(*) FROM project_members WHERE user_id = $1`, u.ID)
				got = m59Count(t, pool, "relation", `SELECT count(*) FROM skein_project_access_set($1)`, u.ID)
				mismatched = m59Count(t, pool, "relation levels", `
SELECT count(*) FROM skein_project_access_set($1) va
 WHERE NOT EXISTS (SELECT 1 FROM project_members pm
                    WHERE pm.user_id = $1 AND pm.project_id = va.project_id AND pm.role = va.access)`, u.ID)
			}
			if got != want || mismatched != 0 {
				t.Errorf("whole relation: %d rows (%d with a level the rule does not give), want %d", got, mismatched, want)
			}

			if got := m66Point(t, pool, u.ID, missing); got != "not_found" {
				t.Errorf("missing project: skein_project_access %q, want not_found", got)
			}
		})
	}

	t.Run("unknown caller", func(t *testing.T) {
		stranger := uuid.New()
		if n := m59Count(t, pool, "relation", `SELECT count(*) FROM skein_project_access_set($1)`, stranger); n != 0 {
			t.Errorf("relation of an unknown id has %d rows, want none", n)
		}
		if got := m66Point(t, pool, stranger, f.ASpace.ID); got != "forbidden" {
			t.Errorf("existing project: %q, want forbidden", got)
		}
		if got := m66Point(t, pool, stranger, missing); got != "not_found" {
			t.Errorf("missing project: %q, want not_found", got)
		}
	})
}

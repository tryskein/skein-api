package migrations

// migration_000071_spaces_transition_test.go — sk-674.18, T016: the
// transition to spaces (K-23 step 2, US-2, US-21) on a database seeded the
// way an installation looks before it. Expectations are written from the
// spec (SC-004, INV-19, US-2 "Expected", INV-4, US-6, INV-6, INV-8, INV-22,
// US-21 "Transition"), not from the migration:
//
//   - row 37: differential — for every pair (ACTIVE user, project) of the
//     seed the visibility before the transition (the old relation, T006)
//     equals the visibility after it (the new relation, T017, at head); a
//     deactivated user sees nothing after it; the sets of issue ids,
//     prefixes and aliases are equal; every project is in exactly one space.
//     The reference is the spec, not the old relation: INV-1 gives access to
//     an active user only, and the old relation deliberately ignored
//     deactivation — INV-19 and SC-004 ("no wider") allow that narrowing
//     (lead's decision (A) on row 37, recorded in sk-674.18); the
//     deactivated keep their memberships and spaces (rows 39, 40);
//   - row 38: a common space (the default one, with its Russian name) is created, its owner the earliest
//     ACTIVE admin by created_at — an earlier deactivated admin is skipped —
//     and by the new rule he is `owner` of every project of the common space
//     without a membership row; another active admin has the owner's
//     actions too — by his admin level without a role (US-6 as changed by
//     the owner 2026-10-17, sk-741; before it he was refused), by his level
//     owner where his explicit `owner` row was carried over;
//   - row 39: every project, archived included, is in the common space with
//     visibility members; every non-external user, deactivated included, is a
//     participant of the common space and the owner and only participant of
//     his own personal space; the external user is in no space;
//   - row 40: every membership row survives with its role, as "manual" and
//     not "from a space"; email_confirmed_at is set for every user;
//   - row 41: whoever picked another user's theme gets his own copy (same
//     name and values) and his choice moves to it; a name taken by his own
//     theme gets a numeric suffix; two users who picked one theme get one
//     copy each.
//
// TestMigration000071_DownRestoresTheRenamedThemes: the
// down right after the transition succeeds on the same seed and gives the
// themes their names before it.
//
// Rows 37 and 38 read the relation, so their «after» is taken at head (the
// new relation is 000073 and later); rows 39–41 read the data right after
// 000071.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

const (
	m71Before = 70 // the schema an installation upgrades from: spaces tables, no spaces data
	m71After  = 71
)

type m71Seed struct {
	admDeact, admA, admB, user, userDeact, ext uuid.UUID
	active, archived                           uuid.UUID
	foreignTheme, ownTheme                     uuid.UUID
	// two themes of admB whose names differ only by case
	// (legal before the transition: themes_name_key is case-sensitive).
	duskUpper, duskLower uuid.UUID
	// «dusk 2» of another author (user).
	otherDusk2 uuid.UUID
	// an admin deleted before the transition, created
	// before every other admin.
	admDeleted uuid.UUID
}

func (s m71Seed) users() []uuid.UUID {
	return []uuid.UUID{s.admDeact, s.admA, s.admB, s.user, s.userDeact, s.ext}
}

func (s m71Seed) activeUsers() []uuid.UUID {
	return []uuid.UUID{s.admA, s.admB, s.user, s.ext}
}

func (s m71Seed) deactivatedUsers() []uuid.UUID {
	return []uuid.UUID{s.admDeact, s.userDeact}
}

// m71User seeds a user with a fixed creation date, so the earliest admin is
// known whatever else the shared database holds.
func m71User(t *testing.T, pool *pgxpool.Pool, role string, created time.Time, deactivated, external bool) uuid.UUID {
	t.Helper()
	id, _ := testsupport.SeedUser(t, pool, role)
	var deact *time.Time
	if deactivated {
		now := time.Now().UTC()
		deact = &now
	}
	m59Exec(t, pool, "shape user", `
UPDATE users SET created_at = $2, deactivated_at = $3, is_external = $4 WHERE id = $1`,
		id, created, deact, external)
	return id
}

func m71Project(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, archived bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	name := "m71_" + id.String()[:8]
	var archivedAt *time.Time
	if archived {
		now := time.Now().UTC()
		archivedAt = &now
	}
	m59Exec(t, pool, "seed project", `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos, archived_at,
                      created_by, created_at, updated_at, metadata)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, $4, $5, now(), now(), '{}'::jsonb)`,
		id, name, name, archivedAt, owner)
	return id
}

func m71Member(t *testing.T, pool *pgxpool.Pool, project, user uuid.UUID, role string) {
	t.Helper()
	m59Exec(t, pool, "seed membership", `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, $3, now(), $2)`, project, user, role)
}

// m71Issue seeds one issue of project with the id <prefix>-1 and an alias
// of the project — the identities row 37 compares.
func m71Issue(t *testing.T, pool *pgxpool.Pool, project, author uuid.UUID) {
	t.Helper()
	m59Exec(t, pool, "seed issue", `
INSERT INTO issues (id, project_id, version, title, description, type, priority, status, pinned, source,
                    design, acceptance_criteria, notes, created_by, created_at, updated_at, metadata)
SELECT p.id_prefix || '-1', p.id, 1, 'm71', '', 'task', 2, 'open', false, 'human',
       '', '', '', $2, now(), now(), '{}'::jsonb
  FROM projects p WHERE p.id = $1`, project, author)
	m59Exec(t, pool, "seed alias", `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind)
SELECT 'a' || p.id_prefix, p.id, $2, 'human' FROM projects p WHERE p.id = $1`, project, author)
}

// m71Visibility is the set of pairs (user of users, seed project) in which
// the user sees the project by the relation of the current schema version.
func m71Visibility(t *testing.T, pool *pgxpool.Pool, s m71Seed, users []uuid.UUID) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), `
SELECT coalesce(string_agg(u::text || '/' || p::text, ',' ORDER BY u, p), '')
  FROM unnest($1::uuid[]) u CROSS JOIN unnest($2::uuid[]) p
 WHERE EXISTS (SELECT 1 FROM skein_project_access_set(u) a WHERE a.project_id = p)`,
		users, []uuid.UUID{s.active, s.archived}).Scan(&out); err != nil {
		t.Fatalf("reading visibility: %v", err)
	}
	return out
}

// m71Identities is the seed projects' issue ids, prefixes and aliases.
func m71Identities(t *testing.T, pool *pgxpool.Pool, s m71Seed) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT coalesce(string_agg(id, ',' ORDER BY id), '') FROM issues WHERE project_id = ANY($1))
    || ' | ' || (SELECT string_agg(id_prefix, ',' ORDER BY id_prefix) FROM projects WHERE id = ANY($1))
    || ' | ' || (SELECT coalesce(string_agg(alias, ',' ORDER BY alias), '') FROM project_aliases WHERE project_id = ANY($1))`,
		[]uuid.UUID{s.active, s.archived}).Scan(&out); err != nil {
		t.Fatalf("reading identities: %v", err)
	}
	return out
}

func m71Theme(t *testing.T, pool *pgxpool.Pool, author uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
INSERT INTO themes (name, base, variables, created_by, updated_by)
VALUES ($1, 'dark', '{"--accent":"#123456"}'::jsonb, $2, $2) RETURNING id`, name, author).Scan(&id); err != nil {
		t.Fatalf("seed theme: %v", err)
	}
	return id
}

func m71Pick(t *testing.T, pool *pgxpool.Pool, user, theme uuid.UUID) {
	t.Helper()
	m59Exec(t, pool, "pick theme",
		`UPDATE users SET theme_mode = 'custom', custom_theme_id = $2 WHERE id = $1`, user, theme)
}

// m71Cleanup removes what the transition made for the seed and the seed's
// projects and themes, before the users' own cleanup deletes the users.
func m71Cleanup(t *testing.T, pool *pgxpool.Pool, s m71Seed) {
	t.Helper()
	ctx := context.Background()
	users := append(s.users(), s.admDeleted)
	projects := []uuid.UUID{s.active, s.archived}
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM issues WHERE project_id = ANY($1)`, projects},
		{`DELETE FROM projects WHERE id = ANY($1)`, projects},
		{`DELETE FROM space_members WHERE user_id = ANY($1)`, users},
		{`DELETE FROM space_members sm USING spaces sp
		   WHERE sm.space_id = sp.id AND sp.owner_id = ANY($1)
		     AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.space_id = sp.id)`, users},
		{`DELETE FROM spaces sp WHERE sp.owner_id = ANY($1)
		     AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.space_id = sp.id)`, users},
		{`UPDATE users SET custom_theme_id = NULL, theme_mode = 'builtin' WHERE id = ANY($1)`, users},
		{`DELETE FROM themes WHERE created_by = ANY($1)`, users},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.arg); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
}

// m71SeedInstallation seeds, at schema 70, the installation both
// transition tests run on and registers its cleanup.
func m71SeedInstallation(t *testing.T, pool *pgxpool.Pool) m71Seed {
	t.Helper()
	base := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	var s m71Seed
	s.admDeact = m71User(t, pool, "admin", base, true, false)
	s.admA = m71User(t, pool, "admin", base.Add(24*time.Hour), false, false)
	s.admB = m71User(t, pool, "admin", base.Add(48*time.Hour), false, false)
	s.user = m71User(t, pool, "user", base.Add(72*time.Hour), false, false)
	s.userDeact = m71User(t, pool, "user", base.Add(96*time.Hour), true, false)
	s.ext = m71User(t, pool, "user", base.Add(120*time.Hour), false, true)
	s.admDeleted = m71User(t, pool, "admin", base.Add(-24*time.Hour), false, false)
	m59Exec(t, pool, "delete admin", `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8) WHERE id = $1`, s.admDeleted)

	s.active = m71Project(t, pool, s.user, false)
	s.archived = m71Project(t, pool, s.admB, true)
	m71Member(t, pool, s.active, s.user, "owner")
	m71Member(t, pool, s.active, s.ext, "member")
	m71Member(t, pool, s.active, s.userDeact, "member")
	m71Member(t, pool, s.archived, s.admB, "owner")
	m71Member(t, pool, s.archived, s.user, "member")
	m71Issue(t, pool, s.active, s.user)
	m71Issue(t, pool, s.archived, s.admB)

	// Themes: B's «Ocean» picked by user (who owns «ocean» himself) and by
	// userDeact; admB keeps his own.
	s.foreignTheme = m71Theme(t, pool, s.admB, "Ocean")
	s.ownTheme = m71Theme(t, pool, s.user, "ocean")
	m71Pick(t, pool, s.user, s.foreignTheme)
	m71Pick(t, pool, s.userDeact, s.foreignTheme)
	m71Pick(t, pool, s.admB, s.foreignTheme)
	s.duskUpper = m71Theme(t, pool, s.admB, "Dusk")
	s.duskLower = m71Theme(t, pool, s.admB, "dusk")
	m71Pick(t, pool, s.admA, s.duskLower) // a foreign pick of the later one
	// another author's theme already holds the suffix
	// the transition gives the later «dusk» of admB — legal before and
	// after it, and in the way of the global uniqueness the down returns.
	s.otherDusk2 = m71Theme(t, pool, s.user, "dusk 2")
	t.Cleanup(func() { m71Cleanup(t, pool, s) })
	return s
}

func TestMigration000071_SpacesTransition(t *testing.T) {
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	if err := m.Migrate(m71Before); err != nil {
		t.Fatalf("Migrate(%d): %v", m71Before, err)
	}

	s := m71SeedInstallation(t, pool)

	membersBefore := m71Memberships(t, pool, s)
	visibleBefore := m71Visibility(t, pool, s, s.activeUsers())
	identitiesBefore := m71Identities(t, pool, s)

	if err := m.Migrate(m71After); err != nil {
		t.Fatalf("Migrate(%d) — the transition: %v", m71After, err)
	}

	// Row 38 (data part): the common space and its owner.
	var common, owner uuid.UUID
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT id, owner_id, name FROM spaces WHERE kind = 'common'`).Scan(&common, &owner, &name); err != nil {
		t.Fatalf("reading the common space: %v", err)
	}
	if owner != s.admA {
		t.Errorf("common space owner = %s, want the earliest ACTIVE admin %s (the deactivated %s and the deleted %s are earlier)",
			owner, s.admA, s.admDeact, s.admDeleted)
	}
	if name != "Common" {
		t.Errorf("common space name = %q, want the default name", name)
	}

	// Row 39: projects, participants, personal spaces.
	for _, p := range []uuid.UUID{s.active, s.archived} {
		if n := m59Count(t, pool, "project placed", `
SELECT count(*) FROM projects WHERE id = $1 AND space_id = $2 AND visibility = 'members'`, p, common); n != 1 {
			t.Errorf("project %s is not in the common space with visibility members", p)
		}
	}
	wantCommon := map[uuid.UUID]string{
		s.admA: "owner", s.admB: "member", s.admDeact: "member", s.user: "member", s.userDeact: "member",
	}
	for _, u := range s.users() {
		var role string
		err := pool.QueryRow(context.Background(),
			`SELECT role FROM space_members WHERE space_id = $1 AND user_id = $2`, common, u).Scan(&role)
		want, in := wantCommon[u]
		switch {
		case in && err != nil:
			t.Errorf("user %s is not a participant of the common space: %v", u, err)
		case in && role != want:
			t.Errorf("user %s in the common space as %q, want %q", u, role, want)
		case !in && err == nil:
			t.Errorf("external user %s became a participant of the common space", u)
		}

		personal := m59Count(t, pool, "personal", `
SELECT count(*) FROM spaces sp
 WHERE sp.kind = 'personal' AND sp.owner_id = $1
   AND (SELECT count(*) FROM space_members sm WHERE sm.space_id = sp.id) = 1
   AND EXISTS (SELECT 1 FROM space_members sm
                WHERE sm.space_id = sp.id AND sm.user_id = $1 AND sm.role = 'owner')`, u)
		if u == s.ext {
			if n := m59Count(t, pool, "external spaces",
				`SELECT count(*) FROM space_members WHERE user_id = $1`, u); n != 0 || personal != 0 {
				t.Errorf("external user is in %d spaces and owns %d personal spaces, want none", n, personal)
			}
			continue
		}
		if personal != 1 {
			t.Errorf("user %s owns %d personal spaces with himself as the only participant, want 1", u, personal)
		}
	}

	// Row 40: memberships kept with their roles, as "manual"; email confirmed.
	if after := m71Memberships(t, pool, s); after != membersBefore {
		t.Errorf("project memberships changed:\n before %s\n after  %s", membersBefore, after)
	}
	if n := m59Count(t, pool, "sources", `
SELECT count(*) FROM project_members
 WHERE project_id = ANY($1) AND (NOT via_manual OR via_space)`, []uuid.UUID{s.active, s.archived}); n != 0 {
		t.Errorf("%d membership rows are not manual only", n)
	}
	if n := m59Count(t, pool, "email confirmed",
		`SELECT count(*) FROM users WHERE id = ANY($1) AND email_confirmed_at IS NULL`, s.users()); n != 0 {
		t.Errorf("%d existing users have no confirmed email", n)
	}

	// Row 41: personal copies of picked foreign themes.
	type pick struct {
		name, base, vars string
		author, created  uuid.UUID
		themeID          uuid.UUID
	}
	readPick := func(u uuid.UUID) pick {
		t.Helper()
		var p pick
		if err := pool.QueryRow(context.Background(), `
SELECT t.id, t.name, t.base, t.variables::text, t.author_id, t.created_by
  FROM users u JOIN themes t ON t.id = u.custom_theme_id WHERE u.id = $1`, u).
			Scan(&p.themeID, &p.name, &p.base, &p.vars, &p.author, &p.created); err != nil {
			t.Fatalf("reading the picked theme of %s: %v", u, err)
		}
		return p
	}
	orig := readPick(s.admB)
	if orig.themeID != s.foreignTheme || orig.author != s.admB {
		t.Errorf("the author's own pick moved: %+v", orig)
	}
	for u, wantName := range map[uuid.UUID]string{s.user: "Ocean 2", s.userDeact: "Ocean"} {
		got := readPick(u)
		if got.themeID == s.foreignTheme || got.author != u {
			t.Errorf("user %s still picks a theme authored by %s, want his own copy", u, got.author)
			continue
		}
		if got.name != wantName || got.base != orig.base || got.vars != orig.vars {
			t.Errorf("user %s copy = %q/%s/%s, want %q/%s/%s", u, got.name, got.base, got.vars,
				wantName, orig.base, orig.vars)
		}
	}
	if n := m59Count(t, pool, "own theme kept",
		`SELECT count(*) FROM themes WHERE id = $1 AND name = 'ocean' AND author_id = $2`, s.ownTheme, s.user); n != 1 {
		t.Error("the user's own theme «ocean» was changed")
	}

	// two themes of one author differing only by case
	// survive the transition (the author-scoped uniqueness is
	// case-insensitive, US-21): the earlier keeps its name, the later gets
	// a numeric suffix, ids and values are kept, and a pick of the later
	// one follows it.
	var upperName, lowerName string
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT name FROM themes WHERE id = $1), (SELECT name FROM themes WHERE id = $2)`,
		s.duskUpper, s.duskLower).Scan(&upperName, &lowerName); err != nil {
		t.Fatalf("reading the case-variant themes: %v", err)
	}
	if upperName != "Dusk" || lowerName != "dusk 2" {
		t.Errorf("case-variant themes of one author after the transition: %q, %q; want \"Dusk\", \"dusk 2\"", upperName, lowerName)
	}
	if got := readPick(s.admA); got.name != "dusk 2" || got.author != s.admA {
		t.Errorf("admA's pick of the renamed theme: %q by %s, want his own copy \"dusk 2\"", got.name, got.author)
	}

	// Rows 37 and 38 (the level part) at head, under the new relation with
	// the instance admin enabled (a transition without it refuses to start).
	m59Up(t, m, "head")
	ctx := context.Background()
	var adminEnabled bool
	if err := pool.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&adminEnabled); err != nil {
		t.Fatalf("reading the admin flag: %v", err)
	}
	if !adminEnabled {
		m59Exec(t, pool, "admin flag on", `UPDATE instance_settings SET admin_enabled = true WHERE id = 1`)
		t.Cleanup(func() {
			m59Exec(t, pool, "admin flag back", `UPDATE instance_settings SET admin_enabled = false WHERE id = 1`)
		})
	}

	// Row 37.
	if after := m71Visibility(t, pool, s, s.activeUsers()); after != visibleBefore {
		t.Errorf("visibility of the active users changed by the transition:\n before %s\n after  %s", visibleBefore, after)
	}
	if after := m71Visibility(t, pool, s, s.deactivatedUsers()); after != "" {
		t.Errorf("a deactivated user sees projects after the transition (INV-1 — active only): %s", after)
	}
	if after := m71Identities(t, pool, s); after != identitiesBefore {
		t.Errorf("issue ids, prefixes or aliases changed:\n before %s\n after  %s", identitiesBefore, after)
	}
	if n := m59Count(t, pool, "one space each",
		`SELECT count(*) FROM projects WHERE id = ANY($1) AND space_id IS NOT NULL`, []uuid.UUID{s.active, s.archived}); n != 2 {
		t.Errorf("%d of the 2 seed projects are in a space, want both in exactly one", n)
	}

	// Row 38: the owner of the common space is the implicit owner.
	for _, p := range []uuid.UUID{s.active, s.archived} {
		if n := m59Count(t, pool, "no owner row",
			`SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`, p, s.admA); n != 0 {
			t.Fatalf("the seed gave the common space owner a membership row in %s", p)
		}
		if level, err := authz.Can(ctx, pool, authz.ForUser(s.admA), authz.ActionManageMembers, p); err != nil || level != authz.LevelOwner {
			t.Errorf("owner of the common space on project %s: level %q, err %v; want owner, allowed", p, level, err)
		}
	}
	// Another admin: the owner's actions without a role, by his admin level
	// (sk-741)…
	if level, err := authz.Can(ctx, pool, authz.ForUser(s.admB), authz.ActionManageMembers, s.active); err != nil || level != authz.LevelAdmin {
		t.Errorf("admin without a role on a project of the common space: level %q, err %v; want admin, allowed", level, err)
	}
	// …and keeps it where his explicit owner row was carried over.
	if level, err := authz.Can(ctx, pool, authz.ForUser(s.admB), authz.ActionManageMembers, s.archived); err != nil || level != authz.LevelOwner {
		t.Errorf("admin with a carried-over owner row: level %q, err %v; want owner, allowed", level, err)
	}
}

// TestMigration000071_DownRestoresTheRenamedThemes —
// the down right after the transition, with nothing written in between,
// returns schema 70 on the very data schema 70 held. The later «dusk» of
// admB was renamed «dusk 2», the name user's own theme already had (legal
// under the author-scoped uniqueness); the global uniqueness the down
// returns admits it only if the renamed theme gets its original name back.
func TestMigration000071_DownRestoresTheRenamedThemes(t *testing.T) {
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	if err := m.Migrate(m71Before); err != nil {
		t.Fatalf("Migrate(%d): %v", m71Before, err)
	}
	s := m71SeedInstallation(t, pool)

	if err := m.Migrate(m71After); err != nil {
		t.Fatalf("Migrate(%d) — the transition: %v", m71After, err)
	}
	if n := m59Count(t, pool, "the suffix collides",
		`SELECT count(*) FROM themes WHERE id = ANY($1) AND name = 'dusk 2'`,
		[]uuid.UUID{s.duskLower, s.otherDusk2}); n != 2 {
		t.Fatalf("%d of admB's later «dusk» and user's «dusk 2» are named «dusk 2» after the "+
			"transition, want both — the seed no longer reaches the collision", n)
	}

	if err := m.Migrate(m71Before); err != nil {
		t.Fatalf("Migrate(%d) — the down right after the transition: %v", m71Before, err)
	}

	names := map[uuid.UUID]string{}
	rows, err := pool.Query(context.Background(), `SELECT id, name FROM themes WHERE id = ANY($1)`,
		[]uuid.UUID{s.duskUpper, s.duskLower, s.otherDusk2, s.foreignTheme, s.ownTheme})
	if err != nil {
		t.Fatalf("reading the themes after the down: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("reading a theme: %v", err)
		}
		names[id] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the themes after the down: %v", err)
	}
	for id, want := range map[uuid.UUID]string{
		s.duskUpper: "Dusk", s.duskLower: "dusk", s.otherDusk2: "dusk 2",
		s.foreignTheme: "Ocean", s.ownTheme: "ocean",
	} {
		if names[id] != want {
			t.Errorf("theme %s after the down is %q, want its name before the transition %q", id, names[id], want)
		}
	}
	// The copies are gone and the picks are back on the originals.
	for u, want := range map[uuid.UUID]uuid.UUID{
		s.admA: s.duskLower, s.user: s.foreignTheme, s.userDeact: s.foreignTheme, s.admB: s.foreignTheme,
	} {
		var got uuid.UUID
		if err := pool.QueryRow(context.Background(),
			`SELECT custom_theme_id FROM users WHERE id = $1`, u).Scan(&got); err != nil {
			t.Fatalf("reading the pick of %s: %v", u, err)
		}
		if got != want {
			t.Errorf("user %s picks %s after the down, want the original %s", u, got, want)
		}
	}
}

// m71Memberships is the seed's membership rows as text: project, user, role.
func m71Memberships(t *testing.T, pool *pgxpool.Pool, s m71Seed) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), `
SELECT coalesce(string_agg(project_id::text || '/' || user_id::text || '/' || role, ',' ORDER BY project_id, user_id), '')
  FROM project_members WHERE project_id = ANY($1)`, []uuid.UUID{s.active, s.archived}).Scan(&out); err != nil {
		t.Fatalf("reading memberships: %v", err)
	}
	return out
}

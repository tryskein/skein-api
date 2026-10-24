package projects

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// seedNamedProject inserts a project with a caller-controlled name and
// id_prefix (the generic seedProjectWithRepos helper derives both from a
// prefix, which makes a clean substring assertion impossible). The caller
// is added as an owner member so the permission filter lets them see it.
func seedNamedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, name, idPrefix string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, idPrefix, owner, seedSpaceID); err != nil {
		t.Fatalf("seed named project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	seedListMember(t, pool, id, owner)
	return id
}

// TestResolve_SubstringMatch is the sk-60 regression: a short query that
// is only a middle SUBSTRING of a longer project name (and of its
// id_prefix) must surface the project. The old name-only trigram step
// scored "infra" against "orion-net-infra-mgmt" at ~0.15 (< 0.5 gate) and
// returned zero candidates; the lexical path catches it via the
// `LOWER(name) LIKE '%infra%'` clause.
func TestResolve_SubstringMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)

	// Unique token so we never collide with other fixtures, embedded in
	// the MIDDLE of the name (not a prefix) so only the substring clause
	// can catch it.
	tok := "infra" + strings.ReplaceAll(uuid.New().String()[:6], "-", "")
	name := "orion-net-" + tok + "-mgmt"
	idPrefix := "net-" + tok
	pid := seedNamedProject(t, pool, owner, name, idPrefix)

	store := NewStore(pool)
	res, err := store.Resolve(context.Background(), tok, owner)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if !resolveHits(res, pid) {
		t.Fatalf("substring query %q did not surface project %s (name=%q) as match or candidate; match=%v candidates=%d",
			tok, pid, name, res.Match, len(res.Candidates))
	}
}

// TestResolve_ExactPathsNoRegress confirms the exact short-circuits still
// auto-return a Match (not just a candidate) for name / id_prefix / alias.
func TestResolve_ExactPathsNoRegress(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)

	uniq := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	name := "exactname" + uniq
	idPrefix := "expfx" + uniq
	alias := "exalias" + uniq
	pid := seedNamedProject(t, pool, owner, name, idPrefix)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())`, alias, pid, owner); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	store := NewStore(pool)
	for _, tc := range []struct {
		label string
		q     string
		via   string
	}{
		{"name", name, "name"},
		{"id_prefix", idPrefix, "id_prefix"},
		{"alias", alias, "alias"},
	} {
		res, err := store.Resolve(context.Background(), tc.q, owner)
		if err != nil {
			t.Fatalf("%s: resolve: %v", tc.label, err)
		}
		if res.Match == nil {
			t.Errorf("%s: expected auto-match for exact %q, got candidates=%d", tc.label, tc.q, len(res.Candidates))
			continue
		}
		if res.Match.ID != pid {
			t.Errorf("%s: matched %s, want %s", tc.label, res.Match.ID, pid)
		}
		if res.Match.MatchedVia != tc.via {
			t.Errorf("%s: MatchedVia = %q, want %q", tc.label, res.Match.MatchedVia, tc.via)
		}
	}
}

// TestResolve_SubstringPermissionFiltered confirms the lexical step never
// leaks a project the caller can't see. A stranger (no membership)
// querying the substring must get an empty result.
func TestResolve_SubstringPermissionFiltered(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	stranger := seedUserForRepoTest(t, pool)

	tok := "secret" + strings.ReplaceAll(uuid.New().String()[:6], "-", "")
	name := "zzz-" + tok + "-zzz"
	idPrefix := "z-" + tok
	_ = seedNamedProject(t, pool, owner, name, idPrefix)

	store := NewStore(pool)
	res, err := store.Resolve(context.Background(), tok, stranger)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Match != nil || len(res.Candidates) != 0 {
		t.Fatalf("stranger leaked project: match=%v candidates=%d", res.Match, len(res.Candidates))
	}
}

// resolveHits reports whether the given project id appears as the Match
// or anywhere in Candidates.
func resolveHits(res *ResolveResult, pid uuid.UUID) bool {
	if res.Match != nil && res.Match.ID == pid {
		return true
	}
	for _, c := range res.Candidates {
		if c.ID == pid {
			return true
		}
	}
	return false
}

// seedResolveProject is seedNamedProject in a given space, with repos; the
// owner is the project's owner member (and so a participant of the space).
func seedResolveProject(t *testing.T, pool *pgxpool.Pool, owner, space uuid.UUID, name, idPrefix string, repoURL string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	repos := "[]"
	if repoURL != "" {
		repos = `[{"id":"` + uuid.NewString() + `","name":"r","url":"` + repoURL + `"}]`
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', $4::jsonb, NULL, $5, now(), now(), '{}'::jsonb, $6)`,
		id, name, idPrefix, repos, owner, space); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	seedListMember(t, pool, id, owner)
	return id
}

// resolveWorld is one caller with two spaces of his and a stranger's space
// the caller is not in: the stranger's projects are inaccessible to him.
type resolveWorld struct {
	pool                 *pgxpool.Pool
	store                *Store
	caller, stranger     uuid.UUID
	spaceX, spaceY       uuid.UUID
	strangersSpace       uuid.UUID
	shortX, shortY, uniq string
}

func newResolveWorld(t *testing.T) *resolveWorld {
	pool := newTestPool(t)
	w := &resolveWorld{pool: pool, store: NewStore(pool),
		caller: seedUserForRepoTest(t, pool), stranger: seedUserForRepoTest(t, pool),
		uniq: strings.ReplaceAll(uuid.NewString()[:8], "-", "")}
	w.spaceX = testsupport.SeedSpace(t, pool, w.caller)
	w.spaceY = testsupport.SeedSpace(t, pool, w.caller)
	w.strangersSpace = testsupport.SeedSpace(t, pool, w.stranger)
	for _, s := range []struct {
		id  uuid.UUID
		out *string
	}{{w.spaceX, &w.shortX}, {w.spaceY, &w.shortY}} {
		if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, s.id).Scan(s.out); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *resolveWorld) resolve(t *testing.T, q string) *ResolveResult {
	t.Helper()
	res, err := w.store.Resolve(context.Background(), q, w.caller)
	if err != nil {
		t.Fatalf("resolve %q: %v", q, err)
	}
	return res
}

// TestResolve_ExactPathsInOrderAmongAccessible — sk-674.18 T024, matrix row
// 100 (INV-29, E-34, E-35). Reference — INV-29 of the spec: the exact paths
// id → prefix → alias → name among the projects accessible to the caller;
// the id, the prefix and the alias are unique on the installation and beat a
// name; an inaccessible same-named project neither takes part nor changes
// the answer.
func TestResolve_ExactPathsInOrderAmongAccessible(t *testing.T) {
	w := newResolveWorld(t)
	u := w.uniq

	// E-34: the string is the name of one accessible project and the prefix
	// (alias, id) of another.
	byPrefix := seedResolveProject(t, w.pool, w.caller, w.spaceY, "holder-prefix-"+u, "rp"+u, "")
	_ = seedResolveProject(t, w.pool, w.caller, w.spaceX, "rp"+u, "rn1"+u, "")
	byAlias := seedResolveProject(t, w.pool, w.caller, w.spaceY, "holder-alias-"+u, "ra"+u, "")
	if _, err := w.pool.Exec(context.Background(), `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())`, "al"+u, byAlias, w.caller); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	_ = seedResolveProject(t, w.pool, w.caller, w.spaceX, "al"+u, "rn2"+u, "")
	byID := seedResolveProject(t, w.pool, w.caller, w.spaceY, "holder-id-"+u, "ri"+u, "")
	_ = seedResolveProject(t, w.pool, w.caller, w.spaceX, byID.String(), "rn3"+u, "")
	// E-35: the name of an accessible project and of an inaccessible one.
	accessible := seedResolveProject(t, w.pool, w.caller, w.spaceX, "shared-name-"+u, "rs"+u, "")
	_ = seedResolveProject(t, w.pool, w.stranger, w.strangersSpace, "shared-name-"+u, "rt"+u, "")

	for _, c := range []struct {
		name, q, via string
		want         uuid.UUID
	}{
		{"prefix beats another's name", "rp" + u, "id_prefix", byPrefix},
		{"alias beats another's name", "al" + u, "alias", byAlias},
		{"id beats another's name", byID.String(), "id", byID},
		{"the accessible same-named project, the inaccessible ignored", "shared-name-" + u, "name", accessible},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := w.resolve(t, c.q)
			if res.Ambiguous || res.Match == nil {
				t.Fatalf("no single match: ambiguous=%v candidates=%+v", res.Ambiguous, res.Candidates)
			}
			if res.Match.ID != c.want || res.Match.MatchedVia != c.via {
				t.Errorf("matched %s via %s, want %s via %s", res.Match.ID, res.Match.MatchedVia, c.want, c.via)
			}
		})
	}
}

// TestResolve_AmbiguousNameIsFinal — sk-674.18 T024, matrix row 101
// (INV-29, E-30, E-36). Reference — INV-29: two accessible projects with
// one name are the final «ambiguous» refusal listing their prefixes and
// spaces — neither a repo URL nor a fuzzy match bypasses it, an
// inaccessible same-named project is not a candidate; without an exact hit
// the repo URL and the fuzzy forms work as before, among the accessible
// only.
func TestResolve_AmbiguousNameIsFinal(t *testing.T) {
	w := newResolveWorld(t)
	u := w.uniq

	t.Run("two accessible same-named projects", func(t *testing.T) {
		// The name is also a repo URL of a third accessible project and a
		// substring of a fourth's name: neither later form may win.
		name := "https://git.example/row101-" + u + ".git"
		inX := seedResolveProject(t, w.pool, w.caller, w.spaceX, name, "ax"+u, "")
		inY := seedResolveProject(t, w.pool, w.caller, w.spaceY, name, "ay"+u, "")
		_ = seedResolveProject(t, w.pool, w.stranger, w.strangersSpace, name, "az"+u, "")
		_ = seedResolveProject(t, w.pool, w.caller, w.spaceX, "repo-holder-"+u, "ar"+u, name)
		_ = seedResolveProject(t, w.pool, w.caller, w.spaceY, name+"-longer", "al"+u, "")

		res := w.resolve(t, name)
		if !res.Ambiguous || res.Match != nil {
			t.Fatalf("ambiguous=%v match=%+v, want the final ambiguous refusal", res.Ambiguous, res.Match)
		}
		got := map[uuid.UUID]ResolveMatch{}
		for _, c := range res.Candidates {
			got[c.ID] = c
		}
		want := map[uuid.UUID][2]string{inX: {"ax" + u, w.shortX}, inY: {"ay" + u, w.shortY}}
		if len(got) != len(want) {
			t.Fatalf("candidates %+v, want exactly the two accessible same-named projects", res.Candidates)
		}
		for id, pw := range want {
			c, ok := got[id]
			if !ok || c.IDPrefix != pw[0] || c.SpaceID != pw[1] || c.SpaceName == "" {
				t.Errorf("candidate %s: %+v, want prefix %s in space %s with its name", id, c, pw[0], pw[1])
			}
		}
		wire := AmbiguousCandidates(res.Candidates)
		for _, c := range wire {
			if c.IDPrefix == "" || c.SpaceID == "" || c.SpaceName == "" || c.Name != name {
				t.Errorf("wire candidate %+v lacks the prefix, the space or the name", c)
			}
		}
	})

	t.Run("a repo URL resolves among the accessible", func(t *testing.T) {
		url := "https://git.example/kept-" + u + ".git"
		mine := seedResolveProject(t, w.pool, w.caller, w.spaceX, "url-mine-"+u, "um"+u, url)
		if res := w.resolve(t, url); res.Match == nil || res.Match.ID != mine || res.Match.MatchedVia != "repo" {
			t.Errorf("own repo URL: %+v", res.Match)
		}
		hidden := "https://git.example/hidden-" + u + ".git"
		_ = seedResolveProject(t, w.pool, w.stranger, w.strangersSpace, "url-hidden-"+u, "uh"+u, hidden)
		if res := w.resolve(t, hidden); res.Match != nil || len(res.Candidates) != 0 {
			t.Errorf("an inaccessible project's repo URL answered %+v %+v", res.Match, res.Candidates)
		}
	})

	t.Run("a fuzzy string lists the accessible only", func(t *testing.T) {
		token := "fz" + u
		mine := seedResolveProject(t, w.pool, w.caller, w.spaceX, "aa-"+token+"-mine", "fm"+u, "")
		hidden := seedResolveProject(t, w.pool, w.stranger, w.strangersSpace, "aa-"+token+"-hidden", "fh"+u, "")
		res := w.resolve(t, token)
		if res.Ambiguous {
			t.Fatalf("a fuzzy string is not an exact ambiguity: %+v", res.Candidates)
		}
		if !resolveHits(res, mine) {
			t.Errorf("the accessible project is neither the match nor a candidate: %+v %+v", res.Match, res.Candidates)
		}
		if resolveHits(res, hidden) {
			t.Error("the inaccessible project took part in the fuzzy form")
		}
		// With the inaccessible one out, the accessible is the sole candidate:
		// the confident auto-pick decides as before, by the score alone.
		if res.Match == nil && len(res.Candidates) != 1 {
			t.Errorf("candidates %+v, want the accessible one alone", res.Candidates)
		}
	})
}

package handler

// projects_prefix_test.go — sk-674.18 T025, matrix rows 106 (the REST half;
// MCP — mcp/tools/project_create_prefix_test.go), 107, 109. References —
// US-15, E-17 and D-4 of the spec, SAAS-PLAN 4.4, written before the code:
// a taken prefix — wherever it is taken, another's "Personal" included — is
// one neutral 422 prefix_taken with 2–3 free variants and nothing more (no
// constraint name, no space, no owner); the prefixes of deleted projects are
// neither suggested nor accepted; a project name is unique inside its space,
// not on the installation; an alias is unique on the installation.

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func prefixRouter(pool *pgxpool.Pool) chi.Router {
	h := NewProjectsHandler(pool, nil)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders)
		r.Use(RequireUser)
		r.Route("/projects", func(r chi.Router) {
			r.Post("/", h.Create)
			r.Get("/prefix-suggestions", h.PrefixSuggestions)
			r.Get("/prefix-check", h.PrefixCheck)
			r.Post("/{projectID}/aliases", h.AddAlias)
		})
	})
	return r
}

func newPrefixWorld(t *testing.T) *projWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	return &projWorld{&spWorld{t: t, pool: pool, router: prefixRouter(pool)}}
}

// create posts a project and forgets it at cleanup when it was created.
func (w *projWorld) create(u spUser, space, name, prefix string) spResp {
	w.t.Helper()
	r := w.do(u, http.MethodPost, "/api/v1/projects", map[string]any{"name": name, "idPrefix": prefix, "spaceId": space})
	if r.status == http.StatusCreated || r.status == http.StatusOK {
		id, _ := r.body["id"].(string)
		if inner, ok := r.body["project"].(map[string]any); ok {
			id, _ = inner["id"].(string)
		}
		w.forgetProject(id)
	}
	return r
}

func TestProjectsPrefix_TakenIsNeutral(t *testing.T) {
	w := newPrefixWorld(t)
	me := seedSpUser(t, w.pool, "user")
	_, mine := w.space(me)
	stranger := seedSpUser(t, w.pool, "user")
	personalID, personalShort := w.personal(stranger)
	w.rename(personalID, "Stranger Personal "+personalShort)
	held, heldPrefix := testsupport.SeedProjectInSpace(t, w.pool, stranger.id, personalID, "e17")
	var heldName string
	if err := w.pool.QueryRow(context.Background(), `SELECT name FROM projects WHERE id = $1`, held).Scan(&heldName); err != nil {
		t.Fatal(err)
	}

	r := w.create(me, mine, "Neutral "+heldPrefix, heldPrefix)
	if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodePrefixTaken {
		t.Fatalf("taken in another's \"Personal\": %d %s, want 422 %s", r.status, r.raw, spaces.CodePrefixTaken)
	}
	keys := make([]string, 0, len(r.body))
	for k := range r.body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !slices.Equal(keys, []string{"error", "message", "suggestions"}) {
		t.Errorf("body keys %v, want the contract's error, message, suggestions", keys)
	}
	sugg, _ := r.body["suggestions"].([]any)
	if len(sugg) < 2 || len(sugg) > 3 {
		t.Errorf("suggestions %v, want 2–3 free variants", sugg)
	}
	for _, s := range sugg {
		if s == heldPrefix {
			t.Errorf("the taken prefix is suggested back: %v", sugg)
		}
	}
	for _, leak := range []string{personalShort, "Stranger Personal", stranger.name, stranger.id.String(),
		held.String(), heldName, "constraint", "id_prefix", "unique", "duplicate", "personal"} {
		if strings.Contains(strings.ToLower(r.raw), strings.ToLower(leak)) {
			t.Errorf("the refusal tells %q: %s", leak, r.raw)
		}
	}
}

func TestProjectsPrefix_DeletedProjectsPrefixIsReserved(t *testing.T) {
	w := newPrefixWorld(t)
	me := seedSpUser(t, w.pool, "user")
	_, mine := w.space(me)
	// A project of a space its owner deletes: its prefix stays reserved.
	gone := seedSpUser(t, w.pool, "user")
	doomed, doomedShort := w.space(gone)
	w.rename(doomed, "Doomed "+doomedShort)
	_, prefix := testsupport.SeedProjectInSpace(t, w.pool, gone.id, doomed, "rsv")
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM reserved_prefixes WHERE prefix = $1`, prefix)
	})
	svc := spaces.NewService(w.pool, spaces.Options{Creation: spaces.CreationAll})
	if err := svc.Delete(context.Background(), spaces.Caller{UserID: gone.id, Username: gone.name,
		Actor: spaces.Actor{UserID: gone.id, Username: gone.name, Type: "human"}}, doomedShort, "Doomed "+doomedShort); err != nil {
		t.Fatalf("delete the space: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM projects WHERE lower(id_prefix) = $1`, prefix); n != 0 {
		t.Fatalf("the project of the deleted space is still there")
	}

	r := w.do(me, http.MethodGet, "/api/v1/projects/prefix-suggestions?name="+url.QueryEscape(prefix), nil)
	if r.status != http.StatusOK {
		t.Fatalf("suggestions: %d %s", r.status, r.raw)
	}
	sugg, _ := r.body["suggestions"].([]any)
	if len(sugg) == 0 || slices.Contains(sugg, any(prefix)) {
		t.Errorf("suggestions for %q: %v, want free ones without the reserved prefix", prefix, sugg)
	}
	if r := w.do(me, http.MethodGet, "/api/v1/projects/prefix-check?prefix="+prefix, nil); r.body["available"] != false || r.body["reason"] != spaces.CodePrefixTaken {
		t.Errorf("check of the reserved prefix: %s", r.raw)
	}
	if r := w.create(me, mine, "Reuse "+prefix, prefix); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodePrefixTaken {
		t.Errorf("creation with the reserved prefix: %d %s, want 422 %s", r.status, r.raw, spaces.CodePrefixTaken)
	}
}

func TestProjectsPrefix_NameUniqueInSpaceAliasOnInstallation(t *testing.T) {
	w := newPrefixWorld(t)
	me := seedSpUser(t, w.pool, "user")
	_, x := w.space(me)
	_, y := w.space(me)
	name := "Same Name " + uniquePrefix("")

	first := w.create(me, x, name, uniquePrefix("nu"))
	if first.status != http.StatusCreated && first.status != http.StatusOK {
		t.Fatalf("the first project: %d %s", first.status, first.raw)
	}
	if r := w.create(me, y, name, uniquePrefix("nu")); r.status != http.StatusCreated && r.status != http.StatusOK {
		t.Errorf("the same name in another space: %d %s, want it created", r.status, r.raw)
	}
	r := w.create(me, x, name, uniquePrefix("nu"))
	if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeProjectNameTaken {
		t.Errorf("the same name in the same space: %d %s, want 422 %s", r.status, r.raw, spaces.CodeProjectNameTaken)
	}
	if n := w.count(`SELECT count(*) FROM projects WHERE name = $1`, name); n != 2 {
		t.Errorf("%d projects named %q, want the two of different spaces", n, name)
	}

	// An alias taken by a project of another space — and of another owner.
	other := seedSpUser(t, w.pool, "user")
	otherSpace, _ := w.space(other)
	held, _ := testsupport.SeedProjectInSpace(t, w.pool, other.id, otherSpace, "als")
	alias := uniquePrefix("al")
	if _, err := w.pool.Exec(context.Background(), `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())`, alias, held, other.id); err != nil {
		t.Fatal(err)
	}
	created, _ := first.body["id"].(string)
	if inner, ok := first.body["project"].(map[string]any); ok {
		created, _ = inner["id"].(string)
	}
	if r := w.do(me, http.MethodPost, "/api/v1/projects/"+created+"/aliases", map[string]any{"alias": alias}); r.status != http.StatusUnprocessableEntity || r.body["error"] != "alias_taken" {
		t.Errorf("an alias taken in another space: %d %s, want 422 alias_taken", r.status, r.raw)
	}
	if r := w.do(me, http.MethodPost, "/api/v1/projects/"+created+"/aliases", map[string]any{"alias": uniquePrefix("al")}); r.status >= 300 {
		t.Errorf("a free alias: %d %s", r.status, r.raw)
	}
}

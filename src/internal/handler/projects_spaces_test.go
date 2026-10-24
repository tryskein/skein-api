package handler

// projects_spaces_test.go — sk-674.18 T024, matrix rows 93, 95, 96 (the REST
// half; MCP — mcp/tools/project_member_add_test.go), 103. References — the
// spec (US-6, US-12, INV-5, E-31), the contract «Choosing the space of a new
// project» and K-20 of the plan, written before the code.

import (
	"context"
	"encoding/base64"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// projWorld drives the projects routes (create, list, patch, members).
type projWorld struct {
	*spWorld
}

func newProjWorld(t *testing.T, defaultVisibility string) *projWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	return &projWorld{&spWorld{t: t, pool: pool, router: projRouter(pool, defaultVisibility)}}
}

func projRouter(pool *pgxpool.Pool, defaultVisibility string) chi.Router {
	h := NewProjectsHandler(pool, nil)
	h.SetDefaultVisibility(defaultVisibility)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders)
		r.Use(RequireUser)
		r.Route("/projects", func(r chi.Router) {
			r.Get("/", h.List)
			r.Post("/", h.Create)
			r.Route("/{projectID}", func(r chi.Router) {
				r.Patch("/", h.Patch)
				r.Put("/members/{userID}", h.UpsertMember)
			})
		})
	})
	return r
}

// forgetProject removes, at cleanup, a project POST /projects created.
func (w *projWorld) forgetProject(id string) {
	w.t.Helper()
	pid, err := uuid.Parse(id)
	if err != nil {
		w.t.Fatalf("project id %q: %v", id, err)
	}
	w.t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM events WHERE project_id = $1`,
			// Issues seeded into the project (their attachments cascade):
			// issues.project_id is RESTRICT, and a project left behind keeps
			// its space — the common space of asCommon cannot be restored.
			`DELETE FROM issues WHERE project_id = $1`,
			`DELETE FROM project_members WHERE project_id = $1`,
			`DELETE FROM project_counters WHERE project_id = $1`,
			`DELETE FROM projects WHERE id = $1`,
		} {
			_, _ = w.pool.Exec(ctx, q, pid)
		}
	})
}

func uniquePrefix(tag string) string {
	return tag + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}

// ---------------------------------------------------------------------------
// Row 93 — creating a project (US-6, US-12)
// ---------------------------------------------------------------------------

func TestProjectsSpaces_Create(t *testing.T) {
	for _, def := range []string{spaces.VisibilitySpace, spaces.VisibilityMembers} {
		t.Run("default "+def, func(t *testing.T) {
			w := newProjWorld(t, def)
			owner := seedSpUser(t, w.pool, "user")
			participant := seedSpUser(t, w.pool, "user")
			admin := seedSpUser(t, w.pool, "admin")
			ext := seedSpExternal(t, w.pool)
			spaceID, short := w.space(owner)
			w.join(spaceID, participant)

			// A participant creates in his space; the visibility is the
			// installation default.
			prefix := uniquePrefix("pcr")
			r := w.do(participant, http.MethodPost, "/api/v1/projects",
				map[string]any{"name": prefix, "idPrefix": prefix, "spaceId": short})
			if r.status != http.StatusCreated {
				t.Fatalf("a participant creates: %d %s", r.status, r.raw)
			}
			w.forgetProject(r.body["id"].(string))
			if r.body["spaceId"] != short || r.body["visibility"] != def {
				t.Errorf("created in %v with visibility %v, want %s and the default %s", r.body["spaceId"], r.body["visibility"], short, def)
			}

			// The admin who sees the space without membership.
			prefix = uniquePrefix("pca")
			r = w.do(admin, http.MethodPost, "/api/v1/projects",
				map[string]any{"name": prefix, "idPrefix": prefix, "spaceId": short})
			if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeNotSpaceMember || r.body["spaceId"] != short {
				t.Errorf("the admin with adminAccess: %d %v, want 422 not_space_member with spaceId", r.status, r.raw)
			}
			// An external user.
			prefix = uniquePrefix("pce")
			r = w.do(ext, http.MethodPost, "/api/v1/projects", map[string]any{"name": prefix, "idPrefix": prefix})
			if r.status != http.StatusUnprocessableEntity || r.body["error"] != "external_cannot_create_project" {
				t.Errorf("an external user: %d %v, want 422 external_cannot_create_project", r.status, r.raw)
			}
			// A personal space: always members.
			_, personal := w.personal(participant)
			prefix = uniquePrefix("pcp")
			r = w.do(participant, http.MethodPost, "/api/v1/projects",
				map[string]any{"name": prefix, "idPrefix": prefix, "spaceId": personal, "visibility": "space"})
			if r.status != http.StatusCreated {
				t.Fatalf("in the personal space: %d %s", r.status, r.raw)
			}
			w.forgetProject(r.body["id"].(string))
			if r.body["visibility"] != spaces.VisibilityMembers {
				t.Errorf("a personal project's visibility %v, want members", r.body["visibility"])
			}
			// Several eligible spaces, none named: space_required with them.
			prefix = uniquePrefix("pcq")
			r = w.do(participant, http.MethodPost, "/api/v1/projects", map[string]any{"name": prefix, "idPrefix": prefix})
			if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeSpaceRequired {
				t.Fatalf("no spaceId with two spaces: %d %s, want 422 space_required", r.status, r.raw)
			}
			listed := map[string]bool{}
			for _, s := range r.body["spaces"].([]any) {
				listed[s.(map[string]any)["id"].(string)] = true
			}
			if len(listed) != 2 || !listed[short] || !listed[personal] {
				t.Errorf("space_required lists %v, want exactly %s and %s", listed, short, personal)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Row 95 — a project never changes its space (E-31)
// ---------------------------------------------------------------------------

func TestProjectsSpaces_SpaceChangeRefusedWithoutEffects(t *testing.T) {
	w := newProjWorld(t, spaces.VisibilityMembers)
	owner := seedSpUser(t, w.pool, "user")
	from, _ := w.space(owner)
	_, to := w.space(owner)
	pid, _ := testsupport.SeedProjectInSpace(t, w.pool, owner.id, from, "psc")
	type snap struct {
		Name, Visibility string
		Space            uuid.UUID
		Updated          string
		Events           int
	}
	read := func() snap {
		var s snap
		if err := w.pool.QueryRow(context.Background(), `
SELECT name, visibility, space_id, updated_at::text, (SELECT count(*) FROM events WHERE project_id = p.id)
  FROM projects p WHERE id = $1`, pid).Scan(&s.Name, &s.Visibility, &s.Space, &s.Updated, &s.Events); err != nil {
			t.Fatalf("read: %v", err)
		}
		return s
	}
	before := read()
	r := w.do(owner, http.MethodPatch, "/api/v1/projects/"+pid.String(),
		map[string]any{"spaceId": to, "name": "Moved and renamed", "visibility": "space"})
	if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeSpaceChangeForbidden {
		t.Errorf("PATCH spaceId: %d %s, want 422 space_change_forbidden", r.status, r.raw)
	}
	if after := read(); after != before {
		t.Errorf("the refusal changed the project: before %+v, after %+v", before, after)
	}
}

// ---------------------------------------------------------------------------
// Row 96 (REST) — who may be added to a project of a space (INV-5)
// ---------------------------------------------------------------------------

func TestProjectsSpaces_AddMemberINV5(t *testing.T) {
	w := newProjWorld(t, spaces.VisibilityMembers)
	owner := seedSpUser(t, w.pool, "user")
	participant := seedSpUser(t, w.pool, "user")
	other := seedSpUser(t, w.pool, "user")
	outsider := seedSpUser(t, w.pool, "user")
	ext := seedSpExternal(t, w.pool)
	spaceID, _ := w.space(owner)
	w.join(spaceID, participant)
	w.join(spaceID, other)
	pid, _ := testsupport.SeedProjectInSpace(t, w.pool, owner.id, spaceID, "pam")
	path := func(u uuid.UUID) string { return "/api/v1/projects/" + pid.String() + "/members/" + u.String() }

	for _, c := range []struct {
		name string
		who  uuid.UUID
		role string
	}{
		{"a participant as member", participant.id, "member"},
		{"a participant as owner", other.id, "owner"},
		{"an external user as member", ext.id, "member"},
	} {
		if r := w.do(owner, http.MethodPut, path(c.who), map[string]any{"role": c.role}); r.status >= 300 {
			t.Errorf("%s: %d %s, want it added", c.name, r.status, r.raw)
		}
	}
	if r := w.do(owner, http.MethodPut, path(ext.id), map[string]any{"role": "owner"}); r.status < 400 {
		t.Errorf("an external user as owner: %d, want a refusal", r.status)
	}
	// Not a participant: the very answer of a user who does not exist.
	stranger := w.do(owner, http.MethodPut, path(outsider.id), map[string]any{"role": "member"})
	missing := w.do(owner, http.MethodPut, path(uuid.New()), map[string]any{"role": "member"})
	if stranger.status < 400 {
		t.Fatalf("a non-participant was added: %d", stranger.status)
	}
	if stranger.status != missing.status || stranger.body["error"] != missing.body["error"] || keysOf(stranger.body) != keysOf(missing.body) {
		t.Errorf("non-participant %d %v differs from nonexistent %d %v", stranger.status, stranger.body, missing.status, missing.body)
	}
	if n := w.count(`SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`, pid, outsider.id); n != 0 {
		t.Errorf("the non-participant got a membership row")
	}
}

func keysOf(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// ---------------------------------------------------------------------------
// Row 103 — /projects in the order of the column (K-20), space=
// ---------------------------------------------------------------------------

func TestProjectsSpaces_ListOrderAndSpaceFilter(t *testing.T) {
	w := newProjWorld(t, spaces.VisibilityMembers)
	u := seedSpUser(t, w.pool, "user")
	commonOwner := seedSpUser(t, w.pool, "user")
	personalID, _ := w.personal(u)
	commonID, _ := w.asCommon(commonOwner)
	w.join(commonID, u)
	zID, zShort := w.space(u)
	w.rename(zID, "Zeta")
	aID, aShort := w.space(u)
	w.rename(aID, "alpha")

	seed := func(space uuid.UUID, name string) uuid.UUID {
		pid, _ := testsupport.SeedProjectInSpace(t, w.pool, u.id, space, "plo")
		if _, err := w.pool.Exec(context.Background(), `UPDATE projects SET name = $2 WHERE id = $1`, pid, name+" "+pid.String()[:6]); err != nil {
			t.Fatal(err)
		}
		return pid
	}
	// Inserted out of order on purpose.
	zB := seed(zID, "b")
	aB := seed(aID, "B")
	common := seed(commonID, "c")
	zA := seed(zID, "a")
	personal := seed(personalID, "p")
	aA := seed(aID, "a")
	want := []uuid.UUID{personal, common, aA, aB, zA, zB}

	var got []uuid.UUID
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/api/v1/projects?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := w.do(u, http.MethodGet, path, nil)
		if r.status != http.StatusOK {
			t.Fatalf("list: %d %s", r.status, r.raw)
		}
		for _, it := range r.body["items"].([]any) {
			row := it.(map[string]any)
			id := uuid.MustParse(row["id"].(string))
			for _, mine := range want {
				if id == mine {
					got = append(got, id)
					if row["spaceId"] == nil || row["spaceId"] == "" || row["visibility"] == nil {
						t.Errorf("row %s without spaceId/visibility: %v", id, row)
					}
				}
			}
		}
		next, _ := r.body["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d of the %d projects across pages: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: %s, want %s (personal → common → spaces by name → projects by name)", i, got[i], want[i])
		}
	}

	r := w.do(u, http.MethodGet, "/api/v1/projects?space="+aShort, nil)
	var only []uuid.UUID
	for _, it := range r.body["items"].([]any) {
		row := it.(map[string]any)
		only = append(only, uuid.MustParse(row["id"].(string)))
		if row["spaceId"] != aShort {
			t.Errorf("space=%s returned a row of %v", aShort, row["spaceId"])
		}
	}
	if len(only) != 2 || only[0] != aA || only[1] != aB {
		t.Errorf("space=%s: %v, want [%s %s]", aShort, only, aA, aB)
	}
	if r := w.do(u, http.MethodGet, "/api/v1/projects?space="+testsupport.NewShortID(t), nil); len(r.body["items"].([]any)) != 0 {
		t.Errorf("an unknown space lists projects")
	}
	_ = zShort
}

// TestProjectsList_CursorCarriesTheShortIDNotTheUUID — Reference — the identifiers of the contract
// (spaces/contract.go): a space goes out by its short id, never by its
// UUID. The list cursor goes out too: decoded, it names no space UUID;
// paging through two same-named spaces — where the space's own key decides
// the order — returns every project once, in both the alphabetic and the
// search list.
func TestProjectsList_CursorCarriesTheShortIDNotTheUUID(t *testing.T) {
	w := newProjWorld(t, spaces.VisibilityMembers)
	u := seedSpUser(t, w.pool, "user")
	tag := uniquePrefix("twin")
	var spaceIDs []uuid.UUID
	var want []uuid.UUID
	for range 2 {
		id, _ := w.space(u)
		w.rename(id, "Twin "+tag)
		spaceIDs = append(spaceIDs, id)
		for range 2 {
			pid, _ := testsupport.SeedProjectInSpace(t, w.pool, u.id, id, "cur")
			if _, err := w.pool.Exec(context.Background(), `UPDATE projects SET name = $2 WHERE id = $1`, pid, tag+" "+pid.String()[:8]); err != nil {
				t.Fatal(err)
			}
			want = append(want, pid)
		}
	}
	for _, query := range []string{"", "&q=" + tag} {
		seen := map[uuid.UUID]int{}
		cursor := ""
		for page := 0; page < 20; page++ {
			path := "/api/v1/projects?limit=1" + query
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			r := w.do(u, http.MethodGet, path, nil)
			if r.status != http.StatusOK {
				t.Fatalf("list %q: %d %s", query, r.status, r.raw)
			}
			for _, it := range r.body["items"].([]any) {
				seen[uuid.MustParse(it.(map[string]any)["id"].(string))]++
			}
			next, _ := r.body["nextCursor"].(string)
			if next == "" {
				break
			}
			raw, err := base64.RawURLEncoding.DecodeString(next)
			if err != nil {
				t.Fatalf("cursor %q is not base64url: %v", next, err)
			}
			for _, id := range spaceIDs {
				if strings.Contains(string(raw), id.String()) {
					t.Errorf("list %q: the cursor names the space's UUID: %s", query, raw)
				}
			}
			cursor = next
		}
		for _, pid := range want {
			if seen[pid] != 1 {
				t.Errorf("list %q: project %s listed %d times across the pages", query, pid, seen[pid])
			}
		}
	}
}

// TestProjectsSpaces_PatchNullSpaceIsAbsent.
// Reference — E-31: a project never changes its space, a spaceId in the
// body is refused. A null spaceId names no space — the «not set» of a
// client that serialises absent fields as null — so the patch applies; any
// value is still refused and changes nothing.
func TestProjectsSpaces_PatchNullSpaceIsAbsent(t *testing.T) {
	w := newProjWorld(t, spaces.VisibilityMembers)
	u := seedSpUser(t, w.pool, "user")
	space, _ := w.space(u)
	_, otherShort := w.space(u)
	pid, _ := testsupport.SeedProjectInSpace(t, w.pool, u.id, space, "pns")
	base := "/api/v1/projects/" + pid.String()

	r := w.do(u, http.MethodPatch, base, map[string]any{"spaceId": nil, "description": "null space"})
	if r.status != http.StatusOK {
		t.Errorf("PATCH with spaceId null: %d %s, want the patch applied", r.status, r.raw)
	}
	if w.count(`SELECT count(*) FROM projects WHERE id = $1 AND description = 'null space'`, pid) != 1 {
		t.Error("the patch with spaceId null was not applied")
	}
	r = w.do(u, http.MethodPatch, base, map[string]any{"spaceId": otherShort, "description": "moved"})
	if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeSpaceChangeForbidden {
		t.Errorf("PATCH with a spaceId: %d %s, want 422 %s", r.status, r.raw, spaces.CodeSpaceChangeForbidden)
	}
	if w.count(`SELECT count(*) FROM projects WHERE id = $1 AND space_id = $2 AND description = 'null space'`, pid, space) != 1 {
		t.Error("the refused PATCH changed the project")
	}
}

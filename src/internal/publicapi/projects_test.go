package publicapi_test

// projects_test.go — the project resources of the public API (sk-878.3.1
// T022): the addressing of {project} (row 80) and the parity of the 13
// operations with the MCP tools of the same names (row 82, US-1, SC-001,
// INV-4). The reference of the parity is the MCP channel itself: the same
// key makes the same request on both channels, over twin projects, and the
// code of every refusal and the events of every success must be the same.

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// addressWorld is the seed of the addressing test.
type addressWorld struct {
	owner uuid.UUID
	space uuid.UUID
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner, space uuid.UUID, name string) (uuid.UUID, string) {
	t.Helper()
	return testsupport.SeedProjectInSpace(t, pool, owner, space, name)
}

// addAlias gives project an alias directly, as an existing value made before
// the rules of new aliases (a reserved word, another project's prefix).
func addAlias(t *testing.T, pool *pgxpool.Pool, project, by uuid.UUID, alias string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind) VALUES ($1, $2, $3, 'human')`,
		alias, project, by); err != nil {
		t.Fatalf("add alias %q: %v", alias, err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM project_aliases WHERE alias = $1`, alias) })
}

func setPrefix(t *testing.T, pool *pgxpool.Pool, project uuid.UUID, prefix string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE projects SET id_prefix = $2 WHERE id = $1`, project, prefix); err != nil {
		t.Fatalf("set prefix %q: %v", prefix, err)
	}
}

func projectsKey(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, l apikeys.Level, only ...uuid.UUID) string {
	t.Helper()
	g := apikeys.Grant{Level: l, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	if len(only) > 0 {
		g.Scope = apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: only}
	}
	return rigKey(t, pool, user, g)
}

// wantProject asserts that GET /projects/{handle} answers 200 with project.
func wantProject(t *testing.T, rg *resourceRig, key, handle string, project uuid.UUID) {
	t.Helper()
	c := rg.rest(key, http.MethodGet, "/api/v1/projects/"+handle, nil, nil)
	if c.status != http.StatusOK {
		t.Fatalf("GET /projects/%s: %d %s, want 200", handle, c.status, c.body)
	}
	if got := c.json(t)["id"]; got != project.String() {
		t.Fatalf("GET /projects/%s named %v, want %s", handle, got, project)
	}
}

// wantNoProject asserts the answer of a handle that names no project within
// reach: 404 project_not_found.
func wantNoProject(t *testing.T, c restCall, what string) {
	t.Helper()
	if c.status != http.StatusNotFound || c.code() != "project_not_found" {
		t.Fatalf("%s: %d %s, want 404 project_not_found", what, c.status, c.body)
	}
}

// TestProjects_Addressing — row 80 («Addressing»). {project} in a path is a
// UUID, a prefix without case or an alias, matched in the order UUID →
// prefix → alias among the projects narrowed to the key's scope first; a
// name is never matched; GET /projects/resolve is always resolveProject,
// even when a project's alias is "resolve" (that project is addressed by
// its UUID); an alias travels URL-encoded. Red when an alias of a project
// outside the key's scope takes the place of a project inside it.
func TestProjects_Addressing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	owner, _ := testsupport.SeedUser(t, pool, "")
	space, _ := seedOwnSpace(t, pool, owner)
	w := addressWorld{owner: owner, space: space}
	key := projectsKey(t, pool, w.owner, apikeys.LevelRead)
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")

	a, aPrefix := seedProject(t, pool, w.owner, w.space, "addra")
	aAlias := "al-" + suffix
	addAlias(t, pool, a, w.owner, aAlias)

	t.Run("each unique handle names the project", func(t *testing.T) {
		wantProject(t, rg, key, a.String(), a)
		wantProject(t, rg, key, strings.ToUpper(aPrefix), a)
		wantProject(t, rg, key, aAlias, a)
	})

	t.Run("an alias travels URL-encoded", func(t *testing.T) {
		alias := "алиас-" + suffix
		addAlias(t, pool, a, w.owner, alias)
		wantProject(t, rg, key, url.PathEscape(alias), a)
	})

	t.Run("a name is not a handle", func(t *testing.T) {
		var name string
		if err := pool.QueryRow(context.Background(), `SELECT name FROM projects WHERE id = $1`, a).Scan(&name); err != nil {
			t.Fatalf("read name: %v", err)
		}
		wantNoProject(t, rg.rest(key, http.MethodGet, "/api/v1/projects/"+url.PathEscape(name), nil, nil), "GET by name")
		wantNoProject(t, rg.rest(key, http.MethodGet, "/api/v1/projects/"+uuid.NewString(), nil, nil), "GET a missing UUID")
	})

	t.Run("a prefix wins over another project's alias", func(t *testing.T) {
		b, _ := seedProject(t, pool, w.owner, w.space, "addrb")
		c, _ := seedProject(t, pool, w.owner, w.space, "addrc")
		handle := "hx" + suffix
		setPrefix(t, pool, b, handle)
		addAlias(t, pool, c, w.owner, handle)
		wantProject(t, rg, key, handle, b)
		wantProject(t, rg, key, c.String(), c)
	})

	t.Run("the key's scope narrows the candidates before matching", func(t *testing.T) {
		inside, _ := seedProject(t, pool, w.owner, w.space, "addri")
		outside, _ := seedProject(t, pool, w.owner, w.space, "addro")
		insideKey := projectsKey(t, pool, w.owner, apikeys.LevelRead, inside)

		// The project outside the scope holds the prefix, the one inside
		// holds the same string as its alias: the alias inside is reached.
		h1 := "hp" + suffix
		setPrefix(t, pool, outside, h1)
		addAlias(t, pool, inside, w.owner, h1)
		wantProject(t, rg, insideKey, h1, inside)

		// And the other way round: the prefix inside wins, whatever alias
		// the project outside the scope has.
		h2 := "hq" + suffix
		setPrefix(t, pool, inside, h2)
		addAlias(t, pool, outside, w.owner, h2)
		wantProject(t, rg, insideKey, h2, inside)

		// The project outside is reached by none of its handles.
		wantNoProject(t, rg.rest(insideKey, http.MethodGet, "/api/v1/projects/"+outside.String(), nil, nil), "GET outside by UUID")
		// With a key of scope «all» the owner reaches it (base line).
		wantProject(t, rg, key, outside.String(), outside)
	})

	t.Run("resolve is always resolveProject", func(t *testing.T) {
		r, _ := seedProject(t, pool, w.owner, w.space, "addrr")
		addAlias(t, pool, r, w.owner, "resolve")
		c := rg.rest(key, http.MethodGet, "/api/v1/projects/resolve?q="+url.QueryEscape(aPrefix), nil, nil)
		if c.status != http.StatusOK {
			t.Fatalf("GET /projects/resolve: %d %s", c.status, c.body)
		}
		match, _ := c.json(t)["match"].(map[string]any)
		if match == nil || match["id"] != a.String() {
			t.Fatalf("GET /projects/resolve?q=%s answered %s, want the match %s", aPrefix, c.body, a)
		}
		wantProject(t, rg, key, r.String(), r)
		wantNoProject(t, rg.rest(projectsKey(t, pool, w.owner, apikeys.LevelManage), http.MethodPatch,
			"/api/v1/projects/resolve", map[string]any{"description": "x"}, nil), "PATCH /projects/resolve")
	})
}

// parity is one operation asked on both channels: the code of the answer
// ("" for a success) must be the same, and want names it.
func parity(t *testing.T, op string, rest restCall, mcp toolAnswer, want string) {
	t.Helper()
	if got := rest.code(); got != want {
		t.Errorf("%s: REST answered %d %s, want code %q", op, rest.status, rest.body, want)
	}
	if mcp.code != want {
		t.Errorf("%s: MCP answered %q (%s), want code %q", op, mcp.code, mcp.text, want)
	}
}

// wantOK asserts a successful REST answer of status.
func wantOK(t *testing.T, op string, c restCall, status int) {
	t.Helper()
	if c.status != status {
		t.Fatalf("%s: REST answered %d %s, want %d", op, c.status, c.body, status)
	}
}

// sameEvents asserts that the success on both channels wrote the same
// events (type and actor type), and at least one.
func sameEvents(t *testing.T, op string, rest, mcp []string) {
	t.Helper()
	if len(rest) == 0 {
		t.Errorf("%s: REST wrote no event", op)
	}
	if !slices.Equal(rest, mcp) {
		t.Errorf("%s: REST wrote %v, MCP wrote %v", op, rest, mcp)
	}
}

func addMember(t *testing.T, pool *pgxpool.Pool, project, user uuid.UUID, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by) VALUES ($1, $2, $3, now(), $2)
ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role`, project, user, role); err != nil {
		t.Fatalf("add member: %v", err)
	}
}

func ids(t *testing.T, items any, key string) []string {
	t.Helper()
	list, _ := items.([]any)
	out := []string{}
	for _, it := range list {
		m, _ := it.(map[string]any)
		s, _ := m[key].(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func componentID(t *testing.T, pool *pgxpool.Pool, project uuid.UUID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM components WHERE project_id = $1 AND name = $2`,
		project, name).Scan(&id); err != nil {
		t.Fatalf("read component %q: %v", name, err)
	}
	return id
}

// TestProjects_ParityWithMCP — row 82 (US-1, SC-001, INV-4). Each of the 13
// operations of T022, asked with the same key on REST and on MCP over twin
// projects, gives the same code for the success and for each business
// refusal, and the same events for the success. createProject is allowed
// only to a key of scope spaces or all, and only inside its spaces;
// updateProject is one atomic command — a refused part changes nothing.
// Red when a code or the events differ from MCP.
func TestProjects_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	member, _ := testsupport.SeedUser(t, pool, "")
	newcomer, newcomerName := testsupport.SeedUser(t, pool, "")
	space, spaceShort := seedOwnSpace(t, pool, owner)
	other, otherShort := seedOwnSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, member)
	// A project member of a non-personal space is a participant of it.
	testsupport.AddSpaceMember(t, pool, space, newcomer)

	pr, prPrefix := seedProject(t, pool, owner, space, "parr")
	pm, pmPrefix := seedProject(t, pool, owner, space, "parm")
	for _, p := range []uuid.UUID{pr, pm} {
		addMember(t, pool, p, member, "member")
	}
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	takenAlias := "pt-" + suffix
	addAlias(t, pool, pr, owner, takenAlias)
	var prName string
	if err := pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, pr).Scan(&prName); err != nil {
		t.Fatalf("read name: %v", err)
	}

	ko := projectsKey(t, pool, owner, apikeys.LevelManage)
	km := projectsKey(t, pool, member, apikeys.LevelManage)
	mcpArgs := func(project uuid.UUID, kv ...any) map[string]any {
		m := map[string]any{"project": project.String()}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	path := func(p uuid.UUID, rest string) string { return "/api/v1/projects/" + p.String() + rest }

	t.Run("getMe", func(t *testing.T) {
		rest := rg.rest(ko, http.MethodGet, "/api/v1/me", nil, nil)
		wantOK(t, "getMe", rest, http.StatusOK)
		mcp := rg.tool(ko, "skein_whoami", nil)
		parity(t, "getMe", rest, mcp, "")
		r, m := rest.json(t), mcp.json(t)
		ru, _ := r["user"].(map[string]any)
		mu, _ := m["user"].(map[string]any)
		if ru["id"] != owner.String() || mu["id"] != owner.String() {
			t.Errorf("getMe user %v, MCP %v, want %s", ru["id"], mu["id"], owner)
		}
		if got, want := ids(t, r["projects"], "id"), ids(t, m["projects"], "id"); !slices.Equal(got, want) {
			t.Errorf("getMe projects %v, MCP %v", got, want)
		}
		if r["isAdmin"] != m["isAdmin"] {
			t.Errorf("getMe isAdmin %v, MCP %v", r["isAdmin"], m["isAdmin"])
		}
	})

	t.Run("listProjects", func(t *testing.T) {
		rest := rg.rest(ko, http.MethodGet, "/api/v1/projects?limit=100", nil, nil)
		wantOK(t, "listProjects", rest, http.StatusOK)
		mcp := rg.tool(ko, "skein_project_list", map[string]any{"limit": 100})
		parity(t, "listProjects", rest, mcp, "")
		if got, want := ids(t, rest.json(t)["items"], "id"), ids(t, mcp.json(t)["items"], "id"); !slices.Equal(got, want) {
			t.Errorf("listProjects %v, MCP %v", got, want)
		}
	})

	t.Run("resolveProject", func(t *testing.T) {
		rest := rg.rest(ko, http.MethodGet, "/api/v1/projects/resolve?q="+url.QueryEscape(prPrefix), nil, nil)
		mcp := rg.tool(ko, "skein_project_resolve", map[string]any{"q": prPrefix})
		parity(t, "resolveProject", rest, mcp, "")
		rm, _ := rest.json(t)["match"].(map[string]any)
		mm, _ := mcp.json(t)["match"].(map[string]any)
		if rm["id"] != pr.String() || mm["id"] != pr.String() {
			t.Errorf("resolveProject match %v, MCP %v, want %s", rm["id"], mm["id"], pr)
		}
		// The same name in two spaces of the owner is ambiguous.
		twin, _ := seedProject(t, pool, owner, other, "partw")
		if _, err := pool.Exec(ctx, `UPDATE projects SET name = $2 WHERE id = $1`, twin, prName); err != nil {
			t.Fatalf("rename: %v", err)
		}
		parity(t, "resolveProject ambiguous",
			rg.rest(ko, http.MethodGet, "/api/v1/projects/resolve?q="+url.QueryEscape(prName), nil, nil),
			rg.tool(ko, "skein_project_resolve", map[string]any{"q": prName}), "ambiguous_project")
	})

	t.Run("createProject", func(t *testing.T) {
		n := "parnew" + suffix
		before := lastEventID(t, pool)
		rest := rg.rest(ko, http.MethodPost, "/api/v1/projects", map[string]any{"name": n + "r", "space": spaceShort}, nil)
		wantOK(t, "createProject", rest, http.StatusCreated)
		restID := uuid.MustParse(rest.json(t)["id"].(string))
		projectCleanup(t, pool, restID)
		if loc := rest.header.Get("Location"); loc != "/api/v1/projects/"+restID.String() {
			t.Errorf("createProject Location %q", loc)
		}
		mcp := rg.tool(ko, "skein_project_create", map[string]any{"name": n + "m", "space": spaceShort})
		parity(t, "createProject", rest, mcp, "")
		mp, _ := mcp.json(t)["project"].(map[string]any)
		mcpID := uuid.MustParse(mp["id"].(string))
		projectCleanup(t, pool, mcpID)
		sameEvents(t, "createProject", eventsSince(t, pool, restID, before), eventsSince(t, pool, mcpID, before))

		for _, c := range []struct {
			name string
			body map[string]any
			want string
		}{
			{"prefix of another project", map[string]any{"name": n + "a", "space": spaceShort, "idPrefix": prPrefix}, "prefix_taken"},
			{"prefix equal to another project's alias", map[string]any{"name": n + "b", "space": spaceShort, "idPrefix": takenAlias}, "prefix_taken"},
			{"prefix equal to a reserved word", map[string]any{"name": n + "c", "space": spaceShort, "idPrefix": "resolve"}, "prefix_taken"},
			{"prefix outside the rule", map[string]any{"name": n + "d", "space": spaceShort, "idPrefix": "9bad"}, "prefix_invalid"},
			{"name taken in the space", map[string]any{"name": prName, "space": spaceShort}, "project_name_taken"},
			{"alias taken", map[string]any{"name": n + "e", "space": spaceShort, "aliases": []string{takenAlias}}, "id_taken"},
			{"alias equal to another project's prefix", map[string]any{"name": n + "f", "space": spaceShort, "aliases": []string{pmPrefix}}, "id_taken"},
			{"several spaces and none named", map[string]any{"name": n + "g"}, "space_required"},
			{"visibility outside the set", map[string]any{"name": n + "h", "space": spaceShort, "visibility": "all"}, "validation_error"},
		} {
			parity(t, "createProject: "+c.name,
				rg.rest(ko, http.MethodPost, "/api/v1/projects", c.body, nil),
				rg.tool(ko, "skein_project_create", c.body), c.want)
		}

		// A key limited to projects cannot create one (insufficient_scope);
		// a key limited to spaces creates only inside them.
		narrow := projectsKey(t, pool, owner, apikeys.LevelManage, pr)
		parity(t, "createProject with a key limited to projects",
			rg.rest(narrow, http.MethodPost, "/api/v1/projects", map[string]any{"name": n + "i", "space": spaceShort}, nil),
			rg.tool(narrow, "skein_project_create", map[string]any{"name": n + "i", "space": spaceShort}), "insufficient_scope")
		spacesKey := rigKey(t, pool, owner, apikeys.Grant{Level: apikeys.LevelManage,
			Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{other}}})
		restOut := rg.rest(spacesKey, http.MethodPost, "/api/v1/projects", map[string]any{"name": n + "j", "space": spaceShort}, nil)
		mcpOut := rg.tool(spacesKey, "skein_project_create", map[string]any{"name": n + "j", "space": spaceShort})
		if restOut.code() == "" || restOut.code() != mcpOut.code {
			t.Errorf("createProject outside the key's spaces: REST %d %s, MCP %q", restOut.status, restOut.body, mcpOut.code)
		}
		inside := rg.rest(spacesKey, http.MethodPost, "/api/v1/projects", map[string]any{"name": n + "k", "space": otherShort}, nil)
		wantOK(t, "createProject inside the key's spaces", inside, http.StatusCreated)
		projectCleanup(t, pool, uuid.MustParse(inside.json(t)["id"].(string)))
	})

	t.Run("getProject", func(t *testing.T) {
		rest := rg.rest(ko, http.MethodGet, path(pr, ""), nil, nil)
		wantOK(t, "getProject", rest, http.StatusOK)
		if rest.json(t)["id"] != pr.String() {
			t.Errorf("getProject answered %s", rest.body)
		}
		missing := uuid.New()
		parity(t, "getProject of a missing project",
			rg.rest(ko, http.MethodGet, path(missing, ""), nil, nil),
			rg.tool(ko, "skein_project_members", mcpArgs(missing, "action", "list")), "project_not_found")
	})

	t.Run("updateProject", func(t *testing.T) {
		before := lastEventID(t, pool)
		rest := rg.rest(ko, http.MethodPatch, path(pr, ""), map[string]any{"description": "rest " + suffix}, nil)
		wantOK(t, "updateProject", rest, http.StatusOK)
		mcp := rg.tool(ko, "skein_project_update", mcpArgs(pm, "description", "mcp "+suffix))
		parity(t, "updateProject", rest, mcp, "")
		sameEvents(t, "updateProject", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))

		for _, c := range []struct {
			name string
			key  string
			body map[string]any
			want string
		}{
			{"by a member", km, map[string]any{"description": "m"}, "forbidden"},
			{"nothing to change", ko, map[string]any{}, "validation_error"},
			{"a space", ko, map[string]any{"space": otherShort}, "space_change_forbidden"},
			{"an alias taken", ko, map[string]any{"aliasesAdd": []string{takenAlias}}, "id_taken"},
			{"an alias equal to another project's prefix", ko, map[string]any{"aliasesAdd": []string{prPrefix}}, "id_taken"},
			{"an alias equal to a reserved word", ko, map[string]any{"aliasesAdd": []string{"resolve"}}, "id_taken"},
			{"an alias not on the project", ko, map[string]any{"aliasesRemove": []string{"none-" + suffix}}, "validation_error"},
		} {
			args := mcpArgs(pm)
			for k, v := range c.body {
				args[k] = v
			}
			parity(t, "updateProject: "+c.name, rg.rest(c.key, http.MethodPatch, path(pm, ""), c.body, nil),
				rg.tool(c.key, "skein_project_update", args), c.want)
		}

		// One atomic command: the refused alias leaves the description as it
		// was.
		var was string
		if err := pool.QueryRow(ctx, `SELECT description FROM projects WHERE id = $1`, pr).Scan(&was); err != nil {
			t.Fatalf("read description: %v", err)
		}
		c := rg.rest(ko, http.MethodPatch, path(pr, ""), map[string]any{"description": "lost", "aliasesAdd": []string{takenAlias}}, nil)
		if c.code() != "id_taken" {
			t.Fatalf("updateProject with a taken alias: %d %s", c.status, c.body)
		}
		var now string
		if err := pool.QueryRow(ctx, `SELECT description FROM projects WHERE id = $1`, pr).Scan(&now); err != nil {
			t.Fatalf("read description: %v", err)
		}
		if now != was {
			t.Errorf("a refused updateProject changed the description: %q → %q", was, now)
		}
	})

	t.Run("listProjectMembers", func(t *testing.T) {
		rest := rg.rest(ko, http.MethodGet, path(pr, "/members"), nil, nil)
		wantOK(t, "listProjectMembers", rest, http.StatusOK)
		mcp := rg.tool(ko, "skein_project_members", mcpArgs(pm, "action", "list"))
		parity(t, "listProjectMembers", rest, mcp, "")
		if got, want := ids(t, rest.json(t)["items"], "userId"), ids(t, mcp.json(t)["members"], "userId"); !slices.Equal(got, want) || len(got) != 2 {
			t.Errorf("listProjectMembers %v, MCP %v", got, want)
		}
	})

	t.Run("putProjectMember and deleteProjectMember", func(t *testing.T) {
		before := lastEventID(t, pool)
		rest := rg.rest(ko, http.MethodPut, path(pr, "/members/"+newcomer.String()), map[string]any{"role": "member"}, nil)
		wantOK(t, "putProjectMember", rest, http.StatusOK)
		mcp := rg.tool(ko, "skein_project_members", mcpArgs(pm, "action", "add", "userId", newcomer.String(), "role", "member"))
		parity(t, "putProjectMember", rest, mcp, "")
		sameEvents(t, "putProjectMember", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))

		parity(t, "putProjectMember by a member",
			rg.rest(km, http.MethodPut, path(pm, "/members/"+newcomer.String()), map[string]any{"role": "owner"}, nil),
			rg.tool(km, "skein_project_members", mcpArgs(pm, "action", "add", "userId", newcomer.String(), "role", "owner")), "forbidden")
		parity(t, "putProjectMember with a role outside the set",
			rg.rest(ko, http.MethodPut, path(pm, "/members/"+newcomerName), map[string]any{"role": "boss"}, nil),
			rg.tool(ko, "skein_project_members", mcpArgs(pm, "action", "add", "username", newcomerName, "role", "boss")), "validation_error")
		parity(t, "putProjectMember of an unknown username",
			rg.rest(ko, http.MethodPut, path(pm, "/members/nobody-"+suffix), map[string]any{"role": "member"}, nil),
			rg.tool(ko, "skein_project_members", mcpArgs(pm, "action", "add", "username", "nobody-"+suffix, "role", "member")), "user_not_found")

		before = lastEventID(t, pool)
		del := rg.rest(ko, http.MethodDelete, path(pr, "/members/"+newcomerName), nil, nil)
		wantOK(t, "deleteProjectMember", del, http.StatusNoContent)
		mdel := rg.tool(ko, "skein_project_members", mcpArgs(pm, "action", "remove", "username", newcomerName))
		parity(t, "deleteProjectMember", del, mdel, "")
		sameEvents(t, "deleteProjectMember", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))

		parity(t, "deleteProjectMember of a non-member",
			rg.rest(ko, http.MethodDelete, path(pm, "/members/"+newcomer.String()), nil, nil),
			rg.tool(ko, "skein_project_members", mcpArgs(pm, "action", "remove", "userId", newcomer.String())), "member_not_found")
		parity(t, "deleteProjectMember of another by a member",
			rg.rest(km, http.MethodDelete, path(pm, "/members/"+owner.String()), nil, nil),
			rg.tool(km, "skein_project_members", mcpArgs(pm, "action", "remove", "userId", owner.String())), "forbidden")
	})

	t.Run("components", func(t *testing.T) {
		before := lastEventID(t, pool)
		cname := "comp-" + suffix
		rest := rg.rest(ko, http.MethodPost, path(pr, "/components"), map[string]any{"name": cname}, nil)
		wantOK(t, "createComponent", rest, http.StatusCreated)
		mcp := rg.tool(ko, "skein_component", mcpArgs(pm, "action", "create", "name", cname))
		parity(t, "createComponent", rest, mcp, "")
		sameEvents(t, "createComponent", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))
		parity(t, "createComponent with a taken name",
			rg.rest(ko, http.MethodPost, path(pm, "/components"), map[string]any{"name": cname}, nil),
			rg.tool(ko, "skein_component", mcpArgs(pm, "action", "create", "name", cname)), "id_taken")
		parity(t, "createComponent by a member",
			rg.rest(km, http.MethodPost, path(pm, "/components"), map[string]any{"name": cname + "x"}, nil),
			rg.tool(km, "skein_component", mcpArgs(pm, "action", "create", "name", cname+"x")), "forbidden")

		list := rg.rest(ko, http.MethodGet, path(pr, "/components"), nil, nil)
		mlist := rg.tool(ko, "skein_component", mcpArgs(pm, "action", "list"))
		parity(t, "listComponents", list, mlist, "")
		if got, want := ids(t, list.json(t)["items"], "name"), ids(t, mlist.json(t)["items"], "name"); !slices.Equal(got, want) || len(got) != 1 {
			t.Errorf("listComponents %v, MCP %v", got, want)
		}

		rc, mc := componentID(t, pool, pr, cname), componentID(t, pool, pm, cname)
		before = lastEventID(t, pool)
		upd := rg.rest(ko, http.MethodPatch, path(pr, "/components/"+rc), map[string]any{"name": cname + "2"}, nil)
		wantOK(t, "updateComponent", upd, http.StatusOK)
		mupd := rg.tool(ko, "skein_component", mcpArgs(pm, "action", "update", "id", mc, "name", cname+"2"))
		parity(t, "updateComponent", upd, mupd, "")
		sameEvents(t, "updateComponent", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))
		missing := uuid.NewString()
		parity(t, "updateComponent of a component outside the project",
			rg.rest(ko, http.MethodPatch, path(pm, "/components/"+missing), map[string]any{"name": "z"}, nil),
			rg.tool(ko, "skein_component", mcpArgs(pm, "action", "update", "id", missing, "name", "z")), "validation_error")

		before = lastEventID(t, pool)
		arc := rg.rest(ko, http.MethodPost, path(pr, "/components/"+rc+"/archive"), nil, nil)
		wantOK(t, "archiveComponent", arc, http.StatusOK)
		marc := rg.tool(ko, "skein_component", mcpArgs(pm, "action", "archive", "id", mc))
		parity(t, "archiveComponent", arc, marc, "")
		sameEvents(t, "archiveComponent", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))
		parity(t, "archiveComponent of a component outside the project",
			rg.rest(ko, http.MethodPost, path(pm, "/components/"+missing+"/archive"), nil, nil),
			rg.tool(ko, "skein_component", mcpArgs(pm, "action", "archive", "id", missing)), "validation_error")

		// Restoring from the archive is updateComponent with archived false.
		before = lastEventID(t, pool)
		res := rg.rest(ko, http.MethodPatch, path(pr, "/components/"+rc), map[string]any{"archived": false}, nil)
		wantOK(t, "updateComponent restoring", res, http.StatusOK)
		mres := rg.tool(ko, "skein_component", mcpArgs(pm, "action", "archive", "id", mc, "archived", false))
		parity(t, "updateComponent restoring", res, mres, "")
		sameEvents(t, "updateComponent restoring", eventsSince(t, pool, pr, before), eventsSince(t, pool, pm, before))
	})
}

// TestProjects_MeWithinTheKeyScope — row 103 (US-3, INV-2), the spaces of
// getMe and skein_whoami: a key of scope «spaces {S1}» sees only S1, a key of
// scope «projects {P2}» only the space of P2, a key of scope «all» both;
// canCreateProject holds only for a key that may create a project (manage,
// scope spaces or all). Red when a space outside the key's scope, or the
// right to create a project the key lacks, is shown.
func TestProjects_MeWithinTheKeyScope(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	owner, _ := testsupport.SeedUser(t, pool, "")
	s1, s1Short := seedOwnSpace(t, pool, owner)
	s2, s2Short := seedOwnSpace(t, pool, owner)
	seedProject(t, pool, owner, s1, "mes")
	p2, _ := seedProject(t, pool, owner, s2, "mes")
	key := func(l apikeys.Level, scope apikeys.Scope) string {
		return rigKey(t, pool, owner, apikeys.Grant{Level: l, Scope: scope})
	}
	// spacesOf reads the spaces of an answer: short id → canCreateProject,
	// only for S1 and S2 (the owner's personal space is not looked at).
	spacesOf := func(body map[string]any) map[string]bool {
		out := map[string]bool{}
		list, _ := body["spaces"].([]any)
		for _, it := range list {
			m, _ := it.(map[string]any)
			id, _ := m["id"].(string)
			if id == s1Short || id == s2Short {
				create, _ := m["canCreateProject"].(bool)
				out[id] = create
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		key  string
		want map[string]bool
	}{
		{"spaces S1, manage", key(apikeys.LevelManage, apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{s1}}),
			map[string]bool{s1Short: true}},
		{"spaces S1, write", key(apikeys.LevelWrite, apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{s1}}),
			map[string]bool{s1Short: false}},
		{"projects P2, manage", key(apikeys.LevelManage, apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{p2}}),
			map[string]bool{s2Short: false}},
		{"all, manage", key(apikeys.LevelManage, apikeys.Scope{Mode: apikeys.ScopeAll}),
			map[string]bool{s1Short: true, s2Short: true}},
		{"all, read", key(apikeys.LevelRead, apikeys.Scope{Mode: apikeys.ScopeAll}),
			map[string]bool{s1Short: false, s2Short: false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rest := rg.rest(c.key, http.MethodGet, "/api/v1/me", nil, nil)
			wantOK(t, "getMe", rest, http.StatusOK)
			if got := spacesOf(rest.json(t)); !maps.Equal(got, c.want) {
				t.Errorf("getMe spaces %v, want %v", got, c.want)
			}
			mcp := rg.tool(c.key, "skein_whoami", map[string]any{})
			if mcp.code != "" {
				t.Fatalf("skein_whoami: %s", mcp.text)
			}
			if got := spacesOf(mcp.json(t)); !maps.Equal(got, c.want) {
				t.Errorf("skein_whoami spaces %v, want %v", got, c.want)
			}
		})
	}
}

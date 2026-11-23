package handler

// components_rest_contract_test.go — the HTTP contract of the project
// components routes of the internal API (/ui/v1/projects/{projectID}/components),
// against the migrated schema.
//
// The handler is mounted on the same paths as in buildRouter
// (cmd/api/main.go, the /projects route group), behind the real
// ParseUserHeaders and RequireUser middleware, and every request carries the
// identity headers skein-web sets. Every write is followed by a separate
// read, so the test sees the stored state, not the echo of the write.
//
// Reference for the expected answers:
//   - create, read, update, archive and restore of a component, DELETE as a
//     soft-archive alias, and the archived filter of the list are the route
//     table of buildRouter and the doc comments of ComponentsHandler and
//     projects.ListComponents;
//   - refusal codes: component_not_found (404) is the code the public API
//     reference names for a component that is not in the project
//     (docs/public-api.md, "Errors"); not_found / forbidden are the project
//     access answers of mapAccessErr shared by every /projects route;
//     component_name_taken, already_archived and not_archived are the
//     business refusals (422) the handler declares for the BFF and the
//     frontend to branch on; invalid_repo_id, repo_not_found and
//     invalid_glob are the path validation codes, also pinned per path by
//     TestValidatePaths.
//
// The internal component routes carry no swag annotations, so there is no
// generated document to compare against; only machine codes and statuses are
// asserted, never message text (TEST_GUIDELINES §5).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// compEnv is one project with a declared repo, its owner, a plain member
// and an outsider, and the components routes mounted as in buildRouter.
type compEnv struct {
	pool     *pgxpool.Pool
	router   chi.Router
	project  uuid.UUID
	repo     uuid.UUID
	owner    uuid.UUID
	member   uuid.UUID
	outsider uuid.UUID
	names    map[uuid.UUID]string
}

func newCompEnv(t *testing.T) *compEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	e := &compEnv{pool: pool, names: map[uuid.UUID]string{}}
	var name string
	e.owner, name = testsupport.SeedUser(t, pool, "user")
	e.names[e.owner] = name
	e.member, name = testsupport.SeedUser(t, pool, "user")
	e.names[e.member] = name
	e.outsider, name = testsupport.SeedUser(t, pool, "user")
	e.names[e.outsider] = name
	e.project, _ = testsupport.SeedProject(t, pool, e.owner, "comprest")
	testsupport.AddProjectMember(t, pool, e.project, e.member, "member")
	e.repo = testsupport.SeedRepo(t, pool, e.project, "api", "https://example.com/api.git")

	h := NewComponentsHandler(pool)
	r := chi.NewRouter()
	r.Use(ParseUserHeaders)
	r.Route("/ui/v1/projects", func(r chi.Router) {
		r.Use(RequireUser)
		r.Route("/{projectID}/components", func(r chi.Router) {
			r.Get("/", h.List)
			r.Post("/", h.Create)
			r.Route("/{componentID}", func(r chi.Router) {
				r.Get("/", h.Get)
				r.Patch("/", h.Patch)
				r.Delete("/", h.Delete)
				r.Post("/archive", h.Archive)
				r.Post("/unarchive", h.Unarchive)
			})
		})
	})
	e.router = r
	return e
}

// base is the components collection path of the env's project.
func (e *compEnv) base() string {
	return "/ui/v1/projects/" + e.project.String() + "/components"
}

// do sends one request as user (uuid.Nil — no identity headers) and returns
// the status and the decoded JSON body (nil for an empty body).
func (e *compEnv) do(t *testing.T, user uuid.UUID, method, path string, body any) (int, any) {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if user != uuid.Nil {
		req.Header.Set(auth.HeaderUserID, user.String())
		req.Header.Set(auth.HeaderUsername, e.names[user])
		req.Header.Set(auth.HeaderUserRole, "user")
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Body.Len() == 0 {
		return rec.Code, nil
	}
	var out any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
	}
	return rec.Code, out
}

// object sends the request, requires status want and a JSON object body.
func (e *compEnv) object(t *testing.T, user uuid.UUID, method, path string, body any, want int) map[string]any {
	t.Helper()
	code, out := e.do(t, user, method, path, body)
	if code != want {
		t.Fatalf("%s %s = %d %v, want %d", method, path, code, out, want)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("%s %s: body %#v is not a JSON object", method, path, out)
	}
	return obj
}

// noContent sends the request and requires 204 with an empty body.
func (e *compEnv) noContent(t *testing.T, user uuid.UUID, method, path string) {
	t.Helper()
	code, out := e.do(t, user, method, path, nil)
	if code != http.StatusNoContent || out != nil {
		t.Fatalf("%s %s = %d %v, want 204 with no body", method, path, code, out)
	}
}

// refusal sends the request and requires status want with machine code wantCode.
func (e *compEnv) refusal(t *testing.T, user uuid.UUID, method, path string, body any, want int, wantCode string) {
	t.Helper()
	obj := e.object(t, user, method, path, body, want)
	if got := obj["error"]; got != wantCode {
		t.Fatalf("%s %s: error = %#v, want %q (body %v)", method, path, got, wantCode, obj)
	}
}

// listIDs returns the component ids of GET list with the given query.
func (e *compEnv) listIDs(t *testing.T, user uuid.UUID, query string) map[string]bool {
	t.Helper()
	code, out := e.do(t, user, http.MethodGet, e.base()+query, nil)
	if code != http.StatusOK {
		t.Fatalf("GET list%s = %d %v, want 200", query, code, out)
	}
	rows, ok := out.([]any)
	if !ok {
		t.Fatalf("GET list%s: body %#v is not a JSON array", query, out)
	}
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.(map[string]any)["id"].(string)] = true
	}
	return ids
}

// componentCount is the number of components rows of the env's project.
func (e *compEnv) componentCount(t *testing.T) int {
	t.Helper()
	return testsupport.CountRows(t, e.pool, "components", "project_id", e.project)
}

// pathsOf returns the paths array of a component body as (repoId, glob) pairs.
func pathsOf(t *testing.T, obj map[string]any) [][2]string {
	t.Helper()
	raw, ok := obj["paths"].([]any)
	if !ok {
		t.Fatalf("paths = %#v, want a JSON array", obj["paths"])
	}
	out := make([][2]string, 0, len(raw))
	for _, p := range raw {
		m := p.(map[string]any)
		out = append(out, [2]string{m["repoId"].(string), m["glob"].(string)})
	}
	return out
}

// componentState is the stored state a refused write must not change.
type componentState struct {
	name, description string
	archived          bool
	paths             int
}

// state reads one component back over HTTP as the owner.
func (e *compEnv) state(t *testing.T, id string) componentState {
	t.Helper()
	got := e.object(t, e.owner, http.MethodGet, e.base()+"/"+id, nil, http.StatusOK)
	_, archived := got["archivedAt"]
	return componentState{
		name:        got["name"].(string),
		description: got["description"].(string),
		archived:    archived,
		paths:       len(pathsOf(t, got)),
	}
}

// TestComponentsREST_Lifecycle walks one component through create, read,
// update, archive, restore and the DELETE alias in one ordered scenario,
// reading the stored state back after every write.
func TestComponentsREST_Lifecycle(t *testing.T) {
	e := newCompEnv(t)

	created := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{
		"name":        "billing",
		"description": "payment flows",
		"paths":       []map[string]any{{"repoId": e.repo.String(), "glob": "billing/**"}},
	}, http.StatusCreated)
	id, _ := created["id"].(string)
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("create: id = %#v, want a UUID", created["id"])
	}
	item := e.base() + "/" + id

	// Created: read back by a member of the project.
	got := e.object(t, e.member, http.MethodGet, item, nil, http.StatusOK)
	if got["id"] != id || got["projectId"] != e.project.String() {
		t.Errorf("GET id/projectId = %v/%v, want %s/%s", got["id"], got["projectId"], id, e.project)
	}
	if got["name"] != "billing" || got["description"] != "payment flows" {
		t.Errorf("GET name/description = %v/%v, want billing/payment flows", got["name"], got["description"])
	}
	if got["createdBy"] != e.owner.String() {
		t.Errorf("GET createdBy = %v, want the creator %s", got["createdBy"], e.owner)
	}
	if p := pathsOf(t, got); len(p) != 1 || p[0] != [2]string{e.repo.String(), "billing/**"} {
		t.Errorf("GET paths = %v, want one path {%s billing/**}", p, e.repo)
	}
	if _, archived := got["archivedAt"]; archived {
		t.Errorf("a new component carries archivedAt = %v", got["archivedAt"])
	}
	if ids := e.listIDs(t, e.member, ""); !ids[id] {
		t.Errorf("the member's default list misses the new component %s", id)
	}

	// A patch changes only the fields it carries.
	patched := e.object(t, e.owner, http.MethodPatch, item, map[string]any{"name": "billing-core"}, http.StatusOK)
	if patched["name"] != "billing-core" {
		t.Errorf("PATCH answer name = %v, want billing-core", patched["name"])
	}
	if st := e.state(t, id); st != (componentState{name: "billing-core", description: "payment flows", paths: 1}) {
		t.Errorf("after PATCH name: %+v, want only the name changed", st)
	}

	// An empty paths array and an empty description clear them.
	e.object(t, e.owner, http.MethodPatch, item, map[string]any{"description": "", "paths": []any{}}, http.StatusOK)
	if st := e.state(t, id); st != (componentState{name: "billing-core"}) {
		t.Errorf("after PATCH description=\"\" paths=[]: %+v, want both cleared and the name kept", st)
	}

	// Archive hides it from the active list; a second archive is refused.
	e.noContent(t, e.owner, http.MethodPost, item+"/archive")
	if st := e.state(t, id); !st.archived {
		t.Error("GET after archive: no archivedAt")
	}
	if ids := e.listIDs(t, e.owner, ""); ids[id] {
		t.Error("the default list still shows the archived component")
	}
	e.refusal(t, e.owner, http.MethodPost, item+"/archive", nil, http.StatusUnprocessableEntity, "already_archived")

	// Unarchive restores it; a second unarchive is refused.
	e.noContent(t, e.owner, http.MethodPost, item+"/unarchive")
	if st := e.state(t, id); st.archived {
		t.Error("GET after unarchive still carries archivedAt")
	}
	if ids := e.listIDs(t, e.owner, ""); !ids[id] {
		t.Error("the default list misses the restored component")
	}
	e.refusal(t, e.owner, http.MethodPost, item+"/unarchive", nil, http.StatusUnprocessableEntity, "not_archived")

	// DELETE is the soft-archive alias: the row stays, archived.
	e.noContent(t, e.owner, http.MethodDelete, item)
	if st := e.state(t, id); !st.archived || st.name != "billing-core" {
		t.Errorf("GET after DELETE: %+v, want the same component archived", st)
	}
	e.refusal(t, e.owner, http.MethodDelete, item, nil, http.StatusUnprocessableEntity, "already_archived")
}

// TestComponentsREST_ArchivedFilter — the archived parameter of the list over
// a project with one active and one archived component: absent and "false"
// list the active one, "true" the archived one, "all" both.
func TestComponentsREST_ArchivedFilter(t *testing.T) {
	e := newCompEnv(t)
	active := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{"name": "active"}, http.StatusCreated)["id"].(string)
	archived := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{"name": "archived"}, http.StatusCreated)["id"].(string)
	e.noContent(t, e.owner, http.MethodPost, e.base()+"/"+archived+"/archive")

	for _, c := range []struct {
		query string
		want  []string
	}{
		{"", []string{active}},
		{"?archived=false", []string{active}},
		{"?archived=true", []string{archived}},
		{"?archived=all", []string{active, archived}},
	} {
		got := e.listIDs(t, e.owner, c.query)
		if len(got) != len(c.want) {
			t.Errorf("GET list%s = %v, want exactly %v", c.query, got, c.want)
			continue
		}
		for _, id := range c.want {
			if !got[id] {
				t.Errorf("GET list%s = %v, want exactly %v", c.query, got, c.want)
			}
		}
	}
}

// TestComponentsREST_Lens — the lens of the list narrows the rows to the
// components with at least one matching issue: on-me — an accepted open issue
// someone else created and assigned to the caller; delegated — an open issue
// the caller created and assigned to someone else. A component without such
// an issue is in neither.
func TestComponentsREST_Lens(t *testing.T) {
	e := newCompEnv(t)
	ctx := context.Background()
	onMe := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{"name": "on-me"}, http.StatusCreated)["id"].(string)
	delegated := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{"name": "delegated"}, http.StatusCreated)["id"].(string)
	quiet := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{"name": "quiet"}, http.StatusCreated)["id"].(string)

	var prefix string
	if err := e.pool.QueryRow(ctx, `SELECT id_prefix FROM projects WHERE id = $1`, e.project).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	attach := func(issueID, componentID string) {
		if _, err := e.pool.Exec(ctx, `INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
			issueID, componentID); err != nil {
			t.Fatalf("attach %s to %s: %v", issueID, componentID, err)
		}
	}
	toOwner := testsupport.SeedIssue(t, e.pool, e.project, e.member, prefix+"-1", "on the owner", "open", 2)
	testsupport.SetAssignee(t, e.pool, toOwner, e.names[e.owner])
	testsupport.Accept(t, e.pool, toOwner, e.owner, true)
	attach(toOwner, onMe)
	fromOwner := testsupport.SeedIssue(t, e.pool, e.project, e.owner, prefix+"-2", "given to the member", "open", 2)
	testsupport.SetAssignee(t, e.pool, fromOwner, e.names[e.member])
	attach(fromOwner, delegated)
	plain := testsupport.SeedIssue(t, e.pool, e.project, e.owner, prefix+"-3", "nobody's", "open", 2)
	attach(plain, quiet)

	for _, c := range []struct {
		lens string
		want string
	}{
		{"on-me", onMe},
		{"delegated", delegated},
	} {
		got := e.listIDs(t, e.owner, "?lens="+c.lens)
		if len(got) != 1 || !got[c.want] {
			t.Errorf("GET list?lens=%s = %v, want exactly [%s]", c.lens, got, c.want)
		}
	}
	if got := e.listIDs(t, e.owner, ""); len(got) != 3 {
		t.Errorf("GET list without a lens = %v, want all three components", got)
	}
}

// TestComponentsREST_Refusals pins the status and machine code of every
// expected refusal; each refused request is followed by a read showing that
// it changed nothing.
func TestComponentsREST_Refusals(t *testing.T) {
	e := newCompEnv(t)

	existing := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{
		"name": "taken",
	}, http.StatusCreated)["id"].(string)
	other := e.object(t, e.owner, http.MethodPost, e.base(), map[string]any{
		"name": "other",
	}, http.StatusCreated)["id"].(string)
	item := e.base() + "/" + existing

	// A component of another project, addressed through this project's path.
	foreignProject, _ := testsupport.SeedProject(t, e.pool, e.owner, "compforeign")
	foreign := testsupport.SeedComponent(t, e.pool, foreignProject, e.owner, "foreign", "")

	missing := uuid.Must(uuid.NewV7()).String()
	missingItem := e.base() + "/" + missing
	wantExisting := componentState{name: "taken"}
	wantOther := componentState{name: "other"}
	before := e.componentCount(t)

	tests := []struct {
		name     string
		user     uuid.UUID
		method   string
		path     string
		body     any
		want     int
		wantCode string
	}{
		// Request shape.
		{"create without a name", e.owner, http.MethodPost, e.base(),
			map[string]any{"description": "no name"}, http.StatusBadRequest, "invalid_name"},
		{"create with a body that is not JSON", e.owner, http.MethodPost, e.base(),
			"{", http.StatusBadRequest, "invalid_json"},
		{"patch with a body that is not JSON", e.owner, http.MethodPatch, item,
			"{", http.StatusBadRequest, "invalid_json"},
		{"project id that is not a UUID on list", e.owner, http.MethodGet,
			"/ui/v1/projects/not-a-uuid/components", nil, http.StatusBadRequest, "invalid_id"},
		{"project id that is not a UUID on create", e.owner, http.MethodPost,
			"/ui/v1/projects/not-a-uuid/components", map[string]any{"name": "x"}, http.StatusBadRequest, "invalid_id"},
		{"project id that is not a UUID on GET", e.owner, http.MethodGet,
			"/ui/v1/projects/not-a-uuid/components/" + missing, nil, http.StatusBadRequest, "invalid_id"},
		{"project id that is not a UUID on PATCH", e.owner, http.MethodPatch,
			"/ui/v1/projects/not-a-uuid/components/" + missing, map[string]any{"name": "x"}, http.StatusBadRequest, "invalid_id"},
		{"project id that is not a UUID on archive", e.owner, http.MethodPost,
			"/ui/v1/projects/not-a-uuid/components/" + missing + "/archive", nil, http.StatusBadRequest, "invalid_id"},
		{"component id that is not a UUID on GET", e.owner, http.MethodGet,
			e.base() + "/not-a-uuid", nil, http.StatusBadRequest, "invalid_component_id"},
		{"component id that is not a UUID on PATCH", e.owner, http.MethodPatch,
			e.base() + "/not-a-uuid", map[string]any{"name": "x"}, http.StatusBadRequest, "invalid_component_id"},
		{"component id that is not a UUID on archive", e.owner, http.MethodPost,
			e.base() + "/not-a-uuid/archive", nil, http.StatusBadRequest, "invalid_component_id"},
		{"list with an unknown lens", e.owner, http.MethodGet, e.base() + "?lens=everyone",
			nil, http.StatusBadRequest, "invalid_lens"},

		// Paths.
		{"create with a repoId that is not a UUID", e.owner, http.MethodPost, e.base(),
			map[string]any{"name": "p1", "paths": []map[string]any{{"repoId": "nope", "glob": "a/**"}}},
			http.StatusBadRequest, "invalid_repo_id"},
		{"create with a repo the project does not declare", e.owner, http.MethodPost, e.base(),
			map[string]any{"name": "p2", "paths": []map[string]any{{"repoId": missing, "glob": "a/**"}}},
			http.StatusBadRequest, "repo_not_found"},
		{"create with an empty glob", e.owner, http.MethodPost, e.base(),
			map[string]any{"name": "p3", "paths": []map[string]any{{"repoId": e.repo.String(), "glob": " "}}},
			http.StatusBadRequest, "invalid_glob"},
		{"patch with a repo the project does not declare", e.owner, http.MethodPatch, item,
			map[string]any{"paths": []map[string]any{{"repoId": missing, "glob": "a/**"}}},
			http.StatusBadRequest, "repo_not_found"},

		// Business refusals.
		{"create with a taken name", e.owner, http.MethodPost, e.base(),
			map[string]any{"name": "taken"}, http.StatusUnprocessableEntity, "component_name_taken"},
		{"rename onto a taken name", e.owner, http.MethodPatch, e.base() + "/" + other,
			map[string]any{"name": "taken"}, http.StatusUnprocessableEntity, "component_name_taken"},

		// Unknown component.
		{"GET an unknown component", e.owner, http.MethodGet, missingItem,
			nil, http.StatusNotFound, "component_not_found"},
		{"PATCH an unknown component", e.owner, http.MethodPatch, missingItem,
			map[string]any{"name": "x"}, http.StatusNotFound, "component_not_found"},
		{"archive an unknown component", e.owner, http.MethodPost, missingItem + "/archive",
			nil, http.StatusNotFound, "component_not_found"},
		{"unarchive an unknown component", e.owner, http.MethodPost, missingItem + "/unarchive",
			nil, http.StatusNotFound, "component_not_found"},
		{"DELETE an unknown component", e.owner, http.MethodDelete, missingItem,
			nil, http.StatusNotFound, "component_not_found"},
		{"GET another project's component through this project", e.owner, http.MethodGet,
			e.base() + "/" + foreign.String(), nil, http.StatusNotFound, "component_not_found"},
		{"PATCH another project's component through this project", e.owner, http.MethodPatch,
			e.base() + "/" + foreign.String(), map[string]any{"name": "x"}, http.StatusNotFound, "component_not_found"},
		{"archive another project's component through this project", e.owner, http.MethodPost,
			e.base() + "/" + foreign.String() + "/archive", nil, http.StatusNotFound, "component_not_found"},

		// Access: a member reads but does not manage, an outsider does not see the project.
		{"member creates", e.member, http.MethodPost, e.base(),
			map[string]any{"name": "by-member"}, http.StatusForbidden, "forbidden"},
		{"member patches", e.member, http.MethodPatch, item,
			map[string]any{"name": "by-member"}, http.StatusForbidden, "forbidden"},
		{"member archives", e.member, http.MethodPost, item + "/archive",
			nil, http.StatusForbidden, "forbidden"},
		{"member unarchives", e.member, http.MethodPost, item + "/unarchive",
			nil, http.StatusForbidden, "forbidden"},
		{"member deletes", e.member, http.MethodDelete, item,
			nil, http.StatusForbidden, "forbidden"},
		{"outsider lists", e.outsider, http.MethodGet, e.base(),
			nil, http.StatusNotFound, "not_found"},
		{"outsider reads", e.outsider, http.MethodGet, item,
			nil, http.StatusNotFound, "not_found"},
		{"outsider creates", e.outsider, http.MethodPost, e.base(),
			map[string]any{"name": "by-outsider"}, http.StatusNotFound, "not_found"},
		{"outsider patches", e.outsider, http.MethodPatch, item,
			map[string]any{"name": "by-outsider"}, http.StatusNotFound, "not_found"},
		{"outsider archives", e.outsider, http.MethodPost, item + "/archive",
			nil, http.StatusNotFound, "not_found"},
		{"request without a user", uuid.Nil, http.MethodGet, e.base(),
			nil, http.StatusUnauthorized, "unauthenticated"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.refusal(t, tc.user, tc.method, tc.path, tc.body, tc.want, tc.wantCode)
			if got := e.componentCount(t); got != before {
				t.Errorf("components in the project = %d after the refusal, want %d", got, before)
			}
			if st := e.state(t, existing); st != wantExisting {
				t.Errorf("the addressed component after the refusal: %+v, want %+v", st, wantExisting)
			}
			if st := e.state(t, other); st != wantOther {
				t.Errorf("the other component after the refusal: %+v, want %+v", st, wantOther)
			}
		})
	}
}

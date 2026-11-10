package publicapi_test

// issues_read_test.go — row 83 (sk-878.3.1 T023; US-1, SC-001): the reads
// of issues of the public API give what the MCP tool of the same name gives
// with the same key — listIssues (both modes), listReadyIssues (a hidden
// blocker counted), getIssue (ETag = version), listIssueEvents,
// listDependencies, listComments, getIssueMovePlan (write in both projects,
// one space) and search. The reference is the MCP channel over the same
// issues.

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// readWorld is the seed of the read parity: the owner's projects P (the
// issues), T (a move target in the same space), Q (in scope «all» only) and
// X (in another space of the owner).
type readWorld struct {
	owner         uuid.UUID
	p, t, q, x    uuid.UUID
	pPrefix       string
	tPrefix       string
	xPrefix       string
	a, b, c, h    string
	word          string
	all, scopedP  string
	pool          *pgxpool.Pool
	commentBodies []string
}

func seedReadWorld(t *testing.T, pool *pgxpool.Pool) readWorld {
	t.Helper()
	ctx := context.Background()
	w := readWorld{pool: pool, word: "rdw" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")}
	w.owner, _ = testsupport.SeedUser(t, pool, "")
	space, _ := seedOwnSpace(t, pool, w.owner)
	other, _ := seedOwnSpace(t, pool, w.owner)
	w.p, w.pPrefix = seedProject(t, pool, w.owner, space, "rdp")
	w.t, w.tPrefix = seedProject(t, pool, w.owner, space, "rdt")
	var qPrefix string
	w.q, qPrefix = seedProject(t, pool, w.owner, space, "rdq")
	w.x, w.xPrefix = seedProject(t, pool, w.owner, other, "rdx")

	w.a = testsupport.SeedIssue(t, pool, w.p, w.owner, w.pPrefix+"-1", "alpha "+w.word, "open", 1)
	w.b = testsupport.SeedIssue(t, pool, w.p, w.owner, w.pPrefix+"-2", "beta "+w.word, "open", 2)
	w.c = testsupport.SeedIssue(t, pool, w.p, w.owner, w.pPrefix+"-3", "gamma "+w.word, "in_progress", 3)
	w.h = testsupport.SeedIssue(t, pool, w.q, w.owner, qPrefix+"-1", "hidden "+w.word, "open", 0)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v (%s)", err, sql)
		}
	}
	dep := func(from, to, typ string) {
		exec(`INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`, from, to, typ, w.owner)
	}
	// a waits on the hidden h (outside the P key's scope), c on b.
	dep(w.a, w.h, "blocks")
	dep(w.c, w.b, "blocks")
	dep(w.b, w.c, "related")
	for i := 0; i < 3; i++ {
		body := "note " + strconv.Itoa(i) + " " + w.word
		w.commentBodies = append(w.commentBodies, body)
		exec(`INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'owner', $3, 'human', 'human', $4, now() + make_interval(secs => $5), now())`,
			uuid.New(), w.a, w.owner, body, i)
	}
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id, payload)
VALUES ('issue.field_changed', 'owner', $1, 'human', 'issue', $2, $3, '{}'::jsonb)`, w.owner, w.a, w.p)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE issue_id = ANY($1)`, []string{w.a, w.b, w.c, w.h})
		_, _ = pool.Exec(c, `DELETE FROM dependencies WHERE from_id = ANY($1)`, []string{w.a, w.b, w.c, w.h})
		_, _ = pool.Exec(c, `DELETE FROM events WHERE entity_id = ANY($1)`, []string{w.a, w.b, w.c, w.h})
	})
	w.all = projectsKey(t, pool, w.owner, apikeys.LevelRead)
	w.scopedP = projectsKey(t, pool, w.owner, apikeys.LevelRead, w.p, w.t, w.x)
	return w
}

// stringsOf is the sorted values of key of the items of a list answer.
func stringsOf(t *testing.T, items any, key string) []string {
	t.Helper()
	out := ids(t, items, key)
	sort.Strings(out)
	return out
}

// TestIssuesRead_ParityWithMCP — row 83. Red when an answer of REST differs
// from the MCP tool's.
func TestIssuesRead_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedReadWorld(t, pool)

	t.Run("listIssues of a project", func(t *testing.T) {
		for _, filter := range []struct {
			rest string
			mcp  map[string]any
		}{
			{"", map[string]any{}},
			{"&statuses=open", map[string]any{"statuses": []string{"open"}}},
			{"&isBlocked=true", map[string]any{"isBlocked": "true"}},
			{"&priority=2", map[string]any{"priority": 2}},
		} {
			rest := rg.rest(w.all, http.MethodGet, "/api/v1/issues?project="+w.pPrefix+filter.rest, nil, nil)
			args := map[string]any{"project": w.pPrefix, "limit": 100}
			for k, v := range filter.mcp {
				args[k] = v
			}
			mcp := rg.tool(w.all, "skein_list", args)
			parity(t, "listIssues"+filter.rest, rest, mcp, "")
			if got, want := stringsOf(t, rest.json(t)["items"], "id"), stringsOf(t, mcp.json(t)["items"], "id"); !slices.Equal(got, want) || len(got) == 0 {
				t.Errorf("listIssues%s %v, MCP %v", filter.rest, got, want)
			}
		}
		parity(t, "listIssues of a missing project",
			rg.rest(w.all, http.MethodGet, "/api/v1/issues?project=nope-"+w.word, nil, nil),
			rg.tool(w.all, "skein_list", map[string]any{"project": "nope-" + w.word}), "project_not_found")
	})

	t.Run("listIssues across projects", func(t *testing.T) {
		for _, key := range []string{w.all, w.scopedP} {
			rest := rg.rest(key, http.MethodGet, "/api/v1/issues?limit=100&q="+w.word, nil, nil)
			mcp := rg.tool(key, "skein_list", map[string]any{"crossProject": true, "q": w.word, "limit": 100})
			parity(t, "listIssues across", rest, mcp, "")
			got, want := stringsOf(t, rest.json(t)["items"], "id"), stringsOf(t, mcp.json(t)["items"], "id")
			if !slices.Equal(got, want) || len(got) == 0 {
				t.Errorf("listIssues across %v, MCP %v", got, want)
			}
			if key == w.scopedP && slices.Contains(got, w.h) {
				t.Errorf("the scoped key lists the issue outside its scope: %v", got)
			}
		}
	})

	t.Run("listReadyIssues counts a hidden blocker", func(t *testing.T) {
		rest := rg.rest(w.scopedP, http.MethodGet, "/api/v1/issues/ready?project="+w.pPrefix, nil, nil)
		mcp := rg.tool(w.scopedP, "skein_ready", map[string]any{"project": w.pPrefix})
		parity(t, "listReadyIssues", rest, mcp, "")
		got, want := stringsOf(t, rest.json(t)["items"], "id"), stringsOf(t, mcp.json(t)["items"], "id")
		if !slices.Equal(got, want) {
			t.Errorf("listReadyIssues %v, MCP %v", got, want)
		}
		if slices.Contains(got, w.a) || !slices.Contains(got, w.b) {
			t.Errorf("listReadyIssues %v: want %s (blocked by a hidden issue) out and %s in", got, w.a, w.b)
		}
	})

	t.Run("getIssue", func(t *testing.T) {
		rest := rg.rest(w.all, http.MethodGet, "/api/v1/issues/"+w.a, nil, nil)
		mcp := rg.tool(w.all, "skein_show", map[string]any{"id": w.a, "includeComments": false})
		parity(t, "getIssue", rest, mcp, "")
		r := rest.json(t)
		mi, _ := mcp.json(t)["issue"].(map[string]any)
		if r["id"] != w.a || mi["id"] != w.a || r["title"] != mi["title"] || r["version"] != mi["version"] {
			t.Errorf("getIssue %v %v %v, MCP %v %v %v", r["id"], r["title"], r["version"], mi["id"], mi["title"], mi["version"])
		}
		if want := `"` + strconv.Itoa(int(r["version"].(float64))) + `"`; rest.header.Get("ETag") != want {
			t.Errorf("ETag %q, want %s", rest.header.Get("ETag"), want)
		}
		parity(t, "getIssue outside the key's scope",
			rg.rest(w.scopedP, http.MethodGet, "/api/v1/issues/"+w.h, nil, nil),
			rg.tool(w.scopedP, "skein_show", map[string]any{"id": w.h}), "issue_not_found")
		parity(t, "getIssue of a missing issue",
			rg.rest(w.all, http.MethodGet, "/api/v1/issues/"+w.pPrefix+"-999", nil, nil),
			rg.tool(w.all, "skein_show", map[string]any{"id": w.pPrefix + "-999"}), "issue_not_found")
	})

	t.Run("listIssueEvents", func(t *testing.T) {
		mcp := rg.tool(w.all, "skein_show", map[string]any{"id": w.a, "includeComments": false, "includeEvents": true})
		want := ids(t, mcp.json(t)["events"], "type")
		var got []string
		cursor := ""
		for page := 0; page < 10; page++ {
			path := "/api/v1/issues/" + w.a + "/events?limit=2"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			rest := rg.rest(w.all, http.MethodGet, path, nil, nil)
			wantOK(t, "listIssueEvents", rest, http.StatusOK)
			m := rest.json(t)
			got = append(got, ids(t, m["items"], "type")...)
			if m["hasMore"] != true {
				break
			}
			cursor, _ = m["nextCursor"].(string)
		}
		sort.Strings(got)
		if len(want) != 3 || !slices.Equal(got, want) {
			t.Errorf("listIssueEvents %v, MCP %v", got, want)
		}
	})

	t.Run("listDependencies", func(t *testing.T) {
		for _, key := range []string{w.all, w.scopedP} {
			rest := rg.rest(key, http.MethodGet, "/api/v1/issues/"+w.a+"/dependencies", nil, nil)
			mcp := rg.tool(key, "skein_show", map[string]any{"id": w.a, "includeComments": false, "depsTypes": []string{"blocks"}})
			parity(t, "listDependencies", rest, mcp, "")
			deps, _ := mcp.json(t)["deps"].(map[string]any)
			got, want := stringsOf(t, rest.json(t)["items"], "toId"), stringsOf(t, deps["outgoing"], "toId")
			if !slices.Equal(got, want) || len(got) != 1 {
				t.Errorf("listDependencies %v, MCP %v", got, want)
			}
			if key == w.scopedP && slices.Contains(got, w.h) {
				t.Errorf("the scoped key sees the hidden end %s: %v", w.h, got)
			}
		}
		typed := rg.rest(w.all, http.MethodGet, "/api/v1/issues/"+w.b+"/dependencies?type=related&direction=outgoing", nil, nil)
		if got := stringsOf(t, typed.json(t)["items"], "toId"); !slices.Equal(got, []string{w.c}) {
			t.Errorf("listDependencies narrowed %v, want [%s]", got, w.c)
		}
	})

	t.Run("listComments", func(t *testing.T) {
		mcp := rg.tool(w.all, "skein_show", map[string]any{"id": w.a, "commentsLimit": 100})
		want := ids(t, mcp.json(t)["comments"], "body")
		var got []string
		cursor := ""
		for page := 0; page < 10; page++ {
			path := "/api/v1/issues/" + w.a + "/comments?limit=2"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			rest := rg.rest(w.all, http.MethodGet, path, nil, nil)
			wantOK(t, "listComments", rest, http.StatusOK)
			m := rest.json(t)
			got = append(got, ids(t, m["items"], "body")...)
			if m["hasMore"] != true {
				break
			}
			cursor, _ = m["nextCursor"].(string)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) || !slices.Equal(got, w.commentBodies) {
			t.Errorf("listComments %v, MCP %v", got, want)
		}
	})

	t.Run("getIssueMovePlan", func(t *testing.T) {
		rest := rg.rest(w.all, http.MethodGet, "/api/v1/issues/"+w.a+"/move-plan?targetProject="+w.tPrefix, nil, nil)
		mcp := rg.tool(w.all, "skein_move", map[string]any{"issueId": w.a, "targetProject": w.tPrefix})
		parity(t, "getIssueMovePlan", rest, mcp, "")
		rt, _ := rest.json(t)["targetProject"].(map[string]any)
		mt, _ := mcp.json(t)["targetProject"].(map[string]any)
		if rt["id"] != w.t.String() || mt["id"] != w.t.String() {
			t.Errorf("getIssueMovePlan target %v, MCP %v", rt["id"], mt["id"])
		}
		steps, _ := rest.json(t)["nextSteps"].([]any)
		if len(steps) != 2 {
			t.Fatalf("getIssueMovePlan steps %v", steps)
		}
		first, _ := steps[0].(map[string]any)
		if first["operationId"] != "createIssue" {
			t.Errorf("step 1 %v", first)
		}
		for _, c := range []struct {
			name, target, want string
		}{
			{"to a project in another space", w.xPrefix, "project_not_found"},
			{"to the issue's own project", w.pPrefix, "validation_error"},
			{"to a missing project", "nope-" + w.word, "project_not_found"},
		} {
			parity(t, "getIssueMovePlan "+c.name,
				rg.rest(w.all, http.MethodGet, "/api/v1/issues/"+w.a+"/move-plan?targetProject="+c.target, nil, nil),
				rg.tool(w.all, "skein_move", map[string]any{"issueId": w.a, "targetProject": c.target}), c.want)
		}
		parity(t, "getIssueMovePlan of an issue outside the key's scope",
			rg.rest(w.scopedP, http.MethodGet, "/api/v1/issues/"+w.h+"/move-plan?targetProject="+w.tPrefix, nil, nil),
			rg.tool(w.scopedP, "skein_move", map[string]any{"issueId": w.h, "targetProject": w.tPrefix}), "issue_not_found")
	})

	t.Run("search", func(t *testing.T) {
		for _, key := range []string{w.all, w.scopedP} {
			rest := rg.rest(key, http.MethodGet, "/api/v1/search?kinds=issue&limit=50&query="+w.word, nil, nil)
			mcp := rg.tool(key, "skein_search", map[string]any{"query": w.word, "kinds": []string{"issue"}, "limit": 50})
			parity(t, "search", rest, mcp, "")
			got, want := stringsOf(t, rest.json(t)["items"], "id"), stringsOf(t, mcp.json(t)["items"], "id")
			if !slices.Equal(got, want) || len(got) == 0 {
				t.Errorf("search %v, MCP %v", got, want)
			}
			if key == w.scopedP && slices.Contains(got, w.h) {
				t.Errorf("the scoped key finds the issue outside its scope: %v", got)
			}
		}
	})
}

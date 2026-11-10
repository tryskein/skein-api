package publicapi_test

// dependencies_test.go — the dependency writes of the public API
// (sk-878.3.1 T026, row 96; US-1, OST-2-5): parity with skein_dep_add and
// skein_dep_remove on the success and the refusals, the edge seen by
// listDependencies, and the origin of a missing issue — the issue of the
// path is 404, the issue a body names is 422 with the same code as MCP. The
// reference of the parity is the MCP channel itself over twin issues; of
// the origin the spec («a reference in the body to an object that does not
// exist or is outside the scope — 422 with the same code as MCP»).

import (
	"net/http"
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// edges lists «direction/type/other end» of the edges of issue as
// listDependencies gives them.
func edges(t *testing.T, rg *resourceRig, key, issue string) []string {
	t.Helper()
	c := rg.rest(key, http.MethodGet, issuePath(issue, "/dependencies"), nil, nil)
	wantOK(t, "listDependencies", c, http.StatusOK)
	items, _ := c.json(t)["items"].([]any)
	out := []string{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		other := m["toId"]
		if m["direction"] == "incoming" {
			other = m["fromId"]
		}
		out = append(out, m["direction"].(string)+"/"+m["type"].(string)+"/"+other.(string))
	}
	return out
}

// depEvents are «type/actor type» of the dependency events of the edges
// from issue written after since.
func (w *writeWorld) depEvents(from string, since int64) []string {
	w.t.Helper()
	return w.strings(`SELECT type || '/' || actor_type FROM events
 WHERE entity_type = 'dep' AND coalesce(new_data, old_data)->>'fromId' = $1 AND id > $2
 ORDER BY id`, from, since)
}

// TestDependencies_ParityWithMCP — row 96 (US-1, INV-4). addDependency and
// removeDependency asked with the same key on REST and on MCP over twin
// issues give the same code for the success and for each business refusal,
// and the same events; the edge added on REST is the edge listDependencies
// shows, and is gone after removeDependency. Red when a code or the events
// differ from MCP, or the list disagrees with the write.
func TestDependencies_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelWrite)

	type twin struct {
		name string
		// prep seeds the state around from and to.
		prep func(from, to string)
		rest func(from, to string) restCall
		mcp  func(from, to string) toolAnswer
		want string
	}
	edge := func(typ string) func(from, to string) {
		return func(from, to string) {
			w.exec(`INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`, from, to, typ, w.owner)
		}
	}
	add := func(typ string) (func(from, to string) restCall, func(from, to string) toolAnswer) {
		return func(from, to string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(from, "/dependencies"), map[string]any{"toId": to, "type": typ}, nil)
			}, func(from, to string) toolAnswer {
				return rg.tool(ko, "skein_dep_add", map[string]any{"fromId": from, "toId": to, "type": typ})
			}
	}
	remove := func(typ string) (func(from, to string) restCall, func(from, to string) toolAnswer) {
		return func(from, to string) restCall {
				return rg.rest(ko, http.MethodDelete, issuePath(from, "/dependencies/"+to+"/"+typ), nil, nil)
			}, func(from, to string) toolAnswer {
				return rg.tool(ko, "skein_dep_remove", map[string]any{"fromId": from, "toId": to, "type": typ})
			}
	}
	addRelated, mcpAddRelated := add("related")
	addBlocks, mcpAddBlocks := add("blocks")
	removeRelated, mcpRemoveRelated := remove("related")
	for _, op := range []twin{
		{name: "addDependency", rest: addRelated, mcp: mcpAddRelated},
		{name: "addDependency of an existing edge", prep: edge("related"), rest: addRelated, mcp: mcpAddRelated, want: "dependency_already_exists"},
		{name: "addDependency closing a cycle", prep: func(from, to string) { edge("blocks")(to, from) },
			rest: addBlocks, mcp: mcpAddBlocks, want: "dependency_cycle"},
		{name: "removeDependency", prep: edge("related"), rest: removeRelated, mcp: mcpRemoveRelated},
		{name: "removeDependency of no edge", rest: removeRelated, mcp: mcpRemoveRelated, want: "dependency_not_found"},
	} {
		t.Run(op.name, func(t *testing.T) {
			rf, rt, mf, mt := w.issue("from"), w.issue("to"), w.issue("from"), w.issue("to")
			if op.prep != nil {
				op.prep(rf, rt)
				op.prep(mf, mt)
			}
			since := lastEventID(t, pool)
			rest, mcp := op.rest(rf, rt), op.mcp(mf, mt)
			parity(t, op.name, rest, mcp, op.want)
			if op.want == "" {
				if rest.status != http.StatusOK && rest.status != http.StatusCreated {
					t.Fatalf("REST answered %d %s", rest.status, rest.body)
				}
				sameEvents(t, op.name, w.depEvents(rf, since), w.depEvents(mf, since))
				if got, ref := edges(t, rg, ko, rf), edges(t, rg, ko, mf); len(got) != len(ref) {
					t.Errorf("after %s REST lists %v, the MCP twin %v", op.name, got, ref)
				}
			}
		})
	}

	t.Run("the edge added is listed, the edge removed is not", func(t *testing.T) {
		from, to := w.issue("listed"), w.issue("listed")
		wantOK(t, "addDependency", rg.rest(ko, http.MethodPost, issuePath(from, "/dependencies"), map[string]any{"toId": to, "type": "related"}, nil), http.StatusCreated)
		if got := edges(t, rg, ko, from); len(got) != 1 || got[0] != "outgoing/related/"+to {
			t.Errorf("listed %v, want the one edge to %s", got, to)
		}
		wantOK(t, "removeDependency", rg.rest(ko, http.MethodDelete, issuePath(from, "/dependencies/"+to+"/related"), nil, nil), http.StatusOK)
		if got := edges(t, rg, ko, from); len(got) != 0 {
			t.Errorf("listed %v after the removal, want none", got)
		}
	})
}

// TestDependencies_OriginOfAMissingIssue — row 96 (OST-2-5). The issue of
// the path that does not exist or is outside the key's scope is 404; the
// issue toId names is the same code with 422 — the code MCP answers for it.
// Red when the reference of the body answers 404.
func TestDependencies_OriginOfAMissingIssue(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	q, qPrefix := seedProject(t, pool, w.member, w.space, "dpo")
	addMember(t, pool, q, w.owner, "member")
	qIssue := testsupport.SeedIssue(t, pool, q, w.member, qPrefix+"-1", "outside", "open", 2)
	scoped := projectsKey(t, pool, w.owner, apikeys.LevelWrite, w.project)
	inside := w.issue("inside")
	missing := w.prefix + "-99999"

	for _, c := range []struct{ name, from, to string }{
		{"a missing issue of the path", missing, inside},
		{"an issue of the path outside the scope", qIssue, inside},
	} {
		got := rg.rest(scoped, http.MethodPost, issuePath(c.from, "/dependencies"), map[string]any{"toId": c.to, "type": "related"}, nil)
		ref := rg.tool(scoped, "skein_dep_add", map[string]any{"fromId": c.from, "toId": c.to, "type": "related"})
		if got.status != http.StatusNotFound || got.code() != ref.code || ref.code != "issue_not_found" {
			t.Errorf("%s: REST %d %s, MCP %q — want 404 with the MCP code issue_not_found", c.name, got.status, got.body, ref.code)
		}
	}
	for _, c := range []struct{ name, to string }{
		{"a missing toId", missing},
		{"a toId outside the scope", qIssue},
	} {
		got := rg.rest(scoped, http.MethodPost, issuePath(inside, "/dependencies"), map[string]any{"toId": c.to, "type": "related"}, nil)
		ref := rg.tool(scoped, "skein_dep_add", map[string]any{"fromId": inside, "toId": c.to, "type": "related"})
		if got.status != http.StatusUnprocessableEntity || got.code() != ref.code || ref.code == "" {
			t.Errorf("%s: REST %d %s, MCP %q — want 422 with the MCP code", c.name, got.status, got.body, ref.code)
		}
	}
	if got := edges(t, rg, w.key(w.owner, apikeys.LevelRead), inside); len(got) != 0 {
		t.Errorf("refused writes left edges %v", got)
	}
}

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
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
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
				if rest.status != http.StatusOK && rest.status != http.StatusCreated && rest.status != http.StatusNoContent {
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
		wantOK(t, "removeDependency", rg.rest(ko, http.MethodDelete, issuePath(from, "/dependencies/"+to+"/related"), nil, nil), http.StatusNoContent)
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

// TestDependencies_CyclePathFollowsTheKeyScope — sk-1007 No. 47. Reference —
// spec US-4 and SC-004: on the public API (addDependency,
// bulkAddDependencies, reparentIssue) and on their MCP twins called with the
// same key, a cycle through an issue outside the key's scope — H, which the
// key's owner sees — answers the code and status of a visible cycle without
// a path and without any id of H; a key of scope all gives the full path.
//
// Fixture: P-2 → H-5 → H-9 → P-1 as blocks and as parent-child edges; the
// requests add P-1 → P-2 or move P-1 under P-2, closing
// P-1 → P-2 → H-5 → H-9 → P-1. A refusal writes nothing, so every surface
// runs over the same graph.
func TestDependencies_CyclePathFollowsTheKeyScope(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	h, hPrefix := seedProject(t, pool, w.owner, w.space, "wrh")
	t.Cleanup(func() {
		w.exec(`DELETE FROM dependencies WHERE from_id IN (SELECT id FROM issues WHERE project_id = $1)
			OR to_id IN (SELECT id FROM issues WHERE project_id = $1)`, h)
	})
	p1, p2 := w.issue("p1"), w.issue("p2")
	h5 := testsupport.SeedIssue(t, pool, h, w.owner, hPrefix+"-5", "h5", "open", 2)
	h9 := testsupport.SeedIssue(t, pool, h, w.owner, hPrefix+"-9", "h9", "open", 2)
	for _, typ := range []string{"blocks", "parent-child"} {
		for _, e := range [][2]string{{p2, h5}, {h5, h9}, {h9, p1}} {
			w.exec(`INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`, e[0], e[1], typ, w.owner)
		}
	}
	wantPath := []any{p1, p2, h5, h9, p1}
	keyP := projectsKey(t, pool, w.owner, apikeys.LevelWrite, w.project)
	keyAll := projectsKey(t, pool, w.owner, apikeys.LevelWrite)

	decode := func(t *testing.T, raw []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("not a JSON object: %v (%s)", err, raw)
		}
		return m
	}
	// rowError is the error object of the only row of a batch answer.
	rowError := func(t *testing.T, m map[string]any) (code any, details map[string]any) {
		t.Helper()
		rows, _ := m["results"].([]any)
		if len(rows) != 1 {
			t.Fatalf("batch results = %v, want one row", m["results"])
		}
		errObj, _ := rows[0].(map[string]any)["error"].(map[string]any)
		details, _ = errObj["details"].(map[string]any)
		return errObj["code"], details
	}
	// surface returns the status (0 on MCP), the code, the object that
	// carries the path, and the raw answer.
	type answer struct {
		status  int
		code    any
		details map[string]any
		raw     string
	}
	item := map[string]any{"fromId": p1, "toId": p2, "type": "blocks"}
	surfaces := []struct {
		name string
		call func(t *testing.T, key string) answer
	}{
		{"addDependency", func(t *testing.T, key string) answer {
			c := rg.rest(key, http.MethodPost, issuePath(p1, "/dependencies"), map[string]any{"toId": p2, "type": "blocks"}, nil)
			m := decode(t, c.body)
			return answer{c.status, m["code"], m, string(c.body)}
		}},
		{"bulkAddDependencies", func(t *testing.T, key string) answer {
			c := rg.rest(key, http.MethodPost, "/api/v1/issues/dependencies/bulk-add", map[string]any{"items": []any{item}}, nil)
			code, details := rowError(t, decode(t, c.body))
			return answer{c.status, code, details, string(c.body)}
		}},
		{"reparentIssue", func(t *testing.T, key string) answer {
			c := rg.rest(key, http.MethodPost, issuePath(p1, "/reparent"), map[string]any{"newParentId": p2}, ifMatch(w.version(p1)))
			m := decode(t, c.body)
			return answer{c.status, m["code"], m, string(c.body)}
		}},
		{"skein_dep_add", func(t *testing.T, key string) answer {
			a := rg.tool(key, "skein_dep_add", item)
			m := decode(t, []byte(a.text))
			return answer{0, m["error"], m, a.text}
		}},
		{"skein_dep_add_many", func(t *testing.T, key string) answer {
			a := rg.tool(key, "skein_dep_add_many", map[string]any{"items": []any{item}})
			code, details := rowError(t, decode(t, []byte(a.text)))
			return answer{0, code, details, a.text}
		}},
		{"skein_reparent", func(t *testing.T, key string) answer {
			a := rg.tool(key, "skein_reparent", map[string]any{"id": p1, "newParentId": p2, "expectedVersion": w.version(p1)})
			m := decode(t, []byte(a.text))
			return answer{0, m["error"], m, a.text}
		}},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			scoped := s.call(t, keyP)
			if scoped.code != "dependency_cycle" {
				t.Fatalf("key projects=[P]: code %v, want dependency_cycle (%s)", scoped.code, scoped.raw)
			}
			if v, ok := scoped.details["path"]; ok {
				t.Errorf("key projects=[P]: path = %v, want no key", v)
			}
			for _, id := range []string{h5, h9} {
				if strings.Contains(scoped.raw, `"`+id+`"`) {
					t.Errorf("key projects=[P] names %s: %s", id, scoped.raw)
				}
			}

			all := s.call(t, keyAll)
			if all.code != "dependency_cycle" || all.status != scoped.status {
				t.Errorf("key all: code %v status %d, want dependency_cycle and the scoped key's status %d",
					all.code, all.status, scoped.status)
			}
			if got := all.details["path"]; !reflect.DeepEqual(got, wantPath) {
				t.Errorf("key all: path %v, want %v", got, wantPath)
			}
		})
	}
}

package main

// links_space_boundary_test.go — sk-674.18, the test delivery of row 142
// (T033, INV-24, INV-3). Reference — the invariants as the spec words them,
// written before the code: a parent, a dependency, the target of a
// duplicate or superseded close, a move and an agent's copy into another
// project stay inside the space of the issue; an attempt with an issue or a
// project of another space is refused WITH THE SAME ANSWER as a
// nonexistent one. The property is the sameness of the answer, so every
// cell compares the two answers instead of naming a code.
//
// Actor uAB of testsupport.SeedTenancy sees space A and space B and may
// write in both, so «another space» is not «invisible»: the only thing that
// can refuse him is the space boundary. The issue acts in A.space; the
// target is an issue (or the project) of B.space; the nonexistent target is
// an id nobody has. Positive controls: the same operations with a target of
// the issue's own space go through. Both channels run through the
// production router.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"skein-api/internal/testsupport"
)

func TestLinksSpaceBoundary_OtherSpaceAnswersAsNonexistent(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	actor := testsupport.ActorBoth
	a, aMembers, b := s.f.ASpace, s.f.AMembers, s.f.BSpace
	sid := s.session(actor)

	next := func(p testsupport.TenancyProject) string {
		var n int
		if err := s.pool.QueryRow(ctx, `UPDATE project_counters SET counter = counter + 1 WHERE project_id = $1 RETURNING counter`,
			p.ID).Scan(&n); err != nil {
			t.Fatalf("counter of %s: %v", p.IDPrefix, err)
		}
		id := fmt.Sprintf("%s-%d", p.IDPrefix, n)
		return testsupport.SeedIssue(t, s.pool, p.ID, p.OwnerID, id, "boundary "+id, "open", 2)
	}
	version := func(id string) int {
		var v int
		if err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("version of %s: %v", id, err)
		}
		return v
	}
	otherSpace := next(b)
	sameSpace := next(aMembers) // another project of the issue's space
	missing := a.IDPrefix + "-999999"

	// answer is the comparable part of a refusal: REST — status and code;
	// MCP — code and message (details name the id asked for and so differ
	// by construction).
	type answer struct {
		ok      bool
		status  int
		code    string
		message string
	}
	restAnswer := func(status int, raw []byte) answer {
		if status >= 200 && status < 300 {
			return answer{ok: true, status: status}
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		return answer{status: status, code: fmt.Sprint(body["error"])}
	}
	toolAnswer := func(r toolResult) answer {
		if !r.isError {
			return answer{ok: true}
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(r.text), &body)
		return answer{code: fmt.Sprint(body["error"]), message: fmt.Sprint(body["message"])}
	}
	same := func(t *testing.T, what string, got, nonexistent answer) {
		t.Helper()
		if got.ok {
			t.Errorf("%s with an issue of another space succeeded", what)
			return
		}
		if got != nonexistent {
			t.Errorf("%s: another space answers %+v, a nonexistent target %+v — distinguishable", what, got, nonexistent)
		}
	}
	allowed := func(t *testing.T, what string, got answer) {
		t.Helper()
		if !got.ok {
			t.Errorf("%s with a target of the issue's own space was refused %+v (positive control)", what, got)
		}
	}
	base := "/api/v1/projects/" + a.ID.String() + "/issues/"

	t.Run("dependency REST", func(t *testing.T) {
		from := next(a)
		dep := func(to string) answer {
			return restAnswer(s.rest(actor, http.MethodPost, base+from+"/dependencies", map[string]any{"toId": to, "type": "related"}))
		}
		same(t, "dependency", dep(otherSpace), dep(missing))
		allowed(t, "dependency", dep(sameSpace))
	})
	t.Run("dependency MCP", func(t *testing.T) {
		from := next(a)
		dep := func(to string) answer {
			return toolAnswer(s.tool(actor, sid, "skein_dep_add", map[string]any{"fromId": from, "toId": to, "type": "related"}))
		}
		same(t, "skein_dep_add", dep(otherSpace), dep(missing))
		allowed(t, "skein_dep_add", dep(sameSpace))
	})
	t.Run("parent on create REST", func(t *testing.T) {
		create := func(parent string) answer {
			return restAnswer(s.rest(actor, http.MethodPost, base, map[string]any{"title": "rest child " + parent, "parentId": parent}))
		}
		same(t, "create with parent", create(otherSpace), create(missing))
		allowed(t, "create with parent", create(next(a)))
	})
	t.Run("parent on create MCP", func(t *testing.T) {
		create := func(parent string) answer {
			return toolAnswer(s.tool(actor, sid, "skein_create", map[string]any{
				// force skips the duplicate gate (decision_required is not an
				// answer of the boundary); the title is unique per call.
				"project": a.ID.String(), "title": "mcp child " + parent, "parentId": parent,
				"kind": "directed", "type": "task", "unassigned": true, "force": true,
			}))
		}
		same(t, "skein_create with parentId", create(otherSpace), create(missing))
		allowed(t, "skein_create with parentId", create(next(a)))
	})
	t.Run("reparent REST", func(t *testing.T) {
		child := next(a)
		reparent := func(parent string) answer {
			return restAnswer(s.rest(actor, http.MethodPost, base+child+"/reparent",
				map[string]any{"newParentId": parent, "expectedVersion": version(child)}))
		}
		same(t, "reparent", reparent(otherSpace), reparent(missing))
		allowed(t, "reparent", reparent(next(a)))
	})
	t.Run("reparent MCP", func(t *testing.T) {
		child := next(a)
		reparent := func(parent string) answer {
			return toolAnswer(s.tool(actor, sid, "skein_reparent", map[string]any{
				"id": child, "newParentId": parent, "expectedVersion": version(child)}))
		}
		same(t, "skein_reparent", reparent(otherSpace), reparent(missing))
		allowed(t, "skein_reparent", reparent(next(a)))
	})
	t.Run("close as duplicate REST", func(t *testing.T) {
		closeAs := func(target string) answer {
			issue := next(a)
			return restAnswer(s.restWith(actor, http.MethodPost, base+issue+"/transitions",
				map[string]any{"status": "closed", "closeOutcome": "duplicate", "closeTarget": target},
				map[string]string{"If-Match": fmt.Sprint(version(issue))}))
		}
		same(t, "close as duplicate", closeAs(otherSpace), closeAs(missing))
		allowed(t, "close as duplicate", closeAs(sameSpace))
	})
	t.Run("close as superseded MCP", func(t *testing.T) {
		closeAs := func(target string) answer {
			issue := next(a)
			return toolAnswer(s.tool(actor, sid, "skein_close", map[string]any{
				"id": issue, "expectedVersion": version(issue), "closeOutcome": "superseded",
				"closeTarget": target, "reason": "boundary"}))
		}
		same(t, "skein_close superseded", closeAs(otherSpace), closeAs(missing))
		allowed(t, "skein_close superseded", closeAs(sameSpace))
	})
	t.Run("move and the agent's copy MCP", func(t *testing.T) {
		issue := next(a)
		move := func(project string) answer {
			return toolAnswer(s.tool(actor, sid, "skein_move", map[string]any{"issueId": issue, "targetProject": project}))
		}
		same(t, "skein_move", move(b.ID.String()), move("00000000-0000-4000-8000-00000000abcd"))
		allowed(t, "skein_move", move(aMembers.ID.String()))
		// The copy itself: skein_create in another space with the supersedes
		// edge back to the issue — the edge is refused as a nonexistent
		// target, so nothing is created.
		copyTo := func(project, source string) answer {
			return toolAnswer(s.tool(actor, sid, "skein_create", map[string]any{
				"project": project, "title": "copy of " + source + " into " + project,
				"kind": "directed", "type": "task", "unassigned": true, "force": true,
				"dependencies": []any{map[string]any{"toId": source, "type": "supersedes"}},
			}))
		}
		same(t, "copy into another space", copyTo(b.ID.String(), issue), copyTo(b.ID.String(), b.IDPrefix+"-999999"))
		allowed(t, "copy inside the space", copyTo(aMembers.ID.String(), issue))
	})
}

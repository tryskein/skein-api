package main

// issue_write_actions_test.go — sk-674.18 T066 (sk-689.37), changed by
// sk-741: «for every issue write path — a permission to a participant of the
// project AND to an instance admin without membership». Reference — the
// action table of US-6 as changed by the owner 2026-10-17 (sk-741): in a
// project of a non-personal space the instance admin has the project
// owner's rights — creates, comments, edits issues and may be the assignee —
// without a membership row. Before sk-741 every cell answered the admin 403
// `forbidden` in REST and `forbidden` in MCP (the production defect).
//
// Project A.members of testsupport.SeedTenancy; actors adm0 (an instance
// admin with no membership, neither in the project nor in its space) and
// uA (a member of the project). Both channels go through the production
// router. Every cell acts on its own fresh issue, so the order of cells
// changes nothing. Attachment writes are not here: the production router has
// no object storage in tests — they are in handler/attachments_actions_test.go
// and mcp/tools/attachment_actions_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"skein-api/internal/testsupport"
)

func TestIssueWritePaths_FollowTheActionTable(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	p := s.f.AMembers
	base := "/api/v1/projects/" + p.ID.String() + "/issues"
	testsupport.SeedRepo(t, s.pool, p.ID, "t066", "https://git.example.test/t066.git")

	// fresh takes the next number of the project's counter, as a create
	// does, so seeded ids never collide with issues the cells create.
	fresh := func() string {
		var n int
		if err := s.pool.QueryRow(ctx, `UPDATE project_counters SET counter = counter + 1
 WHERE project_id = $1 RETURNING counter`, p.ID).Scan(&n); err != nil {
			t.Fatalf("next number of %s: %v", p.IDPrefix, err)
		}
		id := fmt.Sprintf("%s-%d", p.IDPrefix, n)
		return testsupport.SeedIssue(t, s.pool, p.ID, p.OwnerID, id, "t066 "+id, "open", 2)
	}
	version := func(id string) int {
		var v int
		if err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("version of %s: %v", id, err)
		}
		return v
	}
	ifMatch := func(id string) map[string]string {
		return map[string]string{"If-Match": fmt.Sprint(version(id))}
	}
	exec := func(q string, args ...any) {
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// outcome is what one cell answered: allowed, or the refusal's code.
	type outcome struct {
		allowed bool
		code    string
		raw     string
	}
	fromREST := func(status int, raw []byte) outcome {
		if status >= 200 && status < 300 {
			return outcome{allowed: true, raw: string(raw)}
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		return outcome{code: fmt.Sprintf("%d %v", status, body["error"]), raw: string(raw)}
	}
	fromTool := func(r toolResult) outcome {
		var body map[string]any
		_ = json.Unmarshal([]byte(r.text), &body)
		if r.isError {
			return outcome{code: fmt.Sprint(body["error"]), raw: r.text}
		}
		// A batch answers ok with one row per item; its single row decides.
		if rows, ok := body["results"].([]any); ok {
			if len(rows) != 1 {
				return outcome{code: "rows≠1", raw: r.text}
			}
			row, _ := rows[0].(map[string]any)
			if ok, _ := row["ok"].(bool); ok {
				return outcome{allowed: true, raw: r.text}
			}
			e, _ := row["error"].(map[string]any)
			return outcome{code: fmt.Sprint(e["code"]), raw: r.text}
		}
		return outcome{allowed: true, raw: r.text}
	}

	type path struct {
		name string
		// rest or mcp performs the write as actor a on its own fresh issue.
		rest func(a testsupport.Actor) outcome
		mcp  func(a testsupport.Actor, sid string) outcome
	}
	user := func(a testsupport.Actor) testsupport.TenancyUser { return s.f.User(t, a) }
	paths := []path{
		// ---- REST ----
		{name: "REST create issue", rest: func(a testsupport.Actor) outcome {
			return fromREST(s.rest(a, http.MethodPost, base, map[string]any{"title": "t066 rest create"}))
		}},
		{name: "REST create issue with assignee", rest: func(a testsupport.Actor) outcome {
			return fromREST(s.rest(a, http.MethodPost, base, map[string]any{"title": "t066 rest create", "assignee": user(a).Username}))
		}},
		{name: "REST edit issue", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.restWith(a, http.MethodPatch, base+"/"+id, map[string]any{"title": "t066 edited"}, ifMatch(id)))
		}},
		{name: "REST set assignee", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.restWith(a, http.MethodPatch, base+"/"+id, map[string]any{"assignee": user(a).Username}, ifMatch(id)))
		}},
		{name: "REST transition", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.restWith(a, http.MethodPost, base+"/"+id+"/transitions", map[string]any{"status": "deferred"}, ifMatch(id)))
		}},
		{name: "REST pin", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.restWith(a, http.MethodPost, base+"/"+id+"/pin", nil, ifMatch(id)))
		}},
		{name: "REST unpin", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			testsupport.SetPinned(t, s.pool, id, true)
			return fromREST(s.restWith(a, http.MethodPost, base+"/"+id+"/unpin", nil, ifMatch(id)))
		}},
		{name: "REST replace components", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.rest(a, http.MethodPut, base+"/"+id+"/components", map[string]any{"componentIds": []string{}}))
		}},
		{name: "REST add label", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.rest(a, http.MethodPost, base+"/"+id+"/labels", map[string]any{"label": "t066"}))
		}},
		{name: "REST remove label", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			exec(`INSERT INTO issue_labels (issue_id, label) VALUES ($1, 't066')`, id)
			return fromREST(s.rest(a, http.MethodDelete, base+"/"+id+"/labels/t066", nil))
		}},
		{name: "REST add comment", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.rest(a, http.MethodPost, base+"/"+id+"/comments", map[string]any{"body": "t066 comment"}))
		}},
		{name: "REST edit own comment", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			cid := testsupport.SeedComment(t, s.pool, id, user(a).ID, "t066 own")
			return fromREST(s.rest(a, http.MethodPut, base+"/"+id+"/comments/"+cid.String(), map[string]any{"body": "t066 edited", "expectedVersion": 1}))
		}},
		{name: "REST delete own comment", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			cid := testsupport.SeedComment(t, s.pool, id, user(a).ID, "t066 own")
			return fromREST(s.rest(a, http.MethodDelete, base+"/"+id+"/comments/"+cid.String(), nil))
		}},
		{name: "REST add dependency", rest: func(a testsupport.Actor) outcome {
			id, to := fresh(), fresh()
			return fromREST(s.rest(a, http.MethodPost, base+"/"+id+"/dependencies", map[string]any{"toId": to, "type": "related"}))
		}},
		{name: "REST remove dependency", rest: func(a testsupport.Actor) outcome {
			id, to := fresh(), fresh()
			testsupport.SeedDep(t, s.pool, id, to, "related", p.OwnerID)
			return fromREST(s.rest(a, http.MethodDelete, base+"/"+id+"/dependencies/"+to+"/related", nil))
		}},
		{name: "REST reparent", rest: func(a testsupport.Actor) outcome {
			id, parent := fresh(), fresh()
			return fromREST(s.rest(a, http.MethodPost, base+"/"+id+"/reparent", map[string]any{"newParentId": parent, "expectedVersion": version(id)}))
		}},
		{name: "REST bulk", rest: func(a testsupport.Actor) outcome {
			id := fresh()
			return fromREST(s.rest(a, http.MethodPost, base+"/bulk", map[string]any{"ids": []string{id}, "action": "pin"}))
		}},

		// ---- MCP ----
		{name: "MCP skein_create", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_create", map[string]any{"project": p.IDPrefix, "title": "t066 mcp create", "type": "task", "kind": "directed"}))
		}},
		{name: "MCP skein_create with assignee", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_create", map[string]any{"project": p.IDPrefix, "title": "t066 mcp create", "type": "task", "kind": "directed", "assignee": user(a).Username}))
		}},
		{name: "MCP skein_epic_create", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_epic_create", map[string]any{"project": p.IDPrefix, "title": "t066 epic", "kind": "directed"}))
		}},
		{name: "MCP skein_update", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			return fromTool(s.tool(a, sid, "skein_update", map[string]any{"id": id, "expectedVersion": version(id), "title": "t066 edited"}))
		}},
		{name: "MCP skein_update assignee", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			return fromTool(s.tool(a, sid, "skein_update", map[string]any{"id": id, "expectedVersion": version(id), "assignee": user(a).Username}))
		}},
		{name: "MCP skein_update_many", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			return fromTool(s.tool(a, sid, "skein_update_many", map[string]any{"items": []any{
				map[string]any{"id": id, "expectedVersion": version(id), "priority": 1},
			}}))
		}},
		{name: "MCP skein_close", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			return fromTool(s.tool(a, sid, "skein_close", map[string]any{"id": id, "expectedVersion": version(id), "reason": "t066", "closeOutcome": "done"}))
		}},
		{name: "MCP skein_close_many", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			return fromTool(s.tool(a, sid, "skein_close_many", map[string]any{"items": []any{
				map[string]any{"id": id, "expectedVersion": version(id), "reason": "t066", "closeOutcome": "done"},
			}}))
		}},
		{name: "MCP skein_claim", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_claim", map[string]any{"id": fresh()}))
		}},
		{name: "MCP skein_unclaim (own claim)", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			// The caller releases a claim of his own session.
			if r := s.tool(a, sid, "skein_claim", map[string]any{"id": id}); r.isError {
				t.Fatalf("claim before unclaim: %s", r.text)
			}
			return fromTool(s.tool(a, sid, "skein_unclaim", map[string]any{"id": id, "nextStatus": "open", "reason": "t066"}))
		}},
		{name: "MCP skein_comment", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_comment", map[string]any{"issueId": fresh(), "body": "t066 comment", "kind": "directed"}))
		}},
		{name: "MCP skein_comment_update (own comment)", mcp: func(a testsupport.Actor, sid string) outcome {
			cid := testsupport.SeedComment(t, s.pool, fresh(), user(a).ID, "t066 own")
			return fromTool(s.tool(a, sid, "skein_comment_update", map[string]any{"commentId": cid.String(), "body": "t066 edited", "expectedVersion": 1, "kind": "directed"}))
		}},
		{name: "MCP skein_comment_delete (own comment)", mcp: func(a testsupport.Actor, sid string) outcome {
			cid := testsupport.SeedComment(t, s.pool, fresh(), user(a).ID, "t066 own")
			return fromTool(s.tool(a, sid, "skein_comment_delete", map[string]any{"commentId": cid.String()}))
		}},
		{name: "MCP skein_commits_attach", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_commits_attach", map[string]any{"issueId": fresh(), "commits": []any{map[string]any{"hash": "abcdef1"}}}))
		}},
		{name: "MCP skein_commits_detach", mcp: func(a testsupport.Actor, sid string) outcome {
			id := fresh()
			exec(`INSERT INTO issue_commits (issue_id, repo_id, hash, message, linked_by_kind)
SELECT $1, (p.git_repos->0->>'id')::uuid, 'abcdef1', 't066', 'system' FROM projects p WHERE p.id = $2`, id, p.ID)
			return fromTool(s.tool(a, sid, "skein_commits_detach", map[string]any{"issueId": id, "hashes": []any{"abcdef1"}}))
		}},
		{name: "MCP skein_dep_add", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_dep_add", map[string]any{"fromId": fresh(), "toId": fresh(), "type": "related"}))
		}},
		{name: "MCP skein_dep_add_many", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_dep_add_many", map[string]any{"items": []any{
				map[string]any{"fromId": fresh(), "toId": fresh(), "type": "related"},
			}}))
		}},
		{name: "MCP skein_dep_remove", mcp: func(a testsupport.Actor, sid string) outcome {
			id, to := fresh(), fresh()
			testsupport.SeedDep(t, s.pool, id, to, "related", p.OwnerID)
			return fromTool(s.tool(a, sid, "skein_dep_remove", map[string]any{"fromId": id, "toId": to, "type": "related"}))
		}},
		{name: "MCP skein_reparent", mcp: func(a testsupport.Actor, sid string) outcome {
			id, parent := fresh(), fresh()
			return fromTool(s.tool(a, sid, "skein_reparent", map[string]any{"id": id, "newParentId": parent, "expectedVersion": version(id)}))
		}},
		{name: "MCP skein_move", mcp: func(a testsupport.Actor, sid string) outcome {
			return fromTool(s.tool(a, sid, "skein_move", map[string]any{"issueId": fresh(), "targetProject": s.f.ASpace.IDPrefix}))
		}},
	}

	// The table allows every path to both actors.
	for _, actor := range []testsupport.Actor{testsupport.ActorAdmin0, testsupport.ActorOnlyA} {
		t.Run(string(actor), func(t *testing.T) {
			sid := s.session(actor)
			for _, pth := range paths {
				t.Run(pth.name, func(t *testing.T) {
					var got outcome
					if pth.rest != nil {
						got = pth.rest(actor)
					} else {
						got = pth.mcp(actor, sid)
					}
					if !got.allowed {
						t.Errorf("the table allows it, got refusal %s: %s", got.code, got.raw)
					}
				})
			}
		})
	}
}

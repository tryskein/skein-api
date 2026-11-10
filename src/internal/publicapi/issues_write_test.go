package publicapi_test

// issues_write_test.go — the writes of issues of the public API (sk-878.3.1
// T024): parity with the MCP tools (row 84), nothing written on a refusal
// (row 85), the duplicate gate (row 87), two claims at once (row 88), the
// release of a lock (row 89), force and the key right (row 90), claimIssues
// (row 91), the origin of a missing object (row 92), the locks of an old key
// (row 93), objects outside the key's scope (row 130), labels and the
// version (row 133), the executor seen by readers (row 134). The reference
// of the parity is the MCP channel itself, over twin issues.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// writeWorld is the seed of the write tests: an owner with a project of
// his space, a member of it, another user.
type writeWorld struct {
	t        *testing.T
	pool     *pgxpool.Pool
	owner    uuid.UUID
	ownerNm  string
	member   uuid.UUID
	memberNm string
	other    uuid.UUID
	otherNm  string
	space    uuid.UUID
	project  uuid.UUID
	prefix   string
	repoURL  string
	n        int
}

func seedWriteWorld(t *testing.T, pool *pgxpool.Pool) *writeWorld {
	t.Helper()
	w := &writeWorld{t: t, pool: pool}
	w.owner, w.ownerNm = testsupport.SeedUser(t, pool, "")
	w.member, w.memberNm = testsupport.SeedUser(t, pool, "")
	w.other, w.otherNm = testsupport.SeedUser(t, pool, "")
	w.space, _ = seedOwnSpace(t, pool, w.owner)
	testsupport.AddSpaceMember(t, pool, w.space, w.member)
	testsupport.AddSpaceMember(t, pool, w.space, w.other)
	w.project, w.prefix = seedProject(t, pool, w.owner, w.space, "wrp")
	addMember(t, pool, w.project, w.member, "member")
	w.repoURL = "https://git.example.test/" + w.prefix + ".git"
	w.exec(`UPDATE projects SET git_repos = jsonb_build_array(jsonb_build_object('id', $2::text, 'name', 'main', 'url', $3::text)) WHERE id = $1`,
		w.project, uuid.NewString(), w.repoURL)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM events WHERE project_id = $1`, w.project)
		_, _ = pool.Exec(ctx, `DELETE FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, w.project)
		_, _ = pool.Exec(ctx, `DELETE FROM dependencies WHERE from_id IN (SELECT id FROM issues WHERE project_id = $1)
			OR to_id IN (SELECT id FROM issues WHERE project_id = $1)`, w.project)
	})
	return w
}

func (w *writeWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatalf("exec %q: %v", sql, err)
	}
}

// issue seeds an open issue of the project and returns its id.
func (w *writeWorld) issue(title string) string {
	w.t.Helper()
	w.n++
	return testsupport.SeedIssue(w.t, w.pool, w.project, w.owner,
		w.prefix+"-"+strconv.Itoa(900+w.n), title+" "+uuid.NewString()[:8], "open", 2)
}

func (w *writeWorld) key(user uuid.UUID, l apikeys.Level) string {
	return projectsKey(w.t, w.pool, user, l)
}

func (w *writeWorld) version(id string) int {
	w.t.Helper()
	var v int
	if err := w.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		w.t.Fatalf("read version of %s: %v", id, err)
	}
	return v
}

func (w *writeWorld) lockedBy(id string) string {
	w.t.Helper()
	var l *string
	if err := w.pool.QueryRow(context.Background(), `SELECT locked_by FROM issues WHERE id = $1`, id).Scan(&l); err != nil {
		w.t.Fatalf("read lock of %s: %v", id, err)
	}
	if l == nil {
		return ""
	}
	return *l
}

func (w *writeWorld) events(id string) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE entity_id = $1`, id).Scan(&n); err != nil {
		w.t.Fatalf("count events: %v", err)
	}
	return n
}

// issueEvents lists the event types of an issue after since.
func (w *writeWorld) issueEvents(id string, since int64) []string {
	w.t.Helper()
	rows, err := w.pool.Query(context.Background(), `SELECT type || '/' || actor_type FROM events WHERE entity_id = $1 AND id > $2 ORDER BY id`, id, since)
	if err != nil {
		w.t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// treeEvents is issueEvents of the issue and its children (an epic's tree).
func (w *writeWorld) treeEvents(id string, since int64) []string {
	w.t.Helper()
	return w.strings(`SELECT type || '/' || actor_type FROM events
 WHERE entity_id IN (SELECT i.id FROM issues i WHERE i.id = $1 OR i.parent_id = $1) AND id > $2
 ORDER BY (entity_id = $1) DESC, type, actor_type`, id, since)
}

// treeNotifications are the notifications of the events of the issue and of
// its children after since: «event type/recipient», sorted.
func (w *writeWorld) treeNotifications(id string, since int64) []string {
	w.t.Helper()
	return w.strings(`SELECT e.type || '/' || n.user_id::text FROM notifications n JOIN events e ON e.id = n.event_id
 WHERE e.entity_id IN (SELECT i.id FROM issues i WHERE i.id = $1 OR i.parent_id = $1) AND e.id > $2
 ORDER BY 1`, id, since)
}

func (w *writeWorld) strings(sql string, args ...any) []string {
	w.t.Helper()
	rows, err := w.pool.Query(context.Background(), sql, args...)
	if err != nil {
		w.t.Fatalf("read %q: %v", sql, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatalf("read %q: %v", sql, err)
	}
	return out
}

// sameNotifications asserts that both channels notified the same people
// of the same events.
func sameNotifications(t *testing.T, op string, rest, mcp []string) {
	t.Helper()
	if !slices.Equal(rest, mcp) {
		t.Errorf("%s: REST notified %v, MCP notified %v", op, rest, mcp)
	}
}

func ifMatch(v int) map[string]string {
	return map[string]string{"If-Match": `"` + strconv.Itoa(v) + `"`}
}

func issuePath(id, rest string) string { return "/api/v1/issues/" + id + rest }

// TestIssuesWrite_ParityWithMCP — row 84 (US-1, SC-001, INV-4). Each write
// of T024 asked with the same key on REST and on MCP over twin issues gives
// the same code for the success and for its business refusals, and the
// same events. Red when a code or the events differ from MCP.
func TestIssuesWrite_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelManage)
	km := w.key(w.member, apikeys.LevelManage)
	// The member watches the project: every write of the owner below
	// notifies somebody, so the notifications are compared, not two empty
	// lists.
	w.exec(`INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, w.project, w.member)

	// twin runs op on two fresh issues, one per channel, and compares the
	// code and, for a success, the events and the notifications.
	type twinOp struct {
		name  string
		prep  func(id string)
		rest  func(id string) restCall
		mcp   func(id string) toolAnswer
		want  string
		write bool
	}
	run := func(op twinOp) {
		t.Run(op.name, func(t *testing.T) {
			r, m := w.issue(op.name), w.issue(op.name)
			if op.prep != nil {
				op.prep(r)
				op.prep(m)
			}
			since := lastEventID(t, pool)
			rest, mcp := op.rest(r), op.mcp(m)
			parity(t, op.name, rest, mcp, op.want)
			if op.want == "" && op.write {
				sameEvents(t, op.name, w.issueEvents(r, since), w.issueEvents(m, since))
				sameNotifications(t, op.name, w.treeNotifications(r, since), w.treeNotifications(m, since))
			}
		})
	}
	assignTo := func(user string) func(string) {
		return func(id string) { w.exec(`UPDATE issues SET assignee = $2 WHERE id = $1`, id, user) }
	}
	claimedByOther := func(id string) {
		w.exec(`UPDATE issues SET locked_by = 'mcp-session-other', locked_at = now(), status = 'in_progress' WHERE id = $1`, id)
	}
	for _, op := range []twinOp{
		{name: "updateIssue", write: true,
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "changed"}, ifMatch(w.version(id)))
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_update", map[string]any{"id": id, "expectedVersion": w.version(id), "title": "changed"})
			}},
		{name: "updateIssue locked by another", prep: claimedByOther, want: "locked_by_other",
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "x"}, ifMatch(w.version(id)))
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_update", map[string]any{"id": id, "expectedVersion": w.version(id), "title": "x"})
			}},
		{name: "updateIssue to an invalid status", want: "validation_error",
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPatch, issuePath(id, ""), map[string]any{"status": "nonsense"}, ifMatch(w.version(id)))
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_update", map[string]any{"id": id, "expectedVersion": w.version(id), "status": "nonsense"})
			}},
		{name: "claimIssue", write: true,
			rest: func(id string) restCall { return rg.rest(ko, http.MethodPost, issuePath(id, "/claim"), nil, nil) },
			mcp:  func(id string) toolAnswer { return rg.tool(ko, "skein_claim", map[string]any{"id": id}) }},
		{name: "claimIssue of a claimed issue", prep: claimedByOther, want: "already_claimed",
			rest: func(id string) restCall { return rg.rest(ko, http.MethodPost, issuePath(id, "/claim"), nil, nil) },
			mcp:  func(id string) toolAnswer { return rg.tool(ko, "skein_claim", map[string]any{"id": id}) }},
		{name: "unclaimIssue by a non-holder", prep: claimedByOther, want: "not_lock_holder",
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/unclaim"), map[string]any{"nextStatus": "open", "reason": "r"}, nil)
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_unclaim", map[string]any{"id": id, "nextStatus": "open", "reason": "r"})
			}},
		{name: "closeIssue", write: true,
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/close"), map[string]any{"reason": "done", "closeOutcome": "done"}, ifMatch(w.version(id)))
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_close", map[string]any{"id": id, "expectedVersion": w.version(id), "reason": "done", "closeOutcome": "done"})
			}},
		{name: "closeIssue with an unknown repo", want: "repo_not_found",
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/close"), map[string]any{"reason": "d", "closeOutcome": "done",
					"commits": []map[string]any{{"hash": "abcdef1", "repoUrl": "https://nowhere.test/x.git"}}}, ifMatch(w.version(id)))
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_close", map[string]any{"id": id, "expectedVersion": w.version(id), "reason": "d", "closeOutcome": "done",
					"commits": []map[string]any{{"hash": "abcdef1", "repoUrl": "https://nowhere.test/x.git"}}})
			}},
		{name: "acceptIssue", prep: assignTo(w.ownerNm), write: true,
			rest: func(id string) restCall { return rg.rest(ko, http.MethodPost, issuePath(id, "/accept"), nil, nil) },
			mcp:  func(id string) toolAnswer { return rg.tool(ko, "skein_accept", map[string]any{"id": id}) }},
		// skein_reparent names a missing new parent issue_not_found.
		{name: "reparentIssue under a missing parent", want: "issue_not_found",
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/reparent"), map[string]any{"newParentId": w.prefix + "-77777"}, ifMatch(w.version(id)))
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_reparent", map[string]any{"id": id, "newParentId": w.prefix + "-77777", "expectedVersion": w.version(id)})
			}},
		{name: "dropDelegated", prep: assignTo(w.otherNm), write: false,
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/drop-delegated"), nil, nil)
			},
			mcp: func(id string) toolAnswer { return rg.tool(ko, "skein_drop_delegated", map[string]any{"id": id}) }},
		{name: "resumeDelegated", prep: assignTo(w.otherNm), write: false,
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/resume-delegated"), nil, nil)
			},
			mcp: func(id string) toolAnswer { return rg.tool(ko, "skein_resume_delegated", map[string]any{"id": id}) }},
		{name: "attachCommits", write: true,
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(id, "/commits"), map[string]any{"commits": []map[string]any{{"hash": "abcdef12", "repoUrl": w.repoURL}}}, nil)
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_commits_attach", map[string]any{"issueId": id, "commits": []map[string]any{{"hash": "abcdef12", "repoUrl": w.repoURL}}})
			}},
		{name: "attachCommits by a non-holder", prep: claimedByOther, want: "locked_by_other",
			rest: func(id string) restCall {
				return rg.rest(km, http.MethodPost, issuePath(id, "/commits"), map[string]any{"commits": []map[string]any{{"hash": "abcdef12", "repoUrl": w.repoURL}}}, nil)
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(km, "skein_commits_attach", map[string]any{"issueId": id, "commits": []map[string]any{{"hash": "abcdef12", "repoUrl": w.repoURL}}})
			}},
		{name: "detachCommit", write: true,
			prep: func(id string) {
				rg.tool(ko, "skein_commits_attach", map[string]any{"issueId": id, "commits": []map[string]any{{"hash": "abcdef34", "repoUrl": w.repoURL}}})
			},
			rest: func(id string) restCall {
				return rg.rest(ko, http.MethodDelete, issuePath(id, "/commits/abcdef34"), nil, nil)
			},
			mcp: func(id string) toolAnswer {
				return rg.tool(ko, "skein_commits_detach", map[string]any{"issueId": id, "hashes": []string{"abcdef34"}})
			}},
	} {
		run(op)
	}

	t.Run("unclaimIssue", func(t *testing.T) {
		r, m := w.issue("unclaim"), w.issue("unclaim")
		wantOK(t, "claim", rg.rest(ko, http.MethodPost, issuePath(r, "/claim"), nil, nil), http.StatusOK)
		rg.tool(ko, "skein_claim", map[string]any{"id": m})
		since := lastEventID(t, pool)
		parity(t, "unclaimIssue",
			rg.rest(ko, http.MethodPost, issuePath(r, "/unclaim"), map[string]any{"nextStatus": "open", "reason": "r"}, nil),
			rg.tool(ko, "skein_unclaim", map[string]any{"id": m, "nextStatus": "open", "reason": "r"}), "")
		sameEvents(t, "unclaimIssue", w.issueEvents(r, since), w.issueEvents(m, since))
	})

	t.Run("reparentIssue", func(t *testing.T) {
		parent := w.issue("parent")
		r, m := w.issue("child"), w.issue("child")
		since := lastEventID(t, pool)
		parity(t, "reparentIssue",
			rg.rest(ko, http.MethodPost, issuePath(r, "/reparent"), map[string]any{"newParentId": parent}, ifMatch(w.version(r))),
			rg.tool(ko, "skein_reparent", map[string]any{"id": m, "newParentId": parent, "expectedVersion": w.version(m)}), "")
		sameEvents(t, "reparentIssue", w.issueEvents(r, since), w.issueEvents(m, since))
	})

	t.Run("createIssue and createEpic", func(t *testing.T) {
		since := lastEventID(t, pool)
		rest := rg.rest(ko, http.MethodPost, "/api/v1/projects/"+w.project.String()+"/issues",
			map[string]any{"title": "created " + uuid.NewString(), "type": "task", "force": true}, nil)
		mcp := rg.tool(ko, "skein_create", map[string]any{"project": w.project.String(),
			"title": "created " + uuid.NewString(), "type": "task", "force": true, "kind": "directed"})
		parity(t, "createIssue", rest, mcp, "")
		wantOK(t, "createIssue", rest, http.StatusCreated)
		restID, _ := rest.json(t)["id"].(string)
		mcpIssue, _ := mcp.json(t)["issue"].(map[string]any)
		mcpID, _ := mcpIssue["id"].(string)
		sameEvents(t, "createIssue", w.treeEvents(restID, since), w.treeEvents(mcpID, since))
		sameNotifications(t, "createIssue", w.treeNotifications(restID, since), w.treeNotifications(mcpID, since))
		if loc := rest.header.Get("Location"); loc != "/api/v1/issues/"+rest.json(t)["id"].(string) {
			t.Errorf("createIssue Location %q", loc)
		}
		parity(t, "createIssue under a missing parent",
			rg.rest(ko, http.MethodPost, "/api/v1/projects/"+w.project.String()+"/issues",
				map[string]any{"title": "orphan", "type": "task", "force": true, "parentId": w.prefix + "-88888"}, nil),
			rg.tool(ko, "skein_create", map[string]any{"project": w.project.String(), "title": "orphan", "type": "task",
				"force": true, "parentId": w.prefix + "-88888", "kind": "directed"}), "parent_not_found")
		since = lastEventID(t, pool)
		epic := rg.rest(ko, http.MethodPost, "/api/v1/projects/"+w.project.String()+"/epics",
			map[string]any{"title": "epic", "childTemplates": []map[string]any{{"title": "c1"}, {"title": "c2"}}}, nil)
		mepic := rg.tool(ko, "skein_epic_create", map[string]any{"project": w.project.String(), "title": "epic",
			"kind": "directed", "childTemplates": []map[string]any{{"title": "c1"}, {"title": "c2"}}})
		parity(t, "createEpic", epic, mepic, "")
		epicID, _ := epic.json(t)["id"].(string)
		mcpEpic, _ := mepic.json(t)["epic"].(map[string]any)
		mcpEpicID, _ := mcpEpic["id"].(string)
		sameEvents(t, "createEpic", w.treeEvents(epicID, since), w.treeEvents(mcpEpicID, since))
		sameNotifications(t, "createEpic", w.treeNotifications(epicID, since), w.treeNotifications(mcpEpicID, since))
		if children, _ := epic.json(t)["children"].([]any); len(children) != 2 {
			t.Errorf("createEpic children %v", epic.json(t)["children"])
		}
	})
}

// TestIssuesWrite_NothingWrittenOnRefusal — row 85 (US-2, SC-006). A refused
// key right, a stale If-Match (412 with the version from the database) and
// a missing one (428) on updateIssue, closeIssue and reparentIssue leave
// the issue, its events and its version as they were; acceptIssue passes
// without If-Match; unclaimIssue does not check a version. Red when
// anything is written on a refusal.
func TestIssuesWrite_NothingWrittenOnRefusal(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelManage)
	kr := w.key(w.owner, apikeys.LevelRead)
	parent := w.issue("parent")
	writes := []struct {
		name   string
		method string
		suffix string
		body   map[string]any
	}{
		{"updateIssue", http.MethodPatch, "", map[string]any{"title": "x"}},
		{"closeIssue", http.MethodPost, "/close", map[string]any{"reason": "r", "closeOutcome": "done"}},
		{"reparentIssue", http.MethodPost, "/reparent", map[string]any{"newParentId": parent}},
	}
	for _, c := range writes {
		t.Run(c.name, func(t *testing.T) {
			id := w.issue(c.name)
			v, ev := w.version(id), w.events(id)
			still := func(what string) {
				t.Helper()
				if w.version(id) != v || w.events(id) != ev {
					t.Errorf("%s: version %d→%d, events %d→%d", what, v, w.version(id), ev, w.events(id))
				}
			}
			if a := rg.rest(kr, c.method, issuePath(id, c.suffix), c.body, ifMatch(v)); a.status != http.StatusForbidden || a.code() != "insufficient_scope" {
				t.Errorf("a read key: %d %s", a.status, a.body)
			}
			still("a read key")
			a := rg.rest(ko, c.method, issuePath(id, c.suffix), c.body, ifMatch(v+5))
			if a.status != http.StatusPreconditionFailed || a.code() != "version_conflict" {
				t.Errorf("a stale If-Match: %d %s", a.status, a.body)
			} else if cur, _ := a.json(t)["currentVersion"].(float64); int(cur) != v {
				t.Errorf("a stale If-Match: currentVersion %v, want %d", cur, v)
			}
			still("a stale If-Match")
			if a := rg.rest(ko, c.method, issuePath(id, c.suffix), c.body, nil); a.status != http.StatusPreconditionRequired || a.code() != "precondition_required" {
				t.Errorf("no If-Match: %d %s", a.status, a.body)
			}
			still("no If-Match")
		})
	}
	t.Run("acceptIssue without If-Match", func(t *testing.T) {
		id := w.issue("accept")
		w.exec(`UPDATE issues SET assignee = $2 WHERE id = $1`, id, w.ownerNm)
		wantOK(t, "acceptIssue", rg.rest(ko, http.MethodPost, issuePath(id, "/accept"), nil, nil), http.StatusOK)
	})
	t.Run("unclaimIssue checks no version", func(t *testing.T) {
		id := w.issue("unclaim")
		wantOK(t, "claim", rg.rest(ko, http.MethodPost, issuePath(id, "/claim"), nil, nil), http.StatusOK)
		wantOK(t, "unclaimIssue with a stale If-Match", rg.rest(ko, http.MethodPost, issuePath(id, "/unclaim"),
			map[string]any{"nextStatus": "open", "reason": "r"}, ifMatch(1)), http.StatusOK)
	})
}

// TestIssuesWrite_DuplicateGate — row 87 (US-1, OST-3-2). createIssue
// without force and duplicateOf, finding similar issues, creates nothing
// and answers 422 duplicate_suspected with the candidates MCP gives as
// duplicates; the same request under the same Idempotency-Key answers the
// stored 422; force=true under that key is another request (422
// idempotency_key_reused, nothing created); force=true under a new key with
// write creates the issue; duplicateOf creates it with the duplicates edge,
// as MCP. Red when an issue is created on a suspected duplicate.
func TestIssuesWrite_DuplicateGate(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	kw := w.key(w.owner, apikeys.LevelWrite)
	title := "Duplicate gate check " + uuid.NewString()[:8]
	original := testsupport.SeedIssue(t, pool, w.project, w.owner, w.prefix+"-500", title, "open", 2)
	path := "/api/v1/projects/" + w.project.String() + "/issues"
	count := func() int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM issues WHERE project_id = $1`, w.project).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	before := count()
	key := "dup-" + uuid.NewString()
	body := map[string]any{"title": title, "type": "task"}
	first := rg.rest(kw, http.MethodPost, path, body, map[string]string{"Idempotency-Key": key})
	if first.status != http.StatusUnprocessableEntity || first.code() != "duplicate_suspected" {
		t.Fatalf("a similar issue: %d %s", first.status, first.body)
	}
	mcp := rg.tool(kw, "skein_create", map[string]any{"project": w.project.String(), "title": title, "type": "task", "kind": "directed"})
	m := mcp.json(t)
	if m["decisionRequired"] != true {
		t.Fatalf("MCP did not find the duplicate: %s", mcp.text)
	}
	if got, want := ids(t, first.json(t)["duplicates"], "id"), ids(t, m["duplicates"], "id"); !slices.Equal(got, want) || !slices.Contains(got, original) {
		t.Errorf("candidates %v, MCP %v (want %s among them)", got, want, original)
	}
	if count() != before {
		t.Fatalf("an issue was created on a suspected duplicate")
	}
	again := rg.rest(kw, http.MethodPost, path, body, map[string]string{"Idempotency-Key": key})
	if again.status != http.StatusUnprocessableEntity || again.code() != "duplicate_suspected" {
		t.Errorf("the repeat: %d %s", again.status, again.body)
	}
	forced := map[string]any{"title": title, "type": "task", "force": true}
	if a := rg.rest(kw, http.MethodPost, path, forced, map[string]string{"Idempotency-Key": key}); a.status != http.StatusUnprocessableEntity || a.code() != "idempotency_key_reused" {
		t.Errorf("force under the same key: %d %s", a.status, a.body)
	}
	if count() != before {
		t.Fatalf("an issue was created under a reused key")
	}
	wantOK(t, "force under a new key", rg.rest(kw, http.MethodPost, path, forced, map[string]string{"Idempotency-Key": "dup-" + uuid.NewString()}), http.StatusCreated)
	dup := rg.rest(kw, http.MethodPost, path, map[string]any{"title": title, "type": "task", "duplicateOf": original}, nil)
	wantOK(t, "duplicateOf", dup, http.StatusCreated)
	var edges int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'duplicates'`,
		dup.json(t)["id"], original).Scan(&edges); err != nil || edges != 1 {
		t.Errorf("duplicateOf edge: %d %v", edges, err)
	}
}

// TestIssuesWrite_ConcurrentClaims — row 88 (US-5, SC-005). Two claims of one
// issue at once by two executors of one key: exactly one succeeds, the
// other is refused already_claimed (the code of skein_claim for a claimed
// issue); the other executor's change and close are refused locked_by_other
// while the first holds; the key itself (no header) is a holder apart.
// Red when the claim check is outside the claiming transaction.
func TestIssuesWrite_ConcurrentClaims(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	k := w.key(w.owner, apikeys.LevelWrite)
	for round := 0; round < 5; round++ {
		id := w.issue("race")
		var wg sync.WaitGroup
		answers := make([]restCall, 2)
		for i, worker := range []string{"w1", "w2"} {
			wg.Add(1)
			go func(i int, worker string) {
				defer wg.Done()
				answers[i] = rg.rest(k, http.MethodPost, issuePath(id, "/claim"), nil, map[string]string{"X-Skein-Worker-Id": worker})
			}(i, worker)
		}
		wg.Wait()
		ok, refused := 0, ""
		loser := ""
		for i, a := range answers {
			if a.status == http.StatusOK {
				ok++
			} else {
				refused = a.code()
				loser = []string{"w1", "w2"}[i]
			}
		}
		if ok != 1 || refused != "already_claimed" {
			t.Fatalf("round %d: %d successes, the other %q (%d %s / %d %s)", round, ok, refused,
				answers[0].status, answers[0].body, answers[1].status, answers[1].body)
		}
		hdr := ifMatch(w.version(id))
		hdr["X-Skein-Worker-Id"] = loser
		if a := rg.rest(k, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "x"}, hdr); a.code() != "locked_by_other" {
			t.Errorf("the other executor's change: %d %s", a.status, a.body)
		}
		if a := rg.rest(k, http.MethodPost, issuePath(id, "/close"), map[string]any{"reason": "r", "closeOutcome": "done"}, hdr); a.code() != "locked_by_other" {
			t.Errorf("the other executor's close: %d %s", a.status, a.body)
		}
		if a := rg.rest(k, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "x"}, ifMatch(w.version(id))); a.code() != "locked_by_other" {
			t.Errorf("the key without a header: %d %s", a.status, a.body)
		}
	}
}

// TestIssuesWrite_ReleaseLock — row 89 (INV-6, INV-7, SC-017). The owner's own
// lock, taken by another of his keys, an executor or an MCP session, is
// released without a role; another holder's needs force_unlock (403
// without); the version grows, so the next write with the old If-Match is
// 412; an issue nobody holds answers 200 with the same version. Red when
// the release bypasses the version.
func TestIssuesWrite_ReleaseLock(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	m1 := w.key(w.member, apikeys.LevelWrite)
	m2 := w.key(w.member, apikeys.LevelWrite)
	other := w.key(w.other, apikeys.LevelWrite)
	addMember(t, pool, w.project, w.other, "member")
	owner := w.key(w.owner, apikeys.LevelWrite)

	t.Run("own lock of another key and executor", func(t *testing.T) {
		id := w.issue("own")
		wantOK(t, "claim", rg.rest(m1, http.MethodPost, issuePath(id, "/claim"), nil, map[string]string{"X-Skein-Worker-Id": "a"}), http.StatusOK)
		v := w.version(id)
		wantOK(t, "release", rg.rest(m2, http.MethodPost, issuePath(id, "/release-lock"), nil, nil), http.StatusOK)
		if w.lockedBy(id) != "" || w.version(id) != v+1 {
			t.Errorf("after the release: locked %q, version %d (was %d)", w.lockedBy(id), w.version(id), v)
		}
		if a := rg.rest(m2, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "x"}, ifMatch(v)); a.status != http.StatusPreconditionFailed {
			t.Errorf("the old If-Match after the release: %d %s", a.status, a.body)
		}
	})
	t.Run("own lock of an MCP session", func(t *testing.T) {
		id := w.issue("mcp")
		if a := rg.tool(m1, "skein_claim", map[string]any{"id": id}); a.code != "" {
			t.Fatalf("MCP claim: %s", a.text)
		}
		wantOK(t, "release", rg.rest(m2, http.MethodPost, issuePath(id, "/release-lock"), nil, nil), http.StatusOK)
	})
	t.Run("another holder's lock", func(t *testing.T) {
		id := w.issue("other")
		wantOK(t, "claim", rg.rest(m1, http.MethodPost, issuePath(id, "/claim"), nil, nil), http.StatusOK)
		v := w.version(id)
		if a := rg.rest(other, http.MethodPost, issuePath(id, "/release-lock"), nil, nil); a.status != http.StatusForbidden || a.code() != "force_unlock_forbidden" {
			t.Errorf("without force_unlock: %d %s", a.status, a.body)
		}
		if w.version(id) != v || w.lockedBy(id) == "" {
			t.Errorf("a refused release changed the issue")
		}
		wantOK(t, "the project owner", rg.rest(owner, http.MethodPost, issuePath(id, "/release-lock"), nil, nil), http.StatusOK)
		if w.version(id) != v+1 {
			t.Errorf("version %d after the release, want %d", w.version(id), v+1)
		}
	})
	t.Run("nobody holds it", func(t *testing.T) {
		id := w.issue("free")
		v := w.version(id)
		wantOK(t, "release", rg.rest(m1, http.MethodPost, issuePath(id, "/release-lock"), nil, nil), http.StatusOK)
		if w.version(id) != v {
			t.Errorf("a release of a free issue moved the version %d→%d", v, w.version(id))
		}
	})
}

// TestIssuesWrite_ForceNeedsManage — row 90 (SC-017, INV-7). force=true with a
// write key on updateIssue, closeIssue, unclaimIssue, attachCommits and
// detachCommit is 403 insufficient_scope; with manage and the owner's
// force_unlock it bypasses another holder's lock; force in the body of
// createIssue with write passes. Red when force passes with write.
func TestIssuesWrite_ForceNeedsManage(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	kw := w.key(w.owner, apikeys.LevelWrite)
	km := w.key(w.owner, apikeys.LevelManage)
	ops := []struct {
		name   string
		method string
		suffix string
		body   map[string]any
		match  bool
	}{
		{"updateIssue", http.MethodPatch, "", map[string]any{"title": "forced"}, true},
		{"closeIssue", http.MethodPost, "/close", map[string]any{"reason": "r", "closeOutcome": "done"}, true},
		{"unclaimIssue", http.MethodPost, "/unclaim", map[string]any{"nextStatus": "open", "reason": "r"}, false},
		{"attachCommits", http.MethodPost, "/commits", map[string]any{"commits": []map[string]any{{"hash": "abcdef56", "repoUrl": w.repoURL}}}, false},
		{"detachCommit", http.MethodDelete, "/commits/abcdef56", nil, false},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			id := w.issue(op.name)
			w.exec(`UPDATE issues SET locked_by = 'mcp-session-other', locked_at = now(), status = 'in_progress' WHERE id = $1`, id)
			var hdr map[string]string
			if op.match {
				hdr = ifMatch(w.version(id))
			}
			if a := rg.rest(kw, op.method, issuePath(id, op.suffix)+"?force=true", op.body, hdr); a.status != http.StatusForbidden || a.code() != "insufficient_scope" {
				t.Errorf("force with write: %d %s", a.status, a.body)
			}
			// Every DELETE of the API answers 204 (decision R8-06).
			want := http.StatusOK
			if op.method == http.MethodDelete {
				want = http.StatusNoContent
			}
			if a := rg.rest(km, op.method, issuePath(id, op.suffix)+"?force=true", op.body, hdr); a.status != want {
				t.Errorf("force with manage and force_unlock: %d %s, want %d", a.status, a.body, want)
			}
		})
	}
	wantOK(t, "createIssue with force and write", rg.rest(kw, http.MethodPost, "/api/v1/projects/"+w.project.String()+"/issues",
		map[string]any{"title": "forced create " + uuid.NewString(), "type": "task", "force": true}, nil), http.StatusCreated)
}

// TestIssuesWrite_ClaimIssues — row 91. claimIssues takes 1..100 issues: 0 and
// 101 are 400; one refused issue leaves none claimed; the answer keeps the
// order of the request. Red when a batch is claimed in part.
func TestIssuesWrite_ClaimIssues(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	k := w.key(w.owner, apikeys.LevelWrite)
	if a := rg.rest(k, http.MethodPost, "/api/v1/issues/claim", map[string]any{"ids": []string{}}, nil); a.status != http.StatusBadRequest {
		t.Errorf("0 ids: %d %s", a.status, a.body)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("%s-%d", w.prefix, 10000+i)
	}
	if a := rg.rest(k, http.MethodPost, "/api/v1/issues/claim", map[string]any{"ids": many}, nil); a.status != http.StatusBadRequest {
		t.Errorf("101 ids: %d %s", a.status, a.body)
	}
	a, b, c := w.issue("a"), w.issue("b"), w.issue("c")
	w.exec(`UPDATE issues SET locked_by = 'mcp-session-other', locked_at = now(), status = 'in_progress' WHERE id = $1`, c)
	if r := rg.rest(k, http.MethodPost, "/api/v1/issues/claim", map[string]any{"ids": []string{a, b, c}}, nil); r.code() != "already_claimed" {
		t.Errorf("a refused issue in the batch: %d %s", r.status, r.body)
	}
	if w.lockedBy(a) != "" || w.lockedBy(b) != "" {
		t.Errorf("a refused batch left locks: %q %q", w.lockedBy(a), w.lockedBy(b))
	}
	d := w.issue("d")
	r := rg.rest(k, http.MethodPost, "/api/v1/issues/claim", map[string]any{"ids": []string{d, b, a}}, nil)
	wantOK(t, "claimIssues", r, http.StatusOK)
	// ids sorts; the order of the answer is what is checked here.
	got := []string{}
	list, _ := r.json(t)["items"].([]any)
	for _, it := range list {
		m, _ := it.(map[string]any)
		s, _ := m["id"].(string)
		got = append(got, s)
	}
	if !slices.Equal(got, []string{d, b, a}) {
		t.Errorf("order %v, want %v", got, []string{d, b, a})
	}
}

// TestIssuesWrite_OriginOfAMissingObject — row 92 (OST-2-5). The object of the
// path is 404, a reference of the body 422 with the code MCP answers:
// createIssue (the parent), createEpic (the parent), reparentIssue (the new
// parent), closeIssue (the target), claimIssues (an id); an object outside
// the key's scope answers as a missing one. Red when the status is chosen
// without the origin.
func TestIssuesWrite_OriginOfAMissingObject(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	k := w.key(w.owner, apikeys.LevelWrite)
	q, qPrefix := seedProject(t, pool, w.owner, w.space, "wrq")
	qIssue := testsupport.SeedIssue(t, pool, q, w.owner, qPrefix+"-1", "outside", "open", 2)
	scoped := projectsKey(t, pool, w.owner, apikeys.LevelWrite, w.project)
	id := w.issue("subject")
	missing := w.prefix + "-99999"
	for _, key := range []struct {
		name, key, ref string
	}{{"missing", k, missing}, {"outside the scope", scoped, qIssue}} {
		t.Run(key.name, func(t *testing.T) {
			check := func(what string, a restCall, status int, code string) {
				t.Helper()
				if a.status != status || a.code() != code {
					t.Errorf("%s: %d %s, want %d %s", what, a.status, a.body, status, code)
				}
			}
			check("createIssue parent", rg.rest(key.key, http.MethodPost, "/api/v1/projects/"+w.project.String()+"/issues",
				map[string]any{"title": "t", "type": "task", "force": true, "parentId": key.ref}, nil), 422, "parent_not_found")
			check("createEpic parent", rg.rest(key.key, http.MethodPost, "/api/v1/projects/"+w.project.String()+"/epics",
				map[string]any{"title": "e", "parentId": key.ref}, nil), 422, "parent_not_found")
			check("reparentIssue new parent", rg.rest(key.key, http.MethodPost, issuePath(id, "/reparent"),
				map[string]any{"newParentId": key.ref}, ifMatch(w.version(id))), 422, "issue_not_found")
			check("closeIssue target", rg.rest(key.key, http.MethodPost, issuePath(id, "/close"),
				map[string]any{"reason": "r", "closeOutcome": "duplicate", "closeTarget": key.ref}, ifMatch(w.version(id))), 422, "issue_not_found")
			check("claimIssues id", rg.rest(key.key, http.MethodPost, "/api/v1/issues/claim",
				map[string]any{"ids": []string{key.ref}}, nil), 422, "issue_not_found")
			check("the issue of the path", rg.rest(key.key, http.MethodPost, issuePath(key.ref, "/claim"), nil, nil), 404, "issue_not_found")
		})
	}
	if a := rg.rest(k, http.MethodPost, "/api/v1/projects/"+uuid.NewString()+"/issues", map[string]any{"title": "t", "type": "task"}, nil); a.status != 404 || a.code() != "project_not_found" {
		t.Errorf("createIssue in a missing project: %d %s", a.status, a.body)
	}
}

// TestIssuesWrite_LocksOfAnOldKey — row 93 (US-10). A lock taken with an old
// key stays its: a change with the owner's new key is locked_by_other; the
// owner releases it with releaseIssueLock without a role. Red when the
// lock passes to the new key.
func TestIssuesWrite_LocksOfAnOldKey(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	old := w.key(w.member, apikeys.LevelWrite)
	reissued := w.key(w.member, apikeys.LevelWrite)
	id := w.issue("old key")
	wantOK(t, "claim", rg.rest(old, http.MethodPost, issuePath(id, "/claim"), nil, nil), http.StatusOK)
	if a := rg.rest(reissued, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "x"}, ifMatch(w.version(id))); a.code() != "locked_by_other" {
		t.Errorf("the new key's change: %d %s", a.status, a.body)
	}
	wantOK(t, "release by the new key", rg.rest(reissued, http.MethodPost, issuePath(id, "/release-lock"), nil, nil), http.StatusOK)
}

// TestIssuesWrite_OutsideTheScopeIsMissing — row 130 (US-2). With a key of
// scope P, updateIssue, closeIssue and releaseIssueLock of an issue of Q,
// a project of the owner, answer exactly as for a missing id (404
// issue_not_found), never 403. Red when 403 reveals the object.
func TestIssuesWrite_OutsideTheScopeIsMissing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	q, qPrefix := seedProject(t, pool, w.member, w.space, "wro")
	addMember(t, pool, q, w.owner, "member")
	qIssue := testsupport.SeedIssue(t, pool, q, w.member, qPrefix+"-1", "outside", "open", 2)
	scoped := projectsKey(t, pool, w.owner, apikeys.LevelManage, w.project)
	missing := w.prefix + "-99999"
	for _, c := range []struct {
		name, method, suffix string
		body                 map[string]any
	}{
		{"updateIssue", http.MethodPatch, "", map[string]any{"title": "x"}},
		{"closeIssue", http.MethodPost, "/close", map[string]any{"reason": "r", "closeOutcome": "done"}},
		{"releaseIssueLock", http.MethodPost, "/release-lock", nil},
	} {
		in := rg.rest(scoped, c.method, issuePath(qIssue, c.suffix), c.body, ifMatch(1))
		none := rg.rest(scoped, c.method, issuePath(missing, c.suffix), c.body, ifMatch(1))
		if in.status != http.StatusNotFound || in.code() != "issue_not_found" || in.status != none.status || in.code() != none.code() {
			t.Errorf("%s outside the scope %d %s; a missing id %d %s", c.name, in.status, in.body, none.status, none.body)
		}
	}
}

// TestIssuesWrite_LabelsMoveNoVersion — row 133 (US-6). updateIssue with labels
// only keeps the ETag and the version, and the next request with the old
// If-Match passes. Red when the version grows.
func TestIssuesWrite_LabelsMoveNoVersion(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	k := w.key(w.owner, apikeys.LevelWrite)
	id := w.issue("labels")
	v := w.version(id)
	a := rg.rest(k, http.MethodPatch, issuePath(id, ""), map[string]any{"labelsAdd": []string{"l-" + uuid.NewString()[:6]}}, ifMatch(v))
	wantOK(t, "labels", a, http.StatusOK)
	if a.header.Get("ETag") != `"`+strconv.Itoa(v)+`"` || w.version(id) != v {
		t.Errorf("labels moved the version: ETag %s, version %d (was %d)", a.header.Get("ETag"), w.version(id), v)
	}
	wantOK(t, "the old If-Match after labels", rg.rest(k, http.MethodPatch, issuePath(id, ""), map[string]any{"title": "next"}, ifMatch(v)), http.StatusOK)
}

// TestIssuesWrite_ExecutorSeenByReaders — row 134 (US-5). After claimIssue by
// executor W the holder of the lock, read by another reader of the issue
// through getIssue and through skein_show, is W's holder. Red when the
// holder is not shown.
func TestIssuesWrite_ExecutorSeenByReaders(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	k := w.key(w.owner, apikeys.LevelWrite)
	reader := w.key(w.member, apikeys.LevelRead)
	id := w.issue("executor")
	wantOK(t, "claim", rg.rest(k, http.MethodPost, issuePath(id, "/claim"), nil, map[string]string{"X-Skein-Worker-Id": "builder-7"}), http.StatusOK)
	holder := w.lockedBy(id)
	if !strings.HasPrefix(holder, "api:") || !strings.HasSuffix(holder, ":builder-7") {
		t.Fatalf("holder %q is not the executor's", holder)
	}
	if got := rg.rest(reader, http.MethodGet, issuePath(id, ""), nil, nil).json(t)["lockedBy"]; got != holder {
		t.Errorf("getIssue lockedBy %v, want %s", got, holder)
	}
	card, _ := rg.tool(reader, "skein_show", map[string]any{"id": id, "includeComments": false}).json(t)["issue"].(map[string]any)
	if card["lockedBy"] != holder {
		t.Errorf("skein_show lockedBy %v, want %s", card["lockedBy"], holder)
	}
}

// TestIssuesWrite_NullBody — regression (R7-04): an optional body of JSON
// null reads as an empty one. It used to leave the arguments nil and the
// handler panicked writing the issue id into them (500). Red when a write
// with the body null answers other than with the body {}.
func TestIssuesWrite_NullBody(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	k := w.key(w.owner, apikeys.LevelWrite)
	for _, op := range []string{"/release-lock", "/claim"} {
		t.Run(op, func(t *testing.T) {
			empty := rg.rest(k, http.MethodPost, issuePath(w.issue("empty body"), op), "{}", nil)
			null := rg.rest(k, http.MethodPost, issuePath(w.issue("null body"), op), "null", nil)
			if empty.status != http.StatusOK {
				t.Fatalf("the body {}: %d %s", empty.status, empty.body)
			}
			if null.status != empty.status {
				t.Errorf("the body null: %d %s, the body {} %d", null.status, null.body, empty.status)
			}
		})
	}
}

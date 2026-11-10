package publicapi_test

// comments_test.go — the comment operations of the public API (sk-878.3.1
// T026): parity with the MCP tools, the tombstone, authorship and the
// version of an edit, the role of a delete (row 95; US-1, US-6, SC-028), and
// a comment outside the key's scope (row 131; US-2, INV-2). The reference of
// the parity is the MCP channel itself over twin issues; the reference of
// the rest is the spec: «updateComment — write, only the author, If-Match
// of the comment's version; a stale one — 412, nothing written; none — 428;
// deleteComment of another's — write + delete_any_comment»; «a check of the
// owner's role does not reveal an object outside the scope».

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// commentEvents are «type/actor type» of the events of the comments of
// issue written after since.
func (w *writeWorld) commentEvents(issue string, since int64) []string {
	w.t.Helper()
	return w.strings(`SELECT type || '/' || actor_type FROM events
 WHERE entity_type = 'comment' AND entity_id IN (SELECT id::text FROM comments WHERE issue_id = $1) AND id > $2
 ORDER BY id`, issue, since)
}

// commentNotifications are «event type/recipient» of the notifications of
// the comment events of issue after since, sorted.
func (w *writeWorld) commentNotifications(issue string, since int64) []string {
	w.t.Helper()
	return w.strings(`SELECT e.type || '/' || n.user_id::text FROM notifications n JOIN events e ON e.id = n.event_id
 WHERE e.entity_type = 'comment' AND e.entity_id IN (SELECT id::text FROM comments WHERE issue_id = $1) AND e.id > $2
 ORDER BY 1`, issue, since)
}

// comment seeds a comment of author on issue and returns its id.
func (w *writeWorld) comment(issue string, author uuid.UUID, body string) string {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'seed', $3, 'human', 'human', $4, now(), now())`, id, issue, author, body)
	return id.String()
}

// commentRow is the body and the version of a comment as stored.
func (w *writeWorld) commentRow(id string) (body string, version int) {
	w.t.Helper()
	if err := w.pool.QueryRow(context.Background(), `SELECT body, version FROM comments WHERE id = $1`, id).Scan(&body, &version); err != nil {
		w.t.Fatalf("read comment %s: %v", id, err)
	}
	return body, version
}

func commentPath(id string) string { return "/api/v1/comments/" + id }

// TestComments_ParityWithMCP — row 95 (US-1, INV-4). Each comment operation
// asked with the same key on REST and on MCP over twin issues gives the same
// code for the success and for its refusals, and the same events and
// notifications. Red when a code, an event or a notification differs from
// MCP.
func TestComments_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelWrite)
	km := w.key(w.member, apikeys.LevelWrite)
	// The member watches the project: a comment of the owner notifies him.
	w.exec(`INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, w.project, w.member)

	type twin struct {
		name string
		// prep seeds the state of one issue and returns the comment the
		// operation acts on ("" for createComment).
		prep  func(issue string) string
		rest  func(issue, comment string) restCall
		mcp   func(issue, comment string) toolAnswer
		want  string
		write bool
	}
	ownComment := func(issue string) string { return w.comment(issue, w.owner, "the owner's note") }
	membersComment := func(issue string) string { return w.comment(issue, w.member, "the member's note") }
	for _, op := range []twin{
		{name: "createComment", write: true,
			rest: func(issue, _ string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(issue, "/comments"), map[string]any{"body": "hello", "kind": "directed"}, nil)
			},
			mcp: func(issue, _ string) toolAnswer {
				return rg.tool(ko, "skein_comment", map[string]any{"issueId": issue, "body": "hello", "kind": "directed"})
			}},
		{name: "createComment with an empty body", want: "validation_error",
			rest: func(issue, _ string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(issue, "/comments"), map[string]any{"body": "   "}, nil)
			},
			mcp: func(issue, _ string) toolAnswer {
				return rg.tool(ko, "skein_comment", map[string]any{"issueId": issue, "body": "   ", "kind": "directed"})
			}},
		{name: "updateComment by the author", prep: ownComment, write: true,
			rest: func(_, c string) restCall {
				_, v := w.commentRow(c)
				return rg.rest(ko, http.MethodPatch, commentPath(c), map[string]any{"body": "edited"}, ifMatch(v))
			},
			mcp: func(_, c string) toolAnswer {
				_, v := w.commentRow(c)
				return rg.tool(ko, "skein_comment_update", map[string]any{"commentId": c, "body": "edited", "expectedVersion": v, "kind": "directed"})
			}},
		{name: "updateComment by another user", prep: ownComment, want: "forbidden",
			rest: func(_, c string) restCall {
				_, v := w.commentRow(c)
				return rg.rest(km, http.MethodPatch, commentPath(c), map[string]any{"body": "not mine"}, ifMatch(v))
			},
			mcp: func(_, c string) toolAnswer {
				_, v := w.commentRow(c)
				return rg.tool(km, "skein_comment_update", map[string]any{"commentId": c, "body": "not mine", "expectedVersion": v, "kind": "directed"})
			}},
		{name: "deleteComment of another's without delete_any_comment", prep: ownComment, want: "forbidden",
			rest: func(_, c string) restCall { return rg.rest(km, http.MethodDelete, commentPath(c), nil, nil) },
			mcp: func(_, c string) toolAnswer {
				return rg.tool(km, "skein_comment_delete", map[string]any{"commentId": c})
			}},
		{name: "deleteComment of another's by the project owner", prep: membersComment, write: true,
			rest: func(_, c string) restCall { return rg.rest(ko, http.MethodDelete, commentPath(c), nil, nil) },
			mcp: func(_, c string) toolAnswer {
				return rg.tool(ko, "skein_comment_delete", map[string]any{"commentId": c})
			}},
		{name: "deleteComment of one's own", prep: membersComment, write: true,
			rest: func(_, c string) restCall { return rg.rest(km, http.MethodDelete, commentPath(c), nil, nil) },
			mcp: func(_, c string) toolAnswer {
				return rg.tool(km, "skein_comment_delete", map[string]any{"commentId": c})
			}},
	} {
		t.Run(op.name, func(t *testing.T) {
			r, m := w.issue(op.name), w.issue(op.name)
			var rc, mc string
			if op.prep != nil {
				rc, mc = op.prep(r), op.prep(m)
			}
			since := lastEventID(t, pool)
			rest, mcp := op.rest(r, rc), op.mcp(m, mc)
			parity(t, op.name, rest, mcp, op.want)
			if op.want == "" && !(rest.status >= 200 && rest.status < 300) {
				t.Fatalf("REST answered %d %s", rest.status, rest.body)
			}
			if op.write {
				sameEvents(t, op.name, w.commentEvents(r, since), w.commentEvents(m, since))
				sameNotifications(t, op.name, w.commentNotifications(r, since), w.commentNotifications(m, since))
			}
		})
	}

	t.Run("getComment and its tombstone", func(t *testing.T) {
		c := w.comment(w.issue("get"), w.owner, "to be read")
		rest := rg.rest(ko, http.MethodGet, commentPath(c), nil, nil)
		wantOK(t, "getComment", rest, http.StatusOK)
		mcp := rg.tool(ko, "skein_comment_get", map[string]any{"commentId": c})
		if mcp.code != "" {
			t.Fatalf("skein_comment_get: %s", mcp.text)
		}
		got, ref := rest.json(t), mcp.json(t)["comment"].(map[string]any)
		for _, f := range []string{"id", "body", "authorKind", "version", "issueId"} {
			if got[f] != ref[f] {
				t.Errorf("getComment %s = %v, MCP %v", f, got[f], ref[f])
			}
		}
		if _, v := w.commentRow(c); rest.header.Get("ETag") != `"`+strconv.Itoa(v)+`"` {
			t.Errorf("ETag %q, want the version %d", rest.header.Get("ETag"), v)
		}

		wantOK(t, "deleteComment", rg.rest(ko, http.MethodDelete, commentPath(c), nil, nil), http.StatusOK)
		tomb := rg.rest(ko, http.MethodGet, commentPath(c), nil, nil)
		wantOK(t, "getComment of a deleted comment", tomb, http.StatusOK)
		body := tomb.json(t)
		if body["deletedAt"] == nil || body["body"] != "" {
			t.Errorf("tombstone: deletedAt %v body %q, want a time and an empty body", body["deletedAt"], body["body"])
		}
		ref = rg.tool(ko, "skein_comment_get", map[string]any{"commentId": c}).json(t)["comment"].(map[string]any)
		if (ref["deletedAt"] == nil) != (body["deletedAt"] == nil) || ref["body"] != body["body"] {
			t.Errorf("tombstone differs from MCP: %v / %v", body, ref)
		}
		if again := rg.rest(ko, http.MethodDelete, commentPath(c), nil, nil); again.status != http.StatusOK {
			t.Errorf("deleting a deleted comment: %d %s, want 200", again.status, again.body)
		}
	})

	t.Run("createComment without kind is written by api", func(t *testing.T) {
		c := rg.rest(ko, http.MethodPost, issuePath(w.issue("api"), "/comments"), map[string]any{"body": "plain"}, nil)
		wantOK(t, "createComment", c, http.StatusCreated)
		if kind := c.json(t)["authorKind"]; kind != "api" {
			t.Errorf("authorKind %v, want api", kind)
		}
	})
}

// TestComments_IfMatchOfAnEdit — row 95 (US-6, SC-028). updateComment with a
// stale If-Match answers 412 version_conflict with the current version and
// writes nothing — the body, the version and the events stay; without
// If-Match 428 and nothing written. Red when a stale If-Match writes.
func TestComments_IfMatchOfAnEdit(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelWrite)
	issue := w.issue("edit")
	c := w.comment(issue, w.owner, "original")
	_, v := w.commentRow(c)

	since := lastEventID(t, pool)
	stale := rg.rest(ko, http.MethodPatch, commentPath(c), map[string]any{"body": "stale"}, ifMatch(v+1))
	if stale.status != http.StatusPreconditionFailed || stale.code() != "version_conflict" {
		t.Fatalf("a stale If-Match: %d %s, want 412 version_conflict", stale.status, stale.body)
	}
	if cur, _ := stale.json(t)["currentVersion"].(float64); int(cur) != v {
		t.Errorf("currentVersion %v, want %d", stale.json(t)["currentVersion"], v)
	}
	none := rg.rest(ko, http.MethodPatch, commentPath(c), map[string]any{"body": "no precondition"}, nil)
	if none.status != http.StatusPreconditionRequired || none.code() != "precondition_required" {
		t.Errorf("no If-Match: %d %s, want 428 precondition_required", none.status, none.body)
	}
	if body, got := w.commentRow(c); body != "original" || got != v {
		t.Errorf("after the refusals the comment is %q v%d, want %q v%d", body, got, "original", v)
	}
	if ev := w.commentEvents(issue, since); len(ev) != 0 {
		t.Errorf("the refusals wrote events %v", ev)
	}

	ok := rg.rest(ko, http.MethodPatch, commentPath(c), map[string]any{"body": "current"}, ifMatch(v))
	wantOK(t, "updateComment with the current version", ok, http.StatusOK)
	if body, got := w.commentRow(c); body != "current" || got != v+1 || ok.header.Get("ETag") != `"`+strconv.Itoa(v+1)+`"` {
		t.Errorf("after the edit %q v%d ETag %s, want %q v%d", body, got, ok.header.Get("ETag"), "current", v+1)
	}
}

// TestComments_OutsideTheScopeIsMissing — row 131 (US-2, INV-2). With a key
// of scope P, deleteComment of another user's comment and updateComment of
// a comment in Q — where the owner is neither the author nor holds
// delete_any_comment — answer exactly as a comment that does not exist, and
// nothing is written. Red when the check of the owner's role (403) reveals
// the comment.
func TestComments_OutsideTheScopeIsMissing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	// Q lies in a space of the member, where the owner of the key is a
	// participant only: as the owner of his own space he would hold
	// delete_any_comment there.
	memberSpace, _ := seedOwnSpace(t, pool, w.member)
	testsupport.AddSpaceMember(t, pool, memberSpace, w.owner)
	q, qPrefix := seedProject(t, pool, w.member, memberSpace, "cmo")
	addMember(t, pool, q, w.owner, "member")
	qIssue := testsupport.SeedIssue(t, pool, q, w.member, qPrefix+"-1", "outside", "open", 2)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE issue_id = $1`, qIssue)
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, q)
	})
	theirs := w.comment(qIssue, w.member, "the member's, in Q")
	missing := uuid.NewString()
	scoped := projectsKey(t, pool, w.owner, apikeys.LevelWrite, w.project)
	all := w.key(w.owner, apikeys.LevelWrite)

	for _, c := range []struct {
		name   string
		method string
		body   map[string]any
	}{
		{"deleteComment", http.MethodDelete, nil},
		{"updateComment", http.MethodPatch, map[string]any{"body": "x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The base line: through a key of scope all the owner reaches
			// the comment and is refused by his role.
			if base := rg.rest(all, c.method, commentPath(theirs), c.body, ifMatch(1)); base.status != http.StatusForbidden {
				t.Fatalf("the base line: %d %s, want 403", base.status, base.body)
			}
			since := lastEventID(t, pool)
			in := rg.rest(scoped, c.method, commentPath(theirs), c.body, ifMatch(1))
			none := rg.rest(scoped, c.method, commentPath(missing), c.body, ifMatch(1))
			if in.status != http.StatusNotFound || in.status != none.status || in.code() != none.code() {
				t.Errorf("outside the scope %d %s; a missing id %d %s — want one 404", in.status, in.body, none.status, none.body)
			}
			if body, v := w.commentRow(theirs); body != "the member's, in Q" || v != 1 {
				t.Errorf("the comment changed: %q v%d", body, v)
			}
			if ev := eventsSince(t, pool, q, since); len(ev) != 0 {
				t.Errorf("events written in Q: %v", ev)
			}
		})
	}
}

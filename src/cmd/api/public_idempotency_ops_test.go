package main

// public_idempotency_ops_test.go — row 79 (sk-878.3.1; US-7, SC-007, INV-8,
// OST-3-2, OST-3-6): Idempotency-Key on the real operations that accept it
// through the product server (publicAPIServer over config.Load) — createIssue,
// createEpic, closeIssue, the close with commits included. Each kind of
// outcome is kept by the rules of US-7: a 2xx with a write (recorded in the
// operation's transaction), a 422 before the store, a 422
// duplicate_suspected, a 422 after a rollback — for createIssue, createEpic
// and closeIssue each; a repeat of a 2xx creates no object, event or
// notification. closeIssue has no 2xx without a write: a repeated close of
// a closed issue with the same commit writes (version, event) and attaches
// the commit once. The mode regression on the real closeIssue:
// a stored force close is not given out once the owner lost force_unlock,
// nor once the key was narrowed to write. createComment (row 137, T026):
// a 2xx with a write recorded in the comment's transaction and a 422 before
// the store; it has no 422 after a rollback a request can reach — its one
// transactional refusal, the issue gone between the check and the insert,
// is a race — and no duplicate gate.

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// idemCount reads one number of the database.
func (w *classWorld) idemCount(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		w.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// stored reports the status kept for idempotency key k, 0 when none.
func (w *classWorld) stored(k string) int {
	w.t.Helper()
	var status *int
	err := w.pool.QueryRow(context.Background(),
		`SELECT response_status FROM api_idempotency WHERE idempotency_key = $1 AND state = 'completed'`, k).Scan(&status)
	if err != nil || status == nil {
		return 0
	}
	return *status
}

// sameAnswer asserts that the repeat answered what the first request did.
func sameAnswer(t *testing.T, what string, first, again answer) {
	t.Helper()
	if first.status != again.status || string(first.body) != string(again.body) {
		t.Errorf("%s: the repeat answered %d %s, the first %d %s", what, again.status, again.body, first.status, first.body)
	}
}

// TestIdempotencyOps_RealOperations — row 79. Red when a repeat creates
// objects or events, or gives a stored answer out after the role was
// lowered.
func TestIdempotencyOps_RealOperations(t *testing.T) {
	w := newClassWorld(t)
	key := w.key(w.owner, levelKey(apikeys.LevelWrite))
	proj := "/api/v1/projects/" + w.p.String()
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": "k-" + uuid.NewString()} }
	issuesOf := func() int { return w.idemCount(`SELECT count(*) FROM issues WHERE project_id = $1`, w.p) }
	events := func() int { return w.idemCount(`SELECT count(*) FROM events WHERE project_id = $1`, w.p) }
	notifications := func() int {
		return w.idemCount(`SELECT count(*) FROM notifications n JOIN events e ON e.id = n.event_id WHERE e.project_id = $1`, w.p)
	}

	t.Run("2xx with a write", func(t *testing.T) {
		for _, c := range []opCall{
			{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("idem"), "type": "task", "force": true}, nil},
			{http.MethodPost, proj + "/epics", map[string]any{"title": w.fresh("idem"), "childTemplates": []map[string]any{{"title": "c"}}}, nil},
		} {
			h := idem()
			first := w.do(key, c, "", h)
			if first.status != http.StatusCreated {
				t.Fatalf("%s: %d %s", c.path, first.status, first.body)
			}
			n, e, nt := issuesOf(), events(), notifications()
			again := w.do(key, c, "", h)
			sameAnswer(t, c.path, first, again)
			if issuesOf() != n || events() != e || notifications() != nt {
				t.Errorf("%s: the repeat wrote: issues %d→%d, events %d→%d, notifications %d→%d",
					c.path, n, issuesOf(), e, events(), nt, notifications())
			}
			if w.stored(h["Idempotency-Key"]) != http.StatusCreated {
				t.Errorf("%s: the outcome is not kept", c.path)
			}
		}
	})

	t.Run("a close with commits", func(t *testing.T) {
		id := w.freshIssue(w.p)
		c := opCall{http.MethodPost, "/api/v1/issues/" + id + "/close", map[string]any{"reason": "done", "closeOutcome": "done",
			"commits": []map[string]any{{"hash": w.freshHash(), "repoUrl": w.repoURL}}},
			map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}}
		h := idem()
		first := w.do(key, c, "", h)
		if first.status != http.StatusOK {
			t.Fatalf("close: %d %s", first.status, first.body)
		}
		if w.stored(h["Idempotency-Key"]) != http.StatusOK {
			t.Errorf("the close with commits is not kept")
		}
		e, nt, v := events(), notifications(), w.issueVersion(id)
		again := w.do(key, c, "", h)
		sameAnswer(t, "close", first, again)
		if events() != e || notifications() != nt || w.issueVersion(id) != v {
			t.Errorf("the repeat of the close wrote: events %d→%d, notifications %d→%d, version %d→%d",
				e, events(), nt, notifications(), v, w.issueVersion(id))
		}
	})

	// A repeated close of a closed issue with the same commit is a write of
	// its own (the version grows, issue.status_changed is written), not a 2xx
	// without one: the store has no no-op close. Its commit is not attached
	// twice, and a repeat under the same key writes nothing.
	t.Run("a repeated close with the same commit", func(t *testing.T) {
		id := w.freshIssue(w.p)
		commit := map[string]any{"hash": w.freshHash(), "repoUrl": w.repoURL}
		closeCall := func() opCall {
			return opCall{http.MethodPost, "/api/v1/issues/" + id + "/close",
				map[string]any{"reason": "done", "closeOutcome": "done", "commits": []map[string]any{commit}},
				map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}}
		}
		if a := w.do(key, closeCall(), "", idem()); a.status != http.StatusOK {
			t.Fatalf("the first close: %d %s", a.status, a.body)
		}
		commits := func() int {
			return w.idemCount(`SELECT count(*) FROM issue_commits WHERE issue_id = $1`, id)
		}
		c, h := closeCall(), idem()
		first := w.do(key, c, "", h)
		if first.status != http.StatusOK {
			t.Fatalf("the repeated close: %d %s", first.status, first.body)
		}
		if commits() != 1 {
			t.Errorf("the commit is attached %d times, want once", commits())
		}
		if w.stored(h["Idempotency-Key"]) != http.StatusOK {
			t.Errorf("the repeated close is not kept")
		}
		e, nt, v := events(), notifications(), w.issueVersion(id)
		sameAnswer(t, "a repeated close", first, w.do(key, c, "", h))
		if events() != e || notifications() != nt || w.issueVersion(id) != v || commits() != 1 {
			t.Errorf("the repeat under the key wrote: events %d→%d, notifications %d→%d, version %d→%d, commits %d",
				e, events(), nt, notifications(), v, w.issueVersion(id), commits())
		}
	})

	// kept422 runs c under a fresh key and asserts the 422 code is answered,
	// kept, answered again by the repeat, and nothing was written.
	kept422 := func(t *testing.T, what string, c opCall, code string) {
		t.Helper()
		h := idem()
		n, e, nt := issuesOf(), events(), notifications()
		first := w.do(key, c, "", h)
		if first.status != http.StatusUnprocessableEntity || first.code() != code {
			t.Fatalf("%s: %d %s, want 422 %s", what, first.status, first.body, code)
		}
		if w.stored(h["Idempotency-Key"]) != http.StatusUnprocessableEntity {
			t.Errorf("%s: the 422 is not kept", what)
		}
		sameAnswer(t, what, first, w.do(key, c, "", h))
		if issuesOf() != n || events() != e || notifications() != nt {
			t.Errorf("%s: written: issues %d→%d, events %d→%d, notifications %d→%d",
				what, n, issuesOf(), e, events(), nt, notifications())
		}
	}
	closeOf := func(id string, body map[string]any) opCall {
		return opCall{http.MethodPost, "/api/v1/issues/" + id + "/close", body,
			map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}}
	}

	t.Run("422 before the store", func(t *testing.T) {
		kept422(t, "createIssue of an invalid type",
			opCall{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("idem"), "type": "nonsense"}, nil}, "validation_error")
		kept422(t, "createEpic with a child without a title",
			opCall{http.MethodPost, proj + "/epics", map[string]any{"title": w.fresh("idem"), "childTemplates": []map[string]any{{"title": ""}}}, nil},
			"validation_error")
		id := w.freshIssue(w.p)
		v := w.issueVersion(id)
		kept422(t, "closeIssue with an unknown outcome",
			closeOf(id, map[string]any{"reason": "done", "closeOutcome": "nonsense"}), "validation_error")
		if w.issueVersion(id) != v {
			t.Errorf("a refused close changed the issue")
		}
	})

	t.Run("422 duplicate_suspected", func(t *testing.T) {
		title := "Idempotent duplicate " + uuid.NewString()[:8]
		original := testsupport.SeedIssue(t, w.pool, w.p, w.owner, w.pPrefix+"-777", title, "open", 2)
		h := idem()
		c := opCall{http.MethodPost, proj + "/issues", map[string]any{"title": title, "type": "task"}, nil}
		first := w.do(key, c, "", h)
		if first.code() != "duplicate_suspected" {
			t.Fatalf("a similar issue: %d %s", first.status, first.body)
		}
		// Without the original a fresh execution would create the issue: the
		// repeat must answer the kept 422.
		w.exec(`UPDATE issues SET title = $2 WHERE id = $1`, original, "renamed "+uuid.NewString())
		n := issuesOf()
		sameAnswer(t, "422 duplicate_suspected", first, w.do(key, c, "", h))
		if issuesOf() != n {
			t.Errorf("the repeat of a kept duplicate_suspected created an issue")
		}
	})

	t.Run("422 after a rollback", func(t *testing.T) {
		kept422(t, "createIssue with a missing edge target",
			opCall{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("idem"), "type": "task", "force": true,
				"dependencies": []map[string]any{{"toId": w.pPrefix + "-99999", "type": "blocks"}}}, nil}, "issue_not_found")
		kept422(t, "createEpic under a missing parent",
			opCall{http.MethodPost, proj + "/epics", map[string]any{"title": w.fresh("idem"), "parentId": w.pPrefix + "-99998",
				"childTemplates": []map[string]any{{"title": "c"}}}, nil}, "parent_not_found")
		id := w.freshIssue(w.p)
		v := w.issueVersion(id)
		kept422(t, "closeIssue as a duplicate of a missing issue",
			closeOf(id, map[string]any{"reason": "dup", "closeOutcome": "duplicate", "closeTarget": w.pPrefix + "-99997"}), "issue_not_found")
		var status string
		if err := w.pool.QueryRow(context.Background(), `SELECT status FROM issues WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if w.issueVersion(id) != v || status == "closed" {
			t.Errorf("the rolled back close left the issue %s at version %d (was %d)", status, w.issueVersion(id), v)
		}
	})

	t.Run("a force close re-checks the mode", func(t *testing.T) {
		// A project of the shared test space: its owner is an owner row, not
		// the space's implicit owner, so the role can be lowered.
		boss, _ := testsupport.SeedUser(t, w.pool, "")
		project, prefix := testsupport.SeedProject(t, w.pool, boss, "idemf")
		manage := w.key(boss, levelKey(apikeys.LevelManage))
		for _, lower := range []string{"role", "key"} {
			// Each case starts from the owner with the force right.
			w.exec(`UPDATE project_members SET role = 'owner' WHERE project_id = $1 AND user_id = $2`, project, boss)
			id := testsupport.SeedIssue(t, w.pool, project, boss, prefix+"-"+lower, "forced "+lower, "open", 2)
			w.exec(`UPDATE issues SET locked_by = 'mcp-session-other', locked_at = now(), status = 'in_progress' WHERE id = $1`, id)
			c := opCall{http.MethodPost, "/api/v1/issues/" + id + "/close", map[string]any{"reason": "forced", "closeOutcome": "done"},
				map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}}
			h := idem()
			if first := w.do(manage, c, "force=true", h); first.status != http.StatusOK {
				t.Fatalf("%s: force close: %d %s", lower, first.status, first.body)
			}
			var want string
			if lower == "role" {
				w.exec(`UPDATE project_members SET role = 'member' WHERE project_id = $1 AND user_id = $2`, project, boss)
				want = "force_unlock_forbidden"
			} else {
				w.exec(`UPDATE api_keys SET level = 'write' WHERE user_id = $1`, boss)
				want = "insufficient_scope"
			}
			if again := w.do(manage, c, "force=true", h); again.status != http.StatusForbidden || again.code() != want {
				t.Errorf("%s lowered: the repeat %d %s, want 403 %s", lower, again.status, again.body, want)
			}
		}
	})
}

// TestIdempotencyOps_CreateComment — row 137 (T026; US-7, SC-007, INV-8).
// createComment with Idempotency-Key: a 201 is kept and its repeat answers
// it again without a second comment, event or notification; a 422 before
// the store is kept, answered again, and nothing is written. Red when a
// repeat of createComment creates a comment or events.
func TestIdempotencyOps_CreateComment(t *testing.T) {
	w := newClassWorld(t)
	key := w.key(w.owner, levelKey(apikeys.LevelWrite))
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": "k-" + uuid.NewString()} }
	issue := w.freshIssue(w.p)
	// The comment mentions the member of the project: a mention notifies
	// him even of an agent's write (a subscriber is silenced by default), so
	// "no new notification" is asked of a count that the write moves.
	mention := "@" + testsupport.Username(t, w.pool, w.member)
	comments := func() int { return w.idemCount(`SELECT count(*) FROM comments WHERE issue_id = $1`, issue) }
	events := func() int { return w.idemCount(`SELECT count(*) FROM events WHERE project_id = $1`, w.p) }
	notifications := func() int {
		return w.idemCount(`SELECT count(*) FROM notifications n JOIN events e ON e.id = n.event_id WHERE e.project_id = $1`, w.p)
	}
	path := "/api/v1/issues/" + issue + "/comments"

	t.Run("2xx with a write", func(t *testing.T) {
		c, h := opCall{http.MethodPost, path, map[string]any{"body": "once " + w.fresh("c") + " " + mention}, nil}, idem()
		n0, nt0 := comments(), notifications()
		first := w.do(key, c, "", h)
		if first.status != http.StatusCreated {
			t.Fatalf("createComment: %d %s", first.status, first.body)
		}
		if comments() != n0+1 || notifications() == nt0 {
			t.Fatalf("the first request: comments %d→%d, notifications %d→%d — want one comment and its notification",
				n0, comments(), nt0, notifications())
		}
		if w.stored(h["Idempotency-Key"]) != http.StatusCreated {
			t.Errorf("the outcome is not kept")
		}
		n, e, nt := comments(), events(), notifications()
		sameAnswer(t, "createComment", first, w.do(key, c, "", h))
		if comments() != n || events() != e || notifications() != nt {
			t.Errorf("the repeat wrote: comments %d→%d, events %d→%d, notifications %d→%d",
				n, comments(), e, events(), nt, notifications())
		}
	})

	t.Run("422 before the store", func(t *testing.T) {
		c, h := opCall{http.MethodPost, path, map[string]any{"body": "   "}, nil}, idem()
		n, e, nt := comments(), events(), notifications()
		first := w.do(key, c, "", h)
		if first.status != http.StatusUnprocessableEntity || first.code() != "validation_error" {
			t.Fatalf("an empty body: %d %s, want 422 validation_error", first.status, first.body)
		}
		if w.stored(h["Idempotency-Key"]) != http.StatusUnprocessableEntity {
			t.Errorf("the 422 is not kept")
		}
		sameAnswer(t, "an empty body", first, w.do(key, c, "", h))
		if comments() != n || events() != e || notifications() != nt {
			t.Errorf("written: comments %d→%d, events %d→%d, notifications %d→%d",
				n, comments(), e, events(), nt, notifications())
		}
	})
}

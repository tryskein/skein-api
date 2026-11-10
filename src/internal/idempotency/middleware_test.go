package idempotency_test

// middleware_test.go — rows 73–77: the Idempotency-Key mechanism of US-7 on
// the test routes of rig_test.go, against the migrated test database. The
// reference is the spec: the identity of a request (key, operationId, target
// of the path, body, Prefer), 409 while the first request runs, 422 for the
// key reused with another body, the failure window between the commit and
// the body, the re-check before a stored answer is given out, what is kept,
// and 400 for a key the operation does not accept.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"skein-api/internal/apikeys"
	"skein-api/internal/errcode"
	"skein-api/internal/idempotency"
	"skein-api/internal/publicapi/apitest"
	"skein-api/internal/testsupport"
)

func notesPath(issue string) string { return fmt.Sprintf(notesPathTmpl, issue) }

// TestIdempotency_IdentityOfARequest — row 73 (US-7, INV-8; identity by the
// lead's decision in sk-878.3.1, review R6-01). A repeat with the same
// identity answers the first outcome — the same status, id and version — and
// executes nothing. The same key with any other identity — another body,
// force mode, target of the path or Prefer — is 422 idempotency_key_reused
// and executes nothing; the first outcome still answers its own repeat. A
// repeat while the first request's marker lives is 409
// idempotency_in_progress.
func TestIdempotency_IdentityOfARequest(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	t.Run("same identity, then every other identity under the key, then the first again", func(t *testing.T) {
		rg := newRig(t, pool)
		w := seedWorld(t, pool, apikeys.LevelManage)
		first := call{path: notesPath(w.issueA), key: w.key, idemKey: w.idemKeys}

		orig := decodeNote(t, rg.do(t, first))
		if orig.ID != w.issueA || orig.Version != 2 {
			t.Fatalf("first: id %s version %d, want %s version 2", orig.ID, orig.Version, w.issueA)
		}

		again := rg.do(t, first)
		if got := decodeNote(t, again); got.ID != orig.ID || got.Version != orig.Version {
			t.Errorf("repeat: id %s version %d, want %s version %d", got.ID, got.Version, orig.ID, orig.Version)
		}
		if etag := again.Header().Get("ETag"); etag != `"2"` {
			t.Errorf(`repeat ETag %q, want "2"`, etag)
		}
		if n := rg.calls.Load(); n != 1 {
			t.Errorf("repeat: the operation ran %d times, want 1", n)
		}

		reused := first
		reused.body = `{"text":"two"}`
		wantProblem(t, rg.do(t, reused), http.StatusUnprocessableEntity, "idempotency_key_reused")
		if n := rg.calls.Load(); n != 1 {
			t.Errorf("other body: the operation ran %d times, want 1", n)
		}

		otherMode := first
		otherMode.path = notesPath(w.issueA) + "?force=true"
		wantProblem(t, rg.do(t, otherMode), http.StatusUnprocessableEntity, "idempotency_key_reused")
		if n := rg.calls.Load(); n != 1 {
			t.Errorf("other force mode: the operation ran %d times, want 1", n)
		}

		otherTarget := first
		otherTarget.path = notesPath(w.issueB)
		wantProblem(t, rg.do(t, otherTarget), http.StatusUnprocessableEntity, "idempotency_key_reused")
		otherPrefer := first
		otherPrefer.headers = map[string]string{"Prefer": "return=minimal"}
		wantProblem(t, rg.do(t, otherPrefer), http.StatusUnprocessableEntity, "idempotency_key_reused")
		if n := rg.calls.Load(); n != 1 {
			t.Errorf("other target and other Prefer: the operation ran %d times in all, want 1", n)
		}

		back := rg.do(t, first)
		if got := decodeNote(t, back); got.ID != orig.ID || got.Version != orig.Version || got.NoteID != orig.NoteID {
			t.Errorf("first again: id %s version %d note %s, want the first outcome %s version %d note %s",
				got.ID, got.Version, got.NoteID, orig.ID, orig.Version, orig.NoteID)
		}
		if n := rg.calls.Load(); n != 1 {
			t.Errorf("first again: the operation ran %d times in all, want 1", n)
		}
		if a, b := notes(t, pool, w.issueA), notes(t, pool, w.issueB); a != 1 || b != 0 {
			t.Errorf("notes: %d on A, %d on B; want 1 (the first request) and 0", a, b)
		}
	})

	t.Run("repeat while the marker lives: 409", func(t *testing.T) {
		rg := newRig(t, pool)
		w := seedWorld(t, pool, apikeys.LevelWrite)
		body := `{"text":"one"}`
		sum := sha256.Sum256([]byte(body))
		d, err := idempotency.NewStore(pool).Begin(context.Background(), idempotency.Identity{
			KeyID: w.keyID, Key: w.idemKeys, OperationID: opAddNote, Target: notesPath(w.issueA),
			Fingerprint: sum[:],
		}, time.Minute)
		if err != nil || d.Verdict != idempotency.Execute {
			t.Fatalf("first marker: verdict %v, err %v", d.Verdict, err)
		}
		rec := rg.do(t, call{path: notesPath(w.issueA), key: w.key, idemKey: w.idemKeys, body: body})
		wantProblem(t, rec, http.StatusConflict, "idempotency_in_progress")
		if n := rg.calls.Load(); n != 0 {
			t.Errorf("the operation ran %d times, want 0", n)
		}
	})
}

// TestIdempotency_FailureWindow — row 74 (US-7 failure window, OST-3-5). The
// operation commits, the body cannot be written after the commit (a test
// trigger on the UPDATE of the body of this key), another client changes the
// issue. A repeat while the marker lives is 409; once it ended (moved by
// writing the row), the repeat answers the recorded status, id and version
// (ETag the recorded version) with the other fields as they are now, and
// executes nothing.
func TestIdempotency_FailureWindow(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newRig(t, pool)
	w := seedWorld(t, pool, apikeys.LevelWrite)

	fn := "idem_fail_body_" + fmt.Sprintf("%x", sha256.Sum256([]byte(w.idemKeys)))[:12]
	exec(t, pool, "create the failing function", fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'injected failure of the body write';
END $$`, fn))
	exec(t, pool, "create the failing trigger", fmt.Sprintf(`
CREATE TRIGGER %s BEFORE UPDATE OF response_body ON api_idempotency
FOR EACH ROW WHEN (NEW.idempotency_key = '%s' AND NEW.response_body IS NOT NULL)
EXECUTE FUNCTION %s()`, fn, w.idemKeys, fn))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON api_idempotency`, fn))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})

	first := call{path: notesPath(w.issueA), key: w.key, idemKey: w.idemKeys}
	orig := decodeNote(t, rg.do(t, first))
	var bodyStored bool
	if err := pool.QueryRow(context.Background(), `
SELECT response_body IS NOT NULL FROM api_idempotency WHERE api_key_id = $1 AND idempotency_key = $2`,
		w.keyID, w.idemKeys).Scan(&bodyStored); err != nil {
		t.Fatalf("read the outcome: %v", err)
	}
	if bodyStored {
		t.Fatal("the body was stored although its write was made to fail")
	}

	exec(t, pool, "another client changes the issue",
		`UPDATE issues SET title = 'changed by another client', version = version + 1 WHERE id = $1`, w.issueA)

	wantProblem(t, rg.do(t, first), http.StatusConflict, "idempotency_in_progress")

	exec(t, pool, "end the marker", `
UPDATE api_idempotency SET in_progress_until = now() - interval '1 second'
WHERE api_key_id = $1 AND idempotency_key = $2`, w.keyID, w.idemKeys)
	rec := rg.do(t, first)
	got := decodeNote(t, rec)
	if got.ID != orig.ID || got.Version != orig.Version {
		t.Errorf("id %s version %d, want the recorded %s version %d", got.ID, got.Version, orig.ID, orig.Version)
	}
	if etag := rec.Header().Get("ETag"); etag != fmt.Sprintf(`"%d"`, orig.Version) {
		t.Errorf("ETag %q, want the recorded version %d", etag, orig.Version)
	}
	if got.Title != "changed by another client" {
		t.Errorf("title %q, want the current one", got.Title)
	}
	if n := rg.calls.Load(); n != 1 {
		t.Errorf("the operation ran %d times, want 1", n)
	}
	if n := notes(t, pool, w.issueA); n != 1 {
		t.Errorf("%d notes, want 1", n)
	}
}

// TestIdempotency_RecheckBeforeTheStoredAnswer — row 75 (US-7, SC-027,
// OST-2-4, OST-3-6, edge «key revoked between the request and the repeat»).
// Between the first request and its repeat: the key revoked — 401; the key's
// scope narrowed past the issue — as a missing issue; the owner's role
// lowered below the route's action — 403; the first request in force mode,
// the owner left without the action that mode adds, the repeat without force
// — 403; the key's right lowered to write after a force request — 403
// insufficient_scope. In no branch is the stored answer given out.
func TestIdempotency_RecheckBeforeTheStoredAnswer(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	for _, tc := range []struct {
		name   string
		path   func(world) string
		force  bool
		change func(t *testing.T, w world)
		status int
		code   string
	}{
		{
			name: "key revoked",
			path: func(w world) string { return notesPath(w.issueA) },
			change: func(t *testing.T, w world) {
				exec(t, pool, "revoke", `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, w.keyID)
			},
			status: http.StatusUnauthorized, code: "unauthenticated",
		},
		{
			name: "scope narrowed past the issue",
			path: func(w world) string { return notesPath(w.issueA) },
			change: func(t *testing.T, w world) {
				exec(t, pool, "narrow the scope", `UPDATE api_keys SET scope_mode = 'projects' WHERE id = $1`, w.keyID)
				exec(t, pool, "scope the other project", `INSERT INTO api_key_scope_projects (api_key_id, project_id) VALUES ($1, $2)`, w.keyID, w.other)
			},
			status: http.StatusNotFound, code: "issue_not_found",
		},
		{
			name: "owner's role lowered below the action",
			path: func(w world) string { return "/api/v1/issues/" + w.issueA + "/managed-notes" },
			change: func(t *testing.T, w world) {
				exec(t, pool, "lower the role", `UPDATE project_members SET role = 'member' WHERE project_id = $1 AND user_id = $2`, w.project, w.owner)
			},
			status: http.StatusForbidden, code: "forbidden",
		},
		{
			name:  "force request, owner left without force_unlock",
			path:  func(w world) string { return notesPath(w.issueA) + "?force=true" },
			force: true,
			change: func(t *testing.T, w world) {
				exec(t, pool, "lower the role", `UPDATE project_members SET role = 'member' WHERE project_id = $1 AND user_id = $2`, w.project, w.owner)
			},
			status: http.StatusForbidden, code: "force_unlock_forbidden",
		},
		{
			name:  "force request, key lowered to write",
			path:  func(w world) string { return notesPath(w.issueA) + "?force=true" },
			force: true,
			change: func(t *testing.T, w world) {
				exec(t, pool, "lower the key", `UPDATE api_keys SET level = 'write' WHERE id = $1`, w.keyID)
			},
			status: http.StatusForbidden, code: "insufficient_scope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rg := newRig(t, pool)
			w := seedWorld(t, pool, apikeys.LevelManage)
			first := call{path: tc.path(w), key: w.key, idemKey: w.idemKeys}
			decodeNote(t, rg.do(t, first))
			tc.change(t, w)
			rec := rg.do(t, first)
			wantProblem(t, rec, tc.status, tc.code)
			if n := rg.calls.Load(); n != 1 {
				t.Errorf("the operation ran %d times, want 1", n)
			}
		})
	}
}

// TestIdempotency_WhatIsKept — row 76 (US-7 «what is kept», D-11). By the
// literal reference apitest.Codes (not by errcode): every code at every
// origin it may come from is answered twice with the same key; a 422 whose
// code is not literally retryable is kept — the repeat does not execute —
// and every other answer (a retryable 422, 400, 401, 403, 404, 409, 412,
// 428, 429, 5xx) is not — the repeat executes again. A 2xx without a write is
// kept. Every code of errcode has a row in the literal reference.
func TestIdempotency_WhatIsKept(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newRig(t, pool)
	w := seedWorld(t, pool, apikeys.LevelWrite)

	for _, c := range errcode.All() {
		if _, ok := apitest.Codes[c.String()]; !ok {
			t.Errorf("errcode %q has no row in apitest.Codes", c.String())
		}
	}

	n := 0
	ask := func(t *testing.T, body codeBody) (kept bool) {
		t.Helper()
		raw, _ := json.Marshal(body)
		n++
		c := call{path: "/api/v1/answers", key: w.key, idemKey: fmt.Sprintf("%s-%d", w.idemKeys, n), body: string(raw)}
		before := rg.calls.Load()
		firstRec := rg.do(t, c)
		second := rg.do(t, c)
		if second.Code != firstRec.Code {
			t.Errorf("repeat status %d, first %d", second.Code, firstRec.Code)
		}
		switch ran := rg.calls.Load() - before; ran {
		case 1:
			return true
		case 2:
			return false
		default:
			t.Fatalf("the operation ran %d times for two requests", ran)
			return false
		}
	}

	for name, want := range apitest.Codes {
		for origin, status := range want.Status {
			t.Run(fmt.Sprintf("%s from %s", name, origin), func(t *testing.T) {
				wantKept := status == http.StatusUnprocessableEntity && !want.Retryable
				if kept := ask(t, codeBody{Code: name, Origin: string(origin)}); kept != wantKept {
					t.Errorf("status %d retryable %v: kept %v, want %v", status, want.Retryable, kept, wantKept)
				}
			})
		}
	}
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(fmt.Sprintf("%d without a write", status), func(t *testing.T) {
			if !ask(t, codeBody{Status: status}) {
				t.Error("a success was not kept")
			}
		})
	}
}

// TestIdempotency_KeyNotAccepted — row 77 (OST-2-7). Idempotency-Key on a
// single operation that does not accept it and on a batch is 400
// idempotency_not_supported, and the operation does not run.
func TestIdempotency_KeyNotAccepted(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newRig(t, pool)
	w := seedWorld(t, pool, apikeys.LevelWrite)

	for name, path := range map[string]string{
		"single operation": "/api/v1/issues/" + w.issueA + "/edit",
		"batch":            "/api/v1/notes/bulk",
	} {
		t.Run(name, func(t *testing.T) {
			before := rg.calls.Load()
			wantProblem(t, rg.do(t, call{path: path, key: w.key, idemKey: w.idemKeys}),
				http.StatusBadRequest, "idempotency_not_supported")
			if ran := rg.calls.Load() - before; ran != 0 {
				t.Errorf("the operation ran %d times, want 0", ran)
			}
		})
	}
}

// TestIdempotency_CloseRecordedInTheChangeTransaction — the hook of
// closeIssue (D-8 rule 2 at the T015 attachment point): a close through
// issues.Store.UpdateIssue with idempotency.IssueBeforeCommit records the
// committed version of the issue in that transaction; the repeat answers it
// and closes nothing again — one issue.closed event.
func TestIdempotency_CloseRecordedInTheChangeTransaction(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newRig(t, pool)
	w := seedWorld(t, pool, apikeys.LevelWrite)
	c := call{path: "/api/v1/issues/" + w.issueA + "/close", key: w.key, idemKey: w.idemKeys, body: "{}"}

	first := rg.do(t, c)
	if first.Code != http.StatusOK {
		t.Fatalf("close: status %d (body %q)", first.Code, first.Body.String())
	}
	var version int64
	if err := pool.QueryRow(context.Background(), `
SELECT response_version FROM api_idempotency WHERE api_key_id = $1 AND idempotency_key = $2`,
		w.keyID, w.idemKeys).Scan(&version); err != nil {
		t.Fatalf("read the recorded outcome: %v", err)
	}
	var current int64
	if err := pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, w.issueA).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if version != current {
		t.Errorf("recorded version %d, the committed version of the issue %d", version, current)
	}

	again := rg.do(t, c)
	if again.Code != http.StatusOK || again.Header().Get("ETag") != first.Header().Get("ETag") {
		t.Errorf("repeat: status %d ETag %q, want 200 and %q", again.Code, again.Header().Get("ETag"), first.Header().Get("ETag"))
	}
	if n := rg.calls.Load(); n != 1 {
		t.Errorf("the operation ran %d times, want 1", n)
	}
	if n := testsupport.CountEventsForProject(t, pool, w.project, "issue.closed"); n != 1 {
		t.Errorf("%d issue.closed events, want 1", n)
	}
}

package idempotency_test

// middleware_test.go — rows 73–77: the Idempotency-Key mechanism of US-7 on
// the test routes of rig_test.go, against the migrated test database. The
// reference is the spec: the identity of a request (key, operationId, target
// of the path, body, Prefer), 409 while the first request runs, 422 for the
// key reused with another body, the failure window between the commit and
// the body, the re-check before a stored answer is given out, what is kept,
// and 400 for a key the operation does not accept.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/errcode"
	"skein-api/internal/idempotency"
	"skein-api/internal/publicapi"
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

// TestIdempotency_RecheckOfAnOutcomeWithoutAWrite — row 75 for an outcome
// stored without a writing transaction (US-7, SC-027, review R6-02): a 422
// whose body answers with an issue (as the candidates of
// duplicate_suspected) names it as an object of the response; after the
// key's scope is narrowed past that issue the repeat answers as for a
// missing issue, the stored 422 is not given out and nothing executes.
func TestIdempotency_RecheckOfAnOutcomeWithoutAWrite(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newRig(t, pool)
	w := seedWorld(t, pool, apikeys.LevelWrite)
	raw, _ := json.Marshal(codeBody{Code: "dependency_cycle", Origin: "domain",
		Objects: []publicapi.ObjectRef{{Kind: publicapi.ObjectIssue, ID: w.issueA}}})
	c := call{path: "/api/v1/answers", key: w.key, idemKey: w.idemKeys, body: string(raw)}

	wantProblem(t, rg.do(t, c), http.StatusUnprocessableEntity, "dependency_cycle")
	wantProblem(t, rg.do(t, c), http.StatusUnprocessableEntity, "dependency_cycle")
	if n := rg.calls.Load(); n != 1 {
		t.Fatalf("the operation ran %d times before the scope changed, want 1 (the 422 is kept)", n)
	}

	exec(t, pool, "narrow the scope", `UPDATE api_keys SET scope_mode = 'projects' WHERE id = $1`, w.keyID)
	exec(t, pool, "scope the other project", `INSERT INTO api_key_scope_projects (api_key_id, project_id) VALUES ($1, $2)`, w.keyID, w.other)
	wantProblem(t, rg.do(t, c), http.StatusNotFound, "issue_not_found")
	if n := rg.calls.Load(); n != 1 {
		t.Errorf("the operation ran %d times, want 1", n)
	}
}

// TestIdempotency_WhatIsKept — row 76 (US-7 «what is kept», D-11). By the
// literal reference apitest.Codes (not by errcode): every code at every
// origin it may come from is answered twice with the same key; a 422 whose
// code is not literally retryable is kept — the repeat does not execute —
// and every other answer (a retryable 422, 400, 401, 403, 404, 409, 412,
// 428, 429, 5xx) is not — the repeat executes again. A 2xx without a write is
// kept. On a route with owner actions an outcome that named no target is
// not kept (review R6-03). Every code of errcode has a row in the literal
// reference.
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
	t.Run("422 of an operation with owner actions that named no target: not kept", func(t *testing.T) {
		raw, _ := json.Marshal(codeBody{Code: "dependency_cycle", Origin: "domain"})
		c := call{path: "/api/v1/issues/" + w.issueA + "/answers", key: w.key, idemKey: w.idemKeys + "-notarget", body: string(raw)}
		before := rg.calls.Load()
		wantProblem(t, rg.do(t, c), http.StatusUnprocessableEntity, "dependency_cycle")
		wantProblem(t, rg.do(t, c), http.StatusUnprocessableEntity, "dependency_cycle")
		if ran := rg.calls.Load() - before; ran != 2 {
			t.Errorf("the operation ran %d times, want 2 (an outcome the repeat cannot re-check is not kept)", ran)
		}
	})
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

// ----------------- sk-1007 US-6: a repeated refusal narrowed by visibility -----------------
//
// Reference — spec US-6 (D7), written before the code. A repeat of a stored
// dependency_cycle or standing_has_open_children refusal does not run the
// operation and keeps status and code; its body is the stored one narrowed
// by visibility at the repeat: the path only when all of it is visible;
// childIds only the visible ones and unavailableOpenChildren = the stored
// number (0 when absent) + the stored ids hidden now; another reference
// member (toId) of a hidden issue becomes "<hidden>"; nothing hidden by the
// first answer comes back; a deleted issue, or one moved to a project the
// caller does not see, is hidden. A failed re-check of the target answers
// that check, never the stored body.
//
// The bodies are literals stored through the rig's refusing operation
// (rig.refuse): what a builder of this version, or of an earlier one,
// wrote.

// refusalWorld: the key's owner sees P (target T, child P-1) and Q, a
// project of another user he is a member of until a test takes him out.
type refusalWorld struct {
	rg      *rig
	pool    *pgxpool.Pool
	w       world
	target  string
	p1      string
	q       uuid.UUID
	qPrefix string
	qOwner  uuid.UUID
	n       int
}

func newRefusalWorld(t *testing.T) *refusalWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	rw := &refusalWorld{rg: newRig(t, pool), pool: pool, w: seedWorld(t, pool, apikeys.LevelWrite)}
	rw.target, rw.p1 = rw.w.issueA, rw.w.issueB
	rw.qOwner, _ = testsupport.SeedUser(t, pool, "")
	rw.q, rw.qPrefix = testsupport.SeedProject(t, pool, rw.qOwner, "idemq")
	testsupport.AddProjectMember(t, pool, rw.q, rw.w.owner, "member")
	return rw
}

func (rw *refusalWorld) qIssue(t *testing.T) string {
	t.Helper()
	rw.n++
	return testsupport.SeedIssue(t, rw.pool, rw.q, rw.qOwner, rw.qPrefix+"-"+fmt.Sprint(rw.n), "q", "open", 2)
}

func (rw *refusalWorld) lose(t *testing.T) {
	t.Helper()
	exec(t, rw.pool, "leave Q", `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, rw.q, rw.w.owner)
}

func (rw *refusalWorld) regain(t *testing.T) {
	t.Helper()
	testsupport.AddProjectMember(t, rw.pool, rw.q, rw.w.owner, "member")
}

// refuse sends the refusing operation on target with the idempotency key
// idem and the problem members (after type, title, status) as its body.
func (rw *refusalWorld) refuse(t *testing.T, target, idem, members string) *httptest.ResponseRecorder {
	t.Helper()
	problem := `{"type":"about:blank","title":"Refused","status":422,` + members + `}`
	raw, err := json.Marshal(refuseBody{Problem: json.RawMessage(problem)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return rw.rg.do(t, call{path: "/api/v1/issues/" + target + "/refusals", key: rw.w.key, idemKey: idem, body: string(raw)})
}

// store answers the refusal once (it is kept) and returns the first answer.
func (rw *refusalWorld) store(t *testing.T, idem, members string) *httptest.ResponseRecorder {
	t.Helper()
	first := rw.refuse(t, rw.target, idem, members)
	if first.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first answer %d %s, want 422", first.Code, first.Body.String())
	}
	return first
}

// repeated sends the same request again and asserts that the operation did
// not run and nothing was written.
func (rw *refusalWorld) repeated(t *testing.T, idem, members string) *httptest.ResponseRecorder {
	t.Helper()
	calls, notesBefore := rw.rg.calls.Load(), notes(t, rw.pool, rw.target)
	rec := rw.refuse(t, rw.target, idem, members)
	if n := rw.rg.calls.Load(); n != calls {
		t.Errorf("the repeat ran the operation (%d calls, was %d)", n, calls)
	}
	if n := notes(t, rw.pool, rw.target); n != notesBefore {
		t.Errorf("the repeat wrote %d notes", n-notesBefore)
	}
	return rec
}

// members reads a JSON object's top-level members in order.
func membersOf(t *testing.T, body []byte) ([]string, map[string]any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %s", body)
	}
	var keys []string
	vals := map[string]any{}
	for dec.More() {
		tok, _ := dec.Token()
		k := tok.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode %s: %v", k, err)
		}
		keys = append(keys, k)
		vals[k] = v
	}
	return keys, vals
}

// TestIdempotency_RefusalRepeatUnchangedAccess — sk-1007 No. 55: with access
// unchanged a cycle with a visible path, an open-children refusal of
// visible children and a refusal of another code naming a hidden id come
// back byte for byte.
func TestIdempotency_RefusalRepeatUnchangedAccess(t *testing.T) {
	rw := newRefusalWorld(t)
	hidden := rw.qIssue(t)
	rw.lose(t) // hidden is out of reach throughout; the other code must not be narrowed
	for i, m := range []string{
		`"code":"dependency_cycle","path":["` + rw.target + `","` + rw.p1 + `","` + rw.target + `"]`,
		`"code":"standing_has_open_children","id":"` + rw.target + `","childIds":["` + rw.p1 + `"],"unavailableOpenChildren":0`,
		`"code":"parent_closed","id":"` + hidden + `","detail":"x"`,
	} {
		idem := fmt.Sprintf("%s-same-%d", rw.w.idemKeys, i)
		first := rw.store(t, idem, m)
		again := rw.repeated(t, idem, m)
		if again.Code != first.Code || again.Body.String() != first.Body.String() {
			t.Errorf("repeat of %s: %d %s, want the stored %d %s", m, again.Code, again.Body.String(), first.Code, first.Body.String())
		}
	}
}

// TestIdempotency_RefusalRepeatAfterLoss — sk-1007 No. 56: the examples of
// the acceptance after access to the issues of Q is lost.
func TestIdempotency_RefusalRepeatAfterLoss(t *testing.T) {
	rw := newRefusalWorld(t)
	q1, q2 := rw.qIssue(t), rw.qIssue(t)
	cases := []struct {
		name, members string
		wantKeys      []string
		want          map[string]any
	}{
		{"path through Q",
			`"code":"dependency_cycle","path":["` + rw.target + `","` + q1 + `","` + rw.target + `"],"detail":"d"`,
			[]string{"type", "title", "status", "code", "detail"}, map[string]any{}},
		{"stored list and number",
			`"code":"standing_has_open_children","id":"` + rw.target + `","childIds":["` + q1 + `"],"unavailableOpenChildren":1,"detail":"d"`,
			[]string{"type", "title", "status", "code", "id", "childIds", "unavailableOpenChildren", "detail"},
			map[string]any{"childIds": []any{}, "unavailableOpenChildren": float64(2)}},
		{"earlier version without the number",
			`"code":"standing_has_open_children","id":"` + rw.target + `","childIds":["` + q2 + `"],"detail":"d"`,
			[]string{"type", "title", "status", "code", "id", "childIds", "unavailableOpenChildren", "detail"},
			map[string]any{"childIds": []any{}, "unavailableOpenChildren": float64(1)}},
		{"earlier version, all visible",
			`"code":"standing_has_open_children","id":"` + rw.target + `","childIds":["` + rw.p1 + `"],"detail":"d"`,
			[]string{"type", "title", "status", "code", "id", "childIds", "unavailableOpenChildren", "detail"},
			map[string]any{"childIds": []any{rw.p1}, "unavailableOpenChildren": float64(0)}},
	}
	for i, c := range cases {
		rw.store(t, fmt.Sprintf("%s-loss-%d", rw.w.idemKeys, i), c.members)
	}
	rw.lose(t)
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := rw.repeated(t, fmt.Sprintf("%s-loss-%d", rw.w.idemKeys, i), c.members)
			wantProblem(t, rec, http.StatusUnprocessableEntity, codeOf(c.members))
			keys, vals := membersOf(t, rec.Body.Bytes())
			if !reflect.DeepEqual(keys, c.wantKeys) {
				t.Errorf("members %v, want %v (order kept, path removed, the number beside childIds)", keys, c.wantKeys)
			}
			for k, v := range c.want {
				if !reflect.DeepEqual(vals[k], v) {
					t.Errorf("%s = %#v, want %#v", k, vals[k], v)
				}
			}
			for _, id := range []string{q1, q2} {
				if strings.Contains(rec.Body.String(), `"`+id+`"`) {
					t.Errorf("the repeat names %s, hidden now: %s", id, rec.Body.String())
				}
			}
		})
	}
}

// codeOf reads the code of a members literal.
func codeOf(members string) string {
	var m struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte("{"+members+"}"), &m)
	return m.Code
}

// TestIdempotency_RefusalRepeatAfterAccessReturns — sk-1007 No. 57: what
// the first answer hid does not come back when access returns.
func TestIdempotency_RefusalRepeatAfterAccessReturns(t *testing.T) {
	rw := newRefusalWorld(t)
	rw.qIssue(t)
	rw.lose(t)
	stored := []string{
		`"code":"dependency_cycle","detail":"d"`,
		`"code":"standing_has_open_children","id":"` + rw.target + `","childIds":[],"unavailableOpenChildren":2`,
	}
	firsts := make([]string, len(stored))
	for i, m := range stored {
		firsts[i] = rw.store(t, fmt.Sprintf("%s-back-%d", rw.w.idemKeys, i), m).Body.String()
	}
	rw.regain(t)
	for i, m := range stored {
		rec := rw.repeated(t, fmt.Sprintf("%s-back-%d", rw.w.idemKeys, i), m)
		if rec.Body.String() != firsts[i] {
			t.Errorf("after access returned: %s, want the stored %s (never wider)", rec.Body.String(), firsts[i])
		}
	}
}

// TestIdempotency_RefusalRepeatOtherReferences — sk-1007 No. 58: toId of an
// inline dependency refusal of a hidden issue is "<hidden>", a visible one
// stays; a stored child deleted, or moved to a project the caller does not
// see, is counted as hidden.
func TestIdempotency_RefusalRepeatOtherReferences(t *testing.T) {
	rw := newRefusalWorld(t)
	h5 := rw.qIssue(t)
	prefix := strings.TrimSuffix(rw.w.issueA, "-1")
	deleted := testsupport.SeedIssue(t, rw.pool, rw.w.project, rw.w.owner, prefix+"-71", "gone", "open", 2)
	moved := testsupport.SeedIssue(t, rw.pool, rw.w.project, rw.w.owner, prefix+"-72", "moved", "open", 2)
	hiddenOwner, _ := testsupport.SeedUser(t, rw.pool, "")
	hiddenProject, _ := testsupport.SeedProject(t, rw.pool, hiddenOwner, "idemh")
	stored := map[string]string{
		"hidden toId":  `"code":"dependency_cycle","dependencyIndex":0,"toId":"` + h5 + `","type":"blocks"`,
		"visible toId": `"code":"dependency_cycle","dependencyIndex":0,"toId":"` + rw.p1 + `","type":"blocks"`,
		"gone children": `"code":"standing_has_open_children","id":"` + rw.target + `","childIds":["` + deleted + `","` + moved + `","` + rw.p1 +
			`"],"unavailableOpenChildren":0`,
	}
	firsts := map[string]string{}
	for name, m := range stored {
		firsts[name] = rw.store(t, rw.w.idemKeys+"-ref-"+strings.ReplaceAll(name, " ", "-"), m).Body.String()
	}
	rw.lose(t)
	exec(t, rw.pool, "delete a child", `DELETE FROM issues WHERE id = $1`, deleted)
	exec(t, rw.pool, "move a child away", `UPDATE issues SET project_id = $2 WHERE id = $1`, moved, hiddenProject)
	t.Cleanup(func() {
		_, _ = rw.pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, moved)
	})

	_, vals := membersOf(t, rw.repeated(t, rw.w.idemKeys+"-ref-hidden-toId", stored["hidden toId"]).Body.Bytes())
	if vals["toId"] != "<hidden>" {
		t.Errorf("hidden toId = %v, want <hidden>", vals["toId"])
	}
	if rec := rw.repeated(t, rw.w.idemKeys+"-ref-visible-toId", stored["visible toId"]); rec.Body.String() != firsts["visible toId"] {
		t.Errorf("visible toId: %s, want the stored %s", rec.Body.String(), firsts["visible toId"])
	}
	rec := rw.repeated(t, rw.w.idemKeys+"-ref-gone-children", stored["gone children"])
	_, vals = membersOf(t, rec.Body.Bytes())
	if !reflect.DeepEqual(vals["childIds"], []any{rw.p1}) || vals["unavailableOpenChildren"] != float64(2) {
		t.Errorf("deleted and moved children: childIds %v, unavailableOpenChildren %v; want [%s] and 2",
			vals["childIds"], vals["unavailableOpenChildren"], rw.p1)
	}
	for _, id := range []string{deleted, moved} {
		if strings.Contains(rec.Body.String(), `"`+id+`"`) {
			t.Errorf("the repeat names %s: %s", id, rec.Body.String())
		}
	}
}

// TestIdempotency_RefusalRepeatKeyScopeAtTheRepeat — sk-1007 No. 59: the
// same key, its scope narrowed to P before the repeat; the child in Q, which
// the owner still sees, is counted as unavailable.
func TestIdempotency_RefusalRepeatKeyScopeAtTheRepeat(t *testing.T) {
	rw := newRefusalWorld(t)
	qc := rw.qIssue(t)
	m := `"code":"standing_has_open_children","id":"` + rw.target + `","childIds":["` + rw.p1 + `","` + qc + `"],"unavailableOpenChildren":0`
	idem := rw.w.idemKeys + "-scope"
	rw.store(t, idem, m)
	exec(t, rw.pool, "narrow the scope", `UPDATE api_keys SET scope_mode = 'projects' WHERE id = $1`, rw.w.keyID)
	exec(t, rw.pool, "scope P", `INSERT INTO api_key_scope_projects (api_key_id, project_id) VALUES ($1, $2)`, rw.w.keyID, rw.w.project)
	_, vals := membersOf(t, rw.repeated(t, idem, m).Body.Bytes())
	if !reflect.DeepEqual(vals["childIds"], []any{rw.p1}) || vals["unavailableOpenChildren"] != float64(1) {
		t.Errorf("childIds %v, unavailableOpenChildren %v; want [%s] and 1", vals["childIds"], vals["unavailableOpenChildren"], rw.p1)
	}
}

// TestIdempotency_RefusalRepeatTargetGone — sk-1007 No. 60: when the target
// is out of reach, or deleted, the repeat answers the re-check — the status
// and code a repeat of another stored refusal on the same target answers —
// and none of the stored body.
func TestIdempotency_RefusalRepeatTargetGone(t *testing.T) {
	for _, gone := range []string{"out of reach", "deleted"} {
		t.Run(gone, func(t *testing.T) {
			rw := newRefusalWorld(t)
			target, child := rw.qIssue(t), rw.qIssue(t)
			listed := `"code":"standing_has_open_children","id":"` + target + `","childIds":["` + child + `"],"unavailableOpenChildren":0,"path":["` + child + `"]`
			other := `"code":"parent_closed","detail":"d"`
			for name, m := range map[string]string{"listed": listed, "other": other} {
				if rec := rw.refuse(t, target, rw.w.idemKeys+"-gone-"+name, m); rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("store %s: %d %s", name, rec.Code, rec.Body.String())
				}
			}
			if gone == "deleted" {
				exec(t, rw.pool, "delete the target", `DELETE FROM issues WHERE id = $1`, target)
			} else {
				rw.lose(t)
			}
			calls := rw.rg.calls.Load()
			listedRec := rw.refuse(t, target, rw.w.idemKeys+"-gone-listed", listed)
			otherRec := rw.refuse(t, target, rw.w.idemKeys+"-gone-other", other)
			if n := rw.rg.calls.Load(); n != calls {
				t.Errorf("the repeats ran the operation %d times", n-calls)
			}
			if listedRec.Code != otherRec.Code || problemCode(t, listedRec) != problemCode(t, otherRec) {
				t.Errorf("listed refusal repeat %d %s, other refusal repeat %d %s — want the same re-check answer",
					listedRec.Code, listedRec.Body.String(), otherRec.Code, otherRec.Body.String())
			}
			if listedRec.Code == http.StatusUnprocessableEntity {
				t.Errorf("the stored refusal was given out: %s", listedRec.Body.String())
			}
			for _, s := range []string{`"childIds"`, `"path"`, `"` + target + `"`, `"` + child + `"`} {
				if strings.Contains(listedRec.Body.String(), s) {
					t.Errorf("the answer carries %s: %s", s, listedRec.Body.String())
				}
			}
		})
	}
}

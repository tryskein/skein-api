package handler

// issues_crossproject_write_test.go — sk-194.6.50.
//
// # What is under test
//
// Every MUTATING route shaped /projects/{projectID}/issues/{issueID}/... on
// IssuesHandler. Issue ids are globally unique and carry their own project,
// while the ACL runs against the project segment of the path — so nothing in
// the URL forces the two to agree. Before this fix a caller who could see
// project A could name a project-B issue and have the write land in B:
// Broken Object Level Authorization (OWASP API1). The project-level check
// passed; it was simply never the right project's check.
//
// The routes, enumerated so the claim above is checkable against the router
// rather than believed:
//
//	PATCH  /issues/{issueID}
//	POST   /issues/{issueID}/transitions
//	POST   /issues/{issueID}/pin
//	POST   /issues/{issueID}/unpin
//	PUT    /issues/{issueID}/components
//	POST   /issues/{issueID}/labels
//	DELETE /issues/{issueID}/labels/{label}
//	POST   /issues/{issueID}/comments
//	PUT    /issues/{issueID}/comments/{commentID}
//	DELETE /issues/{issueID}/comments/{commentID}
//
// The three comment routes were missing from the first cut of this file
// while its header already claimed "every mutating route". That gap is the
// same shape as the bug the file exists for: Patch LOOKED fixed because it
// answered 404, and a suite that overclaims LOOKS like coverage. A change
// that moved the comment handlers' membership check to AFTER the write
// would have stayed green here. It no longer can.
//
// # Why the assertions are shaped the way they are
//
// A 404 alone does not prove the endpoint is fixed. `Patch` USED to answer
// 404 on exactly this input — by comparing the store's return value AFTER
// PatchIssue had already committed against the foreign project. The response
// hid the breach rather than preventing it. So every negative case asserts
// two things: the status code, and that the row in project B is byte-for-byte
// what it was before the call. The second assertion is the one that would
// have failed on the old code.
//
// # Why 404 and not 403
//
// 403 is an existence oracle: it separates "this id is a real issue somewhere
// in the installation" from "this id is nothing", which is precisely the
// enumeration the attacker wants. 404 leaks nothing, and it is also the
// truthful answer about the resource the caller actually addressed
// (project A's issue X), which does not exist. TestCrossProjectWrite_404NotDistinguishable
// pins that the cross-project answer is byte-identical to the answer for an
// id that was never created.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// ---------------- harness ----------------

// newXPIssuesHandler builds an IssuesHandler over the test pool.
func newXPIssuesHandler(t *testing.T, pool *pgxpool.Pool) *IssuesHandler {
	t.Helper()
	return NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool))
}

// xpRequest builds a request carrying the caller identity plus arbitrary chi
// path params. Unlike withChiURLParams it takes the params as pairs so the
// label-scoped routes ({label}) can be exercised too.
func xpRequest(
	t *testing.T,
	method, ifMatch string,
	body any,
	userID uuid.UUID,
	role string,
	params ...string,
) *http.Request {
	t.Helper()
	if len(params)%2 != 0 {
		t.Fatalf("params must be key/value pairs, got %d", len(params))
	}
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/xp", rdr)
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rctx := chi.NewRouteContext()
	for i := 0; i < len(params); i += 2 {
		rctx.URLParams.Add(params[i], params[i+1])
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	return req.WithContext(ctx)
}

// xpResponse is one handler's answer, kept BOTH decoded and verbatim.
//
// The decoded map is what the ordinary assertions read. Raw + ContentType
// exist for the oracle test: comparing two answers on the "error" key alone
// proves they agree on the code, not that they are the same response —
// writeProblem splices `extra` fields into the body, so a divergence there
// (or in the message, or in the content type) would re-open the discrepancy
// with the decoded comparison still green (CWE-204).
type xpResponse struct {
	Status      int
	ContentType string
	Raw         string
	Body        map[string]any
}

// xpCall runs one handler and returns status + decoded body — the short form
// used by assertions that only look at one field.
func xpCall(t *testing.T, h http.HandlerFunc, req *http.Request) (int, map[string]any) {
	t.Helper()
	res := xpCallFull(t, h, req)
	return res.Status, res.Body
}

// xpCallFull runs one handler and returns its answer whole.
func xpCallFull(t *testing.T, h http.HandlerFunc, req *http.Request) xpResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return xpResponse{
		Status:      res.StatusCode,
		ContentType: res.Header.Get("Content-Type"),
		Raw:         string(raw),
		Body:        parsed,
	}
}

// issueSnapshot is everything the mutating routes under test can change.
// Comparing it before/after a rejected call is what proves the write did not
// happen — as opposed to merely not being reported.
type issueSnapshot struct {
	Version    int
	Title      string
	Status     string
	Pinned     bool
	Labels     []string
	Components []string
	// Comments is one "id|version|deleted|body" line per row, oldest
	// first. Without it a cross-project AddComment that inserted the row
	// and then answered 404 — the exact shape of the original Patch bug,
	// one handler over — would look like a pass.
	Comments []string
}

func snapIssue(t *testing.T, pool *pgxpool.Pool, id string) issueSnapshot {
	t.Helper()
	var s issueSnapshot
	if err := pool.QueryRow(t.Context(), `
SELECT version, title, status, pinned FROM issues WHERE id = $1`, id).
		Scan(&s.Version, &s.Title, &s.Status, &s.Pinned); err != nil {
		t.Fatalf("snapshot issue %s: %v", id, err)
	}
	rows, err := pool.Query(t.Context(),
		`SELECT label FROM issue_labels WHERE issue_id = $1 ORDER BY label`, id)
	if err != nil {
		t.Fatalf("snapshot labels %s: %v", id, err)
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("scan label: %v", err)
		}
		s.Labels = append(s.Labels, l)
	}
	rows.Close()
	crows, err := pool.Query(t.Context(),
		`SELECT component_id::text FROM issue_components WHERE issue_id = $1 ORDER BY component_id`, id)
	if err != nil {
		t.Fatalf("snapshot components %s: %v", id, err)
	}
	for crows.Next() {
		var c string
		if err := crows.Scan(&c); err != nil {
			t.Fatalf("scan component: %v", err)
		}
		s.Components = append(s.Components, c)
	}
	crows.Close()
	mrows, err := pool.Query(t.Context(), `
SELECT id::text, version, (deleted_at IS NOT NULL), body
  FROM comments WHERE issue_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		t.Fatalf("snapshot comments %s: %v", id, err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			cid, body string
			version   int
			deleted   bool
		)
		if err := mrows.Scan(&cid, &version, &deleted, &body); err != nil {
			t.Fatalf("scan comment: %v", err)
		}
		s.Comments = append(s.Comments,
			cid+"|"+strconv.Itoa(version)+"|"+strconv.FormatBool(deleted)+"|"+body)
	}
	return s
}

func equalSnapshots(a, b issueSnapshot) bool {
	if a.Version != b.Version || a.Title != b.Title || a.Status != b.Status || a.Pinned != b.Pinned {
		return false
	}
	if len(a.Labels) != len(b.Labels) || len(a.Components) != len(b.Components) ||
		len(a.Comments) != len(b.Comments) {
		return false
	}
	for i := range a.Comments {
		if a.Comments[i] != b.Comments[i] {
			return false
		}
	}
	for i := range a.Labels {
		if a.Labels[i] != b.Labels[i] {
			return false
		}
	}
	for i := range a.Components {
		if a.Components[i] != b.Components[i] {
			return false
		}
	}
	return true
}

// xpMutation is one mutating endpoint expressed so the same description can
// drive both the legitimate call and the cross-project call.
type xpMutation struct {
	// name is the route, as it reads in the router.
	name string
	// invoke runs the handler. pathProject is the project id that goes in
	// the URL; it is the ONLY thing that differs between the positive and
	// the negative case.
	invoke func(t *testing.T, h *IssuesHandler, pathProject uuid.UUID, issueID string,
		version int, caller uuid.UUID, extra xpFixtures) xpResponse
	// wantOK is the success status of the legitimate call.
	wantOK int
}

// xpFixtures carries per-project scratch objects the endpoints need.
type xpFixtures struct {
	// componentInIssueProject is a component of the issue's OWN project, so
	// a rejected ReplaceComponents cannot be explained away as "the store
	// refused a foreign component" — only the path gate can reject it.
	componentInIssueProject uuid.UUID
	// seededLabel is a label already present on the issue, so RemoveLabel's
	// legitimate call has something to remove.
	seededLabel string
	// seededComment is a comment on the issue, authored by the issue's
	// creator, so UpdateComment's AUTHOR-ONLY gate and DeleteComment's
	// AUTHOR-OR-OWNER gate can be reached by the legitimate call. Its
	// version is the OCC token UpdateComment demands.
	seededComment        uuid.UUID
	seededCommentVersion int
}

func xpMutations() []xpMutation {
	return []xpMutation{
		{
			name:   "PATCH /issues/{issueID}",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, v int,
				caller uuid.UUID, _ xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPatch, strconv.Itoa(v),
					map[string]any{"title": "rewritten by the caller"}, caller, "user",
					"projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.Patch, req)
			},
		},
		{
			name:   "POST /issues/{issueID}/transitions",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, v int,
				caller uuid.UUID, _ xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPost, strconv.Itoa(v),
					// in_progress, not closed: the fixture issue is pinned (so
					// /pin and /unpin both have a real state change to make),
					// and closing a pinned issue legitimately needs ?force=true.
					// The transition itself is what is under test, not which one.
					map[string]any{"status": "in_progress", "reason": "taken by the caller"}, caller, "user",
					"projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.UpdateStatus, req)
			},
		},
		{
			name:   "POST /issues/{issueID}/pin",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, v int,
				caller uuid.UUID, _ xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPost, strconv.Itoa(v), nil, caller, "user",
					"projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.Pin, req)
			},
		},
		{
			name:   "POST /issues/{issueID}/unpin",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, v int,
				caller uuid.UUID, _ xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPost, strconv.Itoa(v), nil, caller, "user",
					"projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.Unpin, req)
			},
		},
		{
			name:   "PUT /issues/{issueID}/components",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, _ int,
				caller uuid.UUID, f xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPut, "",
					map[string]any{"componentIds": []string{f.componentInIssueProject.String()}},
					caller, "user", "projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.ReplaceComponents, req)
			},
		},
		{
			name:   "POST /issues/{issueID}/labels",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, _ int,
				caller uuid.UUID, _ xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPost, "",
					map[string]any{"label": "injected"}, caller, "user",
					"projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.AddLabel, req)
			},
		},
		{
			// POST /comments is the closest sibling of the original Patch
			// bug: it loads the issue, compares projects, then inserts.
			// Only the before/after snapshot distinguishes "checked first"
			// from "inserted, then answered 404".
			name:   "POST /issues/{issueID}/comments",
			wantOK: http.StatusCreated,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, _ int,
				caller uuid.UUID, _ xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPost, "",
					map[string]any{"body": "injected through the wrong project"},
					caller, "user", "projectID", pp.String(), "issueID", id)
				return xpCallFull(t, h.AddComment, req)
			},
		},
		{
			name:   "PUT /issues/{issueID}/comments/{commentID}",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, _ int,
				caller uuid.UUID, f xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodPut, "",
					map[string]any{
						"body":            "rewritten through the wrong project",
						"expectedVersion": f.seededCommentVersion,
					},
					caller, "user", "projectID", pp.String(), "issueID", id,
					"commentID", f.seededComment.String())
				return xpCallFull(t, h.UpdateComment, req)
			},
		},
		{
			name:   "DELETE /issues/{issueID}/comments/{commentID}",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, _ int,
				caller uuid.UUID, f xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodDelete, "", nil, caller, "user",
					"projectID", pp.String(), "issueID", id,
					"commentID", f.seededComment.String())
				return xpCallFull(t, h.DeleteComment, req)
			},
		},
		{
			name:   "DELETE /issues/{issueID}/labels/{label}",
			wantOK: http.StatusOK,
			invoke: func(t *testing.T, h *IssuesHandler, pp uuid.UUID, id string, _ int,
				caller uuid.UUID, f xpFixtures) xpResponse {
				req := xpRequest(t, http.MethodDelete, "", nil, caller, "user",
					"projectID", pp.String(), "issueID", id, "label", f.seededLabel)
				return xpCallFull(t, h.RemoveLabel, req)
			},
		},
	}
}

// seedXPIssue creates one issue in projectID plus the scratch objects the
// mutations need, and returns its id, live version and fixtures.
func seedXPIssue(
	t *testing.T,
	pool *pgxpool.Pool,
	projectID, creator uuid.UUID,
	prefix, suffix string,
) (string, int, xpFixtures) {
	t.Helper()
	id := prefix + "-" + suffix
	seedCmtIssue(t, pool, projectID, creator, id, "human")
	// Pin it so both /pin and /unpin have a real state change available and
	// neither degenerates into a no-op that would pass regardless.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET pinned = true WHERE id = $1`, id); err != nil {
		t.Fatalf("pin seed issue: %v", err)
	}
	label := "seeded-label"
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO issue_labels (issue_id, label) VALUES ($1, $2)`, id, label); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	comp := seedXPComponent(t, pool, projectID, creator)
	commentID, commentVersion := seedXPComment(t, pool, id, creator)
	var version int
	if err := pool.QueryRow(t.Context(),
		`SELECT version FROM issues WHERE id = $1`, id).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return id, version, xpFixtures{
		componentInIssueProject: comp,
		seededLabel:             label,
		seededComment:           commentID,
		seededCommentVersion:    commentVersion,
	}
}

// seedXPComment inserts one comment authored by `author` straight through
// the pool — not through AddComment — so the fixture for the comment routes
// does not depend on the handler under test.
func seedXPComment(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID) (uuid.UUID, int) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(t.Context(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body,
                      created_at, updated_at, version)
VALUES ($1, $2, 'seed', $3, 'human', 'seeded comment', now(), now(), 1)`,
		id, issueID, author); err != nil {
		t.Fatalf("seed comment on %s: %v", issueID, err)
	}
	return id, 1
}

func seedXPComponent(t *testing.T, pool *pgxpool.Pool, projectID, createdBy uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(t.Context(), `
INSERT INTO components (id, project_id, name, description, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', $4, now(), now())`,
		id, projectID, "xp-"+id.String()[:8], createdBy); err != nil {
		t.Fatalf("seed component: %v", err)
	}
	return id
}

// ---------------- tests ----------------

// TestCrossProjectWrite_RejectedOnEveryMutatingRoute is the sk-194.6.50
// regression guard, run once per mutating endpoint.
//
// The caller is a member of BOTH projects. That is deliberate and it is the
// sharpest version of the test: it removes project visibility from the
// picture entirely, so nothing but the object-level gate can be responsible
// for the rejection. A caller who legitimately sees project A still may not
// reach project B's issue through project A's URL.
func TestCrossProjectWrite_RejectedOnEveryMutatingRoute(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, _ := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPIssuesHandler(t, pool)

	for i, m := range xpMutations() {
		t.Run(m.name, func(t *testing.T) {
			// Each subtest gets its own issue in B: the legitimate call
			// mutates it, so sharing one row across subtests would make the
			// later expectations depend on execution order.
			issueID, version, fx := seedXPIssue(t, pool, pidB, user, prefixB, "x"+strconv.Itoa(i))

			t.Run("negative: addressed through project A → 404, nothing written", func(t *testing.T) {
				before := snapIssue(t, pool, issueID)
				res := m.invoke(t, h, pidA, issueID, version, user, fx)
				if res.Status != http.StatusNotFound {
					t.Fatalf("cross-project call = %d, want 404 (body=%s)", res.Status, res.Raw)
				}
				if code, _ := res.Body["error"].(string); code != "issue_not_found" {
					t.Errorf("error code = %q, want issue_not_found (body=%s)", code, res.Raw)
				}
				// The assertion that matters. A handler that mutates first
				// and compares project ids afterwards also answers 404 here
				// — and has already written to project B.
				after := snapIssue(t, pool, issueID)
				if !equalSnapshots(before, after) {
					t.Fatalf("cross-project call answered 404 but MUTATED the issue:\n"+
						"  before = %+v\n  after  = %+v", before, after)
				}
			})

			t.Run("positive: addressed through its own project → success", func(t *testing.T) {
				res := m.invoke(t, h, pidB, issueID, version, user, fx)
				if res.Status != m.wantOK {
					t.Fatalf("same-project call = %d, want %d (body=%s)", res.Status, m.wantOK, res.Raw)
				}
			})
		})
	}
}

// TestCrossProjectWrite_404NotDistinguishableFromNonexistent pins the reason
// the code is 404 rather than 403.
//
// If a cross-project id answered 403 while a made-up id answered 404, the
// pair of responses would be an existence oracle: an attacker walking ids
// through their own project's URL would learn which ones are real issues
// elsewhere in the installation. The two answers must be identical — same
// status, same error code — so the responses carry no information about the
// foreign project at all.
func TestCrossProjectWrite_404NotDistinguishableFromNonexistent(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPIssuesHandler(t, pool)

	realInB, version, fx := seedXPIssue(t, pool, pidB, user, prefixB, "oracle")
	// Same shape as a real id in project A, but never created.
	neverCreated := prefixA + "-99999"

	for _, m := range xpMutations() {
		t.Run(m.name, func(t *testing.T) {
			real := m.invoke(t, h, pidA, realInB, version, user, fx)
			fake := m.invoke(t, h, pidA, neverCreated, version, user, fx)

			if real.Status != fake.Status {
				t.Fatalf("existence oracle: real-but-foreign id → %d, nonexistent id → %d; "+
					"they must be identical or the status code reveals that the "+
					"foreign issue exists", real.Status, fake.Status)
			}
			if real.Status != http.StatusNotFound {
				t.Fatalf("want 404 for both, got %d (body=%s)", real.Status, real.Raw)
			}
			if real.ContentType != fake.ContentType {
				t.Fatalf("existence oracle in Content-Type: %q vs %q",
					real.ContentType, fake.ContentType)
			}
			// Whole body, not just the code: an `extra` field that only one
			// branch sets would leak the same fact.
			if real.Raw != fake.Raw {
				t.Fatalf("existence oracle in the response body:\n"+
					"  foreign     = %s\n  nonexistent = %s", real.Raw, fake.Raw)
			}
		})
	}
}

// TestCrossProjectWrite_NonMemberOfIssueProjectAlsoGets404 covers the
// realistic attacker: someone who is a member of project A only, guessing at
// ids from project B, which they cannot see at all.
//
// Project visibility would already stop them from calling B's URL directly.
// The whole point of the attack is that they call THEIR OWN project's URL,
// where the visibility check passes — so this case is only stopped by the
// object-level gate.
func TestCrossProjectWrite_NonMemberOfIssueProjectAlsoGets404(t *testing.T) {
	pool := newIMTestPool(t)
	attacker, _ := seedIMUser(t, pool, "user")
	victim, _ := seedIMUser(t, pool, "user")
	pidA, _ := seedIMProject(t, pool, attacker) // attacker owns A
	pidB, prefixB := seedIMProject(t, pool, victim)
	h := newXPIssuesHandler(t, pool)

	issueID, version, fx := seedXPIssue(t, pool, pidB, victim, prefixB, "nonmember")

	for _, m := range xpMutations() {
		t.Run(m.name, func(t *testing.T) {
			before := snapIssue(t, pool, issueID)
			res := m.invoke(t, h, pidA, issueID, version, attacker, fx)
			if res.Status != http.StatusNotFound {
				t.Fatalf("non-member cross-project call = %d, want 404 (body=%s)", res.Status, res.Raw)
			}
			if code, _ := res.Body["error"].(string); code != "issue_not_found" {
				t.Errorf("non-member error code = %q, want issue_not_found (body=%s)", code, res.Raw)
			}
			after := snapIssue(t, pool, issueID)
			if !equalSnapshots(before, after) {
				t.Fatalf("non-member cross-project call MUTATED the issue:\n"+
					"  before = %+v\n  after  = %+v", before, after)
			}
		})
	}
}

// ---------------- bulk (ids in the BODY, not the path) ----------------

// callXPBulk drives POST /projects/{projectID}/issues/bulk with the given ids.
func callXPBulk(
	t *testing.T,
	pool *pgxpool.Pool,
	pathProject uuid.UUID,
	caller uuid.UUID,
	action string,
	ids []string,
) (int, bulkResponse) {
	t.Helper()
	bh := NewBulkHandler(pool, issues.NewStore(pool))
	req := xpRequest(t, http.MethodPost, "",
		map[string]any{"ids": ids, "action": action}, caller, "user",
		"projectID", pathProject.String())
	rec := httptest.NewRecorder()
	bh.Bulk(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body bulkResponse
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestBulk_CrossProjectIdsRejectedAndNotMutated covers the one mutating
// surface in this family where the issue id does NOT come from the path.
//
// That distinction is the whole point. For every route above, {issueID} is a
// path segment, so at least the router has looked at it. Bulk takes its ids
// from the request BODY: nothing between the JSON and the UPDATE knows which
// project those ids belong to except runOne's own check. A batch is also the
// cheapest possible enumeration primitive — 100 guessed ids per request, and
// the per-id `failed[].code` is the answer channel — so "foreign" and
// "nonexistent" must be the same code there too.
func TestBulk_CrossProjectIdsRejectedAndNotMutated(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)

	// One issue in each project; the caller is a member of both, so only the
	// object-level check can reject the B ids addressed through A.
	mine, _, _ := seedXPIssue(t, pool, pidA, user, prefixA, "bulkmine")
	foreign, _, _ := seedXPIssue(t, pool, pidB, user, prefixB, "bulkforeign")
	neverCreated := prefixA + "-99999"

	for _, action := range []string{BulkActionClose, BulkActionPin, BulkActionUnpin, BulkActionArchive} {
		t.Run(action, func(t *testing.T) {
			before := snapIssue(t, pool, foreign)

			status, body := callXPBulk(t, pool, pidA, user, action, []string{foreign, neverCreated})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("bulk with only foreign/nonexistent ids = %d, want 422 (body=%+v)", status, body)
			}
			if len(body.Succeeded) != 0 {
				t.Fatalf("bulk reported %v as succeeded through the wrong project", body.Succeeded)
			}
			codes := map[string]string{}
			for _, f := range body.Failed {
				codes[f.ID] = f.Code
			}
			if codes[foreign] != "not_found" {
				t.Errorf("foreign id code = %q, want not_found (failed=%+v)", codes[foreign], body.Failed)
			}
			// The anti-oracle equality, per id inside one response.
			if codes[foreign] != codes[neverCreated] {
				t.Fatalf("existence oracle inside one bulk response: foreign id → %q, "+
					"nonexistent id → %q; they must be identical",
					codes[foreign], codes[neverCreated])
			}
			after := snapIssue(t, pool, foreign)
			if !equalSnapshots(before, after) {
				t.Fatalf("bulk %s reported not_found but MUTATED the foreign issue:\n"+
					"  before = %+v\n  after  = %+v", action, before, after)
			}
		})
	}

	// Positive control: the same batch shape through the id's own project
	// still works, so the gate is not simply refusing everything.
	status, body := callXPBulk(t, pool, pidA, user, BulkActionUnpin, []string{mine})
	if status != http.StatusOK || len(body.Succeeded) != 1 {
		t.Fatalf("same-project bulk = %d succeeded=%v failed=%+v, want 200 with 1 success",
			status, body.Succeeded, body.Failed)
	}
}

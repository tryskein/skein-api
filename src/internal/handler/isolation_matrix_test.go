package handler

// isolation_matrix_test.go — sk-674.18 T004: the "actor × surface" isolation
// matrix over REST, on the CURRENT access semantics (matrix rows 22, 23,
// 23a, 24, 24a, 25, 26, 27, 28, 29, 30; the MCP half and the REST↔MCP
// parity of row 31 live in cmd/api/isolation_mcp_parity_test.go).
//
// Fixture: testsupport.SeedTenancy (T003, O-f1) — two tenants A/B with a
// `space` and a `members` project each, a "Personal" per non-external actor,
// every actor of the matrix legend. The test adds content on top of it:
// a comment and an attachment on every marker issue, an epic in A.space with
// a visible child (A.space) and a closed child plus a blocker in A.members,
// a unique search word in every title / comment / project description, and
// one embedding on every fixture row for the semantic search mode.
//
// # Reference
//
// The expected visibility of every fixture project for every actor is the
// literal table testsupport.TenancyVisible, written from the legend of row
// 22, not from the code:
//
//	uA, vP, vPr           — A.space, A.members, own Pers
//	uB                    — B.space, B.members, own Pers
//	uAB                   — all four tenant projects, own Pers
//	uc¬p                  — A.space, own Pers (not A.members)
//	vnsh                   — A.members only (no Pers: INV-8)
//	admA, adm0 (f+)       — all non-personal, own Pers; Pers of others — N
//	deakt                 — nothing
//	post                  — own Pers only
//
// A project outside the actor's set is N: no row, number or text of it in
// any answer, and a direct request for it answers exactly as for a
// nonexistent id (INV-3: status, `error` code and key set of the body).
//
// # Cells waiting for narrowing
//
// Some cells are known holes of the current code (authz research §7, plan
// K-16/K-3) and are switched on by the task that narrows the surface. They
// run as subtests that skip with "waiting for narrowing: T0NN", so the narrowing
// task finds the cell by name and turns the skip into the assertion:
//
//   -
//     T030; search cursor — T031; themes — T035 (row 29);
//   - INV-3 for theme ids — T035 (invitation ids: T040,
//     TestIsolationMatrix_InvitationIDs; space ids: T023,
//     TestIsolationMatrix_SpaceIDs).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/attachments"
	"skein-api/internal/auth"
	"skein-api/internal/dashboard"
	"skein-api/internal/dependencies"
	"skein-api/internal/invitations"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/search"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// ---------------------------------------------------------------------------
// Reference table
// ---------------------------------------------------------------------------

// Project keys of the fixture: the four tenant projects and "L:<actor>".
const (
	keyASpace   = "A.space"
	keyAMembers = "A.members"
	keyBSpace   = "B.space"
	keyBMembers = "B.members"
)

func personalKey(a testsupport.Actor) string { return "L:" + string(a) }

type cellState int

const (
	cellVisible cellState = iota
	cellHidden
)

// isoCell is the expectation of one (actor, project) cell.
func isoCell(a testsupport.Actor, key string) cellState {
	if slices.Contains(testsupport.TenancyVisible[a], key) {
		return cellVisible
	}
	return cellHidden
}

// ---------------------------------------------------------------------------
// World: fixture + content + router
// ---------------------------------------------------------------------------

type isoProject struct {
	testsupport.TenancyProject
	Key          string
	CommentID    uuid.UUID
	CommentBody  string
	AttachmentID uuid.UUID
	ComponentID  uuid.UUID
	// IssueIDs — every issue the matrix seeded in this project, marker
	// included.
	IssueIDs []string
}

type isoWorld struct {
	t       *testing.T
	pool    *pgxpool.Pool
	f       *testsupport.Tenancy
	router  chi.Router
	word    string // unique search word of this run
	byKey   map[string]*isoProject
	ordered []*isoProject

	// The epic in A.space and its relatives.
	epicID         string // A.space marker, type epic
	visibleChildID string // open, A.space
	hiddenChildID  string // closed, A.members
	blockerID      string // open, A.members; epic blocks on it
}

const isoEmbeddingDim = 1024

// isoUnitVector is the one embedding every fixture row carries and the stub
// embedder returns for every query: cosine distance 0 to every seeded row,
// so the semantic branch of search finds all of them and only the access
// predicate can keep a row out.
func isoUnitVector() []float32 {
	v := make([]float32, isoEmbeddingDim)
	v[0] = 1
	return v
}

func isoVectorLiteral() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range isoUnitVector() {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", x)
	}
	b.WriteByte(']')
	return b.String()
}

// isoS3 is the object-store boundary stub: the matrix asserts who gets a
// download link, not what S3 does with it.
type isoS3 struct{}

func (isoS3) PresignedPostPolicy(context.Context, string, string, int64, time.Duration) (attachments.PostPolicy, error) {
	return attachments.PostPolicy{}, nil
}
func (isoS3) PresignedGetURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://s3.isomatrix.test/" + key, nil
}
func (isoS3) RemoveObject(context.Context, string) error { return nil }
func (isoS3) StatObject(context.Context, string) error   { return nil }
func (isoS3) ListKeys(context.Context, string, func(string) error) error {
	return nil
}

// newOllamaStub answers POST /api/embed with isoUnitVector for every input.
func newOllamaStub(t *testing.T) *search.Embedder {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{}
		for range req.Input {
			out.Embeddings = append(out.Embeddings, isoUnitVector())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	e, err := search.NewEmbedder(search.EmbedConfig{BaseURL: srv.URL, Model: "isomatrix", Dim: isoEmbeddingDim})
	if err != nil {
		t.Fatalf("stub embedder: %v", err)
	}
	return e
}

func newIsoWorld(t *testing.T) *isoWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	f := testsupport.SeedTenancy(t, pool)

	w := &isoWorld{
		t: t, pool: pool, f: f,
		word:  "isoword" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""),
		byKey: map[string]*isoProject{},
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	add := func(key string, p testsupport.TenancyProject) {
		ip := &isoProject{TenancyProject: p, Key: key, IssueIDs: []string{p.IssueID}}
		w.byKey[key] = ip
		w.ordered = append(w.ordered, ip)
	}
	add(keyASpace, f.ASpace)
	add(keyAMembers, f.AMembers)
	add(keyBSpace, f.BSpace)
	add(keyBMembers, f.BMembers)
	for _, a := range testsupport.TenancyActors {
		if p, ok := f.Personal[a]; ok {
			add(personalKey(a), p)
		}
	}

	vec := isoVectorLiteral()
	for _, p := range w.ordered {
		exec(`UPDATE projects SET description = $2, embedding = $3::halfvec WHERE id = $1`,
			p.ID, "isomatrix project "+w.word, vec)
		exec(`UPDATE issues SET title = title || ' ' || $2, embedding = $3::halfvec WHERE id = $1`,
			p.IssueID, w.word, vec)
		p.CommentBody = "isomatrix comment " + w.word + " " + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
		p.CommentID = testsupport.SeedComment(t, pool, p.IssueID, p.OwnerID, p.CommentBody)
		exec(`UPDATE comments SET embedding = $2::halfvec WHERE id = $1`, p.CommentID, vec)
		p.AttachmentID = testsupport.SeedAttachment(t, pool, p.IssueID, p.OwnerID, "ready")
		p.ComponentID = testsupport.SeedComponent(t, pool, p.ID, p.OwnerID, "isocomp", "isomatrix component "+w.word)
		exec(`UPDATE components SET embedding = $2::halfvec WHERE id = $1`, p.ComponentID, vec)
	}

	// The epic and its relatives: a visible child in A.space, a CLOSED child
	// and an open blocker in A.members. For uc¬p the closed child and the
	// blocker are N, so neither may reach a visible number or the body.
	aSpace, aMembers := w.byKey[keyASpace], w.byKey[keyAMembers]
	w.epicID = aSpace.IssueID
	exec(`UPDATE issues SET type = 'epic' WHERE id = $1`, w.epicID)
	w.visibleChildID = testsupport.SeedIssue(t, pool, aSpace.ID, aSpace.OwnerID,
		aSpace.IDPrefix+"-2", "isomatrix visible child "+w.word, "open", 2)
	aSpace.IssueIDs = append(aSpace.IssueIDs, w.visibleChildID)
	w.hiddenChildID = testsupport.SeedIssue(t, pool, aMembers.ID, aMembers.OwnerID,
		aMembers.IDPrefix+"-2", "isomatrix hidden child "+w.word, "closed", 2)
	w.blockerID = testsupport.SeedIssue(t, pool, aMembers.ID, aMembers.OwnerID,
		aMembers.IDPrefix+"-3", "isomatrix blocker "+w.word, "open", 2)
	aMembers.IssueIDs = append(aMembers.IssueIDs, w.hiddenChildID, w.blockerID)
	testsupport.SeedDep(t, pool, w.visibleChildID, w.epicID, "parent-child", aSpace.OwnerID)
	testsupport.SeedDep(t, pool, w.hiddenChildID, w.epicID, "parent-child", aMembers.OwnerID)
	testsupport.SeedDep(t, pool, w.epicID, w.blockerID, "blocks", aSpace.OwnerID)
	exec(`UPDATE issues SET embedding = $2::halfvec WHERE id = ANY($1)`,
		[]string{w.visibleChildID, w.hiddenChildID, w.blockerID}, vec)

	// History: one status-change event on every marker, so a visible
	// activity feed has a known entry to show.
	var projectIDs []uuid.UUID
	for _, p := range w.ordered {
		projectIDs = append(projectIDs, p.ID)
		w.seedIssueEvent(t, p)
	}
	// Registered after the fixture: the events (actor_user_id is ON DELETE
	// RESTRICT) leave before the users.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = ANY($1)`, projectIDs)
	})

	w.router = w.buildRouter()
	return w
}

// seedIssueEvent writes one issue.status_changed event on p's marker by its
// owner, now. Returns the event id.
func (w *isoWorld) seedIssueEvent(t *testing.T, p *isoProject) int64 {
	t.Helper()
	var id int64
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES (now(), 'issue.status_changed', 'isomatrix', $1, 'human', 'issue', $2, $3, '{}'::jsonb)
RETURNING id`, p.OwnerID, p.IssueID, p.ID).Scan(&id); err != nil {
		t.Fatalf("seed event on %s: %v", p.IssueID, err)
	}
	return id
}

// buildRouter mounts the production handlers on the production paths of the
// surfaces under test (cmd/api/main.go buildRouter), behind the same user
// gate the BFF channel passes: ParseUserHeaders + RequireUser.
func (w *isoWorld) buildRouter() chi.Router {
	pool := w.pool
	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	searchSvc, err := search.NewService(search.NewStore(pool), nil, 0)
	if err != nil {
		w.t.Fatalf("search service: %v", err)
	}
	semanticSvc, err := search.NewService(search.NewStore(pool), newOllamaStub(w.t), 0)
	if err != nil {
		w.t.Fatalf("semantic search service: %v", err)
	}

	projectsH := NewProjectsHandler(pool, nil)
	issuesH := NewIssuesHandler(pool, projectsStore, depsStore)
	depsH := NewDependenciesHandler(pool, depsStore, issuesStore)
	readyH := NewReadyHandler(pool, depsStore)
	issuesAllH := NewIssuesAllHandler(pool, issuesStore, nil)
	issuesMeH := NewIssuesMeHandler(pool, issuesStore)
	activityH := NewActivityHandler(pool, issuesStore)
	attachmentsH := NewAttachmentsHandler(pool, isoS3{}, "skein-attachments", 1<<20, 60)
	usersH := NewUsersHandler(pool, users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil), projectsStore)
	meH := NewMeHandler(pool, issuesStore, notifications.NewStore(pool))
	notifH := NewNotificationsHandler(pool)
	dashH := NewDashboardHandler(pool, dashboard.New(pool), issuesStore)
	remindersH := NewRemindersHandler(pool, reminders.NewStore(pool), issuesStore, projectsStore)
	searchH := NewSearchHandler(searchSvc)
	semanticH := NewSearchHandler(semanticSvc)
	pollH := NewAgentPollHandler(pool, 0)
	spacesSvc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})
	spacesH := NewSpacesHandler(spacesSvc)
	invitationsH := NewInvitationsHandler(invitations.NewService(pool, spacesSvc,
		accounts.NewTurnstile("", accounts.TurnstileVerifyURL), invitations.Options{InviteTTL: 24 * time.Hour}))
	themesH := NewThemesHandler(pool)
	meThemeH := NewMeThemeHandler(pool, nil)

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders)
		r.Use(RequireUser)
		r.Get("/search", searchH.Search)
		r.Get("/search/lookup-id", searchH.LookupID)
		r.Post("/search/lookup-id-batch", searchH.LookupIDBatch)
		// The semantic mode is the same handler over a service with an
		// embedder; production has one route and picks the mode by the
		// embedder's availability.
		r.Get("/search-semantic", semanticH.Search)
		r.Get("/issues", issuesAllH.List)
		r.Get("/issues/tree", issuesAllH.Tree)
		r.Get("/issues/{issueID}/attachments", attachmentsH.List)
		r.Get("/issues/{issueID}/watchers", notifH.ListIssueWatchers)
		r.Get("/attachments/{id}/download-url", attachmentsH.DownloadURL)
		r.Get("/me/memberships", usersH.GetMyMemberships)
		r.Get("/users/directory", usersH.ListDirectory)
		r.Get("/users/profile/{username}", usersH.GetUserProfile)
		r.Get("/me/created-by", issuesMeH.CreatedBy)
		r.Get("/me/assigned-to", issuesMeH.AssignedTo)
		r.Get("/me/notification-counts", meH.NotificationCounts)
		r.Get("/me/watching", meH.GetWatching)
		r.Get("/notifications", notifH.ListNotifications)
		r.Get("/reminders", remindersH.List)
		r.Get("/dashboard/scoring", dashH.Scoring)
		r.Get("/dashboard/feed", dashH.Feed)
		r.Get("/dashboard/in-progress", dashH.InProgress)
		r.Get("/dashboard/race", dashH.Race)
		r.Get("/dashboard/projects", dashH.Projects)
		r.Get("/dashboard/agents", dashH.Agents)
		r.Get("/spaces/{id}", spacesH.Get)
		r.Post("/spaces/{id}/invitations/{invId}/resend", invitationsH.Resend)
		r.Delete("/spaces/{id}/invitations/{invId}", invitationsH.Revoke)
		r.Post("/me/invitations/{invId}/accept", invitationsH.AcceptMine)
		r.Post("/me/invitations/{invId}/decline", invitationsH.DeclineMine)
		r.Get("/themes", themesH.List)
		r.Post("/themes", themesH.Create)
		r.Get("/themes/{id}", themesH.Get)
		r.Patch("/themes/{id}", themesH.Patch)
		r.Delete("/themes/{id}", themesH.Delete)
		r.Put("/me/theme", meThemeH.SetTheme)
		r.Route("/projects", func(r chi.Router) {
			r.Get("/", projectsH.List)
			r.Get("/resolve", projectsH.Resolve)
			r.Route("/{projectID}", func(r chi.Router) {
				r.Get("/", projectsH.Get)
				r.Get("/members", projectsH.ListMembers)
				r.Get("/ready", readyH.Ready)
				r.Get("/activity", activityH.ListProjectActivity)
				r.Get("/issues/tree", issuesH.Tree)
				r.Route("/issues/{issueID}", func(r chi.Router) {
					r.Get("/", issuesH.Get)
					r.Patch("/", issuesH.Patch)
					r.Post("/review", issuesH.MarkReviewed)
					r.Get("/comments", issuesH.ListComments)
					r.Put("/comments/{commentID}", issuesH.UpdateComment)
					r.Delete("/comments/{commentID}", issuesH.DeleteComment)
					r.Get("/dependencies", depsH.List)
					r.Get("/activity", activityH.ListIssueActivity)
				})
			})
		})
	})
	r.With(auth.RequireMonitorToken(monitorcreds.New(pool, time.Hour), "notifications:poll")).
		Get("/agent/v1/notifications/poll", pollH.Poll)
	return r
}

type isoResp struct {
	status int
	body   []byte
}

func (w *isoWorld) do(a testsupport.Actor, method, path string, body any) isoResp {
	w.t.Helper()
	u := w.f.User(w.t, a)
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.HeaderUserID, u.ID.String())
	req.Header.Set(auth.HeaderUsername, u.Username)
	req.Header.Set(auth.HeaderUserRole, u.Role)
	req.Header.Set(auth.HeaderUserIsExternal, fmt.Sprint(u.External))
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	return isoResp{status: rec.Code, body: rec.Body.Bytes()}
}

func (w *isoWorld) get(a testsupport.Actor, path string) isoResp {
	return w.do(a, http.MethodGet, path, nil)
}

// tokens are the strings that identify a project's data in any answer. Ids
// of issues are matched quoted, so "<prefix>-1" does not match "<prefix>-10".
func (p *isoProject) tokens() []string {
	out := []string{
		p.ID.String(), p.Name, p.Alias, p.CommentBody,
		p.CommentID.String(), p.AttachmentID.String(), p.ComponentID.String(),
	}
	for _, id := range p.IssueIDs {
		out = append(out, `"`+id+`"`)
	}
	return out
}

// assertNoLeak fails when the answer to actor a carries any token of a
// project that is N for a. Pending cells are not checked here — their task
// switches them on.
func (w *isoWorld) assertNoLeak(t *testing.T, a testsupport.Actor, surface string, body []byte) {
	t.Helper()
	w.assertNoLeakExcept(t, a, surface, body, nil)
}

// assertNoLeakExcept is assertNoLeak with known holes: a token in `known`
// is a cell waiting for narrowing (declared by name in the test that passes
// it) and is not reported here; every other token still is.
//
// A pending cell is not checked here, but it is not dropped silently either:
// every pending project of the actor becomes a named subtest
// "pending/<project>" that skips with its task, so the aggregate surface
// shows each of its unchecked cells.
func (w *isoWorld) assertNoLeakExcept(t *testing.T, a testsupport.Actor, surface string, body []byte, known []string) {
	t.Helper()
	for _, p := range w.ordered {
		if isoCell(a, p.Key) != cellHidden {
			continue
		}
		for _, tok := range p.tokens() {
			if slices.Contains(known, tok) {
				continue
			}
			if i := bytes.Index(body, []byte(tok)); i >= 0 {
				lo, hi := max(0, i-160), min(len(body), i+len(tok)+80)
				t.Errorf("%s: answer to %s carries %q of N project %s: …%s…",
					surface, a, tok, p.Key, body[lo:hi])
			}
		}
	}
}

// problemShape is what INV-3 compares: status, machine code and key set.
type problemShape struct {
	Status int
	Code   string
	Keys   string
}

func shapeOf(t *testing.T, r isoResp) problemShape {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode problem %s: %v", r.body, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	code, _ := m["error"].(string)
	return problemShape{Status: r.status, Code: code, Keys: strings.Join(keys, ",")}
}

// assertSameAsMissing is INV-3: the answer for an inaccessible object equals
// the answer for a nonexistent one.
func assertSameAsMissing(t *testing.T, what string, hidden, missing isoResp) {
	t.Helper()
	if hidden.status < 400 {
		t.Errorf("%s: inaccessible object answered %d, want a refusal; body %s", what, hidden.status, hidden.body)
		return
	}
	if got, want := shapeOf(t, hidden), shapeOf(t, missing); got != want {
		t.Errorf("%s: inaccessible %+v differs from nonexistent %+v", what, got, want)
	}
}

// assertSameAnswer is INV-3 for a surface whose answer to "nothing found" is
// a success (resolve: 200 with match null): the status, the top-level key set
// and which keys are null must equal the nonexistent query's. Content of
// non-null keys may legitimately differ (fuzzy candidates among visible
// projects); that no N token is in it is assertNoLeak's job. A refusal falls
// back to assertSameAsMissing.
func assertSameAnswer(t *testing.T, what string, hidden, missing isoResp) {
	t.Helper()
	if missing.status >= 400 {
		assertSameAsMissing(t, what, hidden, missing)
		return
	}
	if hidden.status != missing.status {
		t.Errorf("%s: status %d, nonexistent %d", what, hidden.status, missing.status)
		return
	}
	nullness := func(r isoResp) string {
		var m map[string]any
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatalf("%s: decode %s: %v", what, r.body, err)
		}
		keys := make([]string, 0, len(m))
		for k, v := range m {
			keys = append(keys, fmt.Sprintf("%s:null=%v", k, v == nil))
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	if got, want := nullness(hidden), nullness(missing); got != want {
		t.Errorf("%s: answer shape %s differs from nonexistent %s", what, got, want)
	}
}

func (w *isoWorld) eachActor(t *testing.T, fn func(t *testing.T, a testsupport.Actor)) {
	for _, a := range testsupport.TenancyActors {
		t.Run(string(a), func(t *testing.T) { fn(t, a) })
	}
}

// eachCell runs fn for every fixture project of actor a.
func (w *isoWorld) eachCell(t *testing.T, a testsupport.Actor, fn func(t *testing.T, p *isoProject, st cellState)) {
	for _, p := range w.ordered {
		t.Run(p.Key, func(t *testing.T) {
			fn(t, p, isoCell(a, p.Key))
		})
	}
}

func decodeInto(t *testing.T, r isoResp, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func missingUUID() string { return uuid.NewString() }

// ---------------------------------------------------------------------------
// Row 22 — project: list, card, resolve
// ---------------------------------------------------------------------------

func TestIsolationMatrix_Project(t *testing.T) {
	w := newIsoWorld(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		// List: follow the cursor to the end — an admin's list spans the
		// whole database.
		listed := map[string]bool{}
		cursor := ""
		for page := 0; ; page++ {
			if page > 200 {
				t.Fatalf("project list did not end after 200 pages")
			}
			path := "/api/v1/projects?limit=100"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			r := w.get(a, path)
			if r.status != http.StatusOK {
				t.Fatalf("GET projects: %d %s", r.status, r.body)
			}
			w.assertNoLeak(t, a, "GET /projects", r.body)
			var res struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				NextCursor string `json:"nextCursor"`
				HasMore    bool   `json:"hasMore"`
			}
			decodeInto(t, r, &res)
			for _, it := range res.Items {
				listed[it.ID] = true
			}
			if !res.HasMore {
				break
			}
			cursor = res.NextCursor
		}

		missingCard := w.get(a, "/api/v1/projects/"+missingUUID()+"/")
		missingResolve := w.get(a, "/api/v1/projects/resolve?q="+url.QueryEscape("isomissing"+w.word))

		w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
			card := w.get(a, "/api/v1/projects/"+p.ID.String()+"/")
			if st == cellVisible {
				if !listed[p.ID.String()] {
					t.Errorf("visible project missing from GET /projects")
				}
				var c struct {
					ID string `json:"id"`
				}
				decodeInto(t, card, &c)
				if card.status != http.StatusOK || c.ID != p.ID.String() {
					t.Errorf("card: %d id %q, want 200 and %s", card.status, c.ID, p.ID)
				}
			} else {
				assertSameAsMissing(t, "project card", card, missingCard)
			}

			for via, q := range map[string]string{"name": p.Name, "prefix": p.IDPrefix, "alias": p.Alias} {
				r := w.get(a, "/api/v1/projects/resolve?q="+url.QueryEscape(q))
				var res struct {
					Match *struct {
						ID string `json:"id"`
					} `json:"match"`
				}
				decodeInto(t, r, &res)
				if st == cellVisible {
					if r.status != http.StatusOK || res.Match == nil || res.Match.ID != p.ID.String() {
						t.Errorf("resolve by %s: %d %s, want the project", via, r.status, r.body)
					}
					continue
				}
				assertSameAnswer(t, "resolve by "+via, r, missingResolve)
				if res.Match != nil && res.Match.ID == p.ID.String() {
					t.Errorf("resolve by %s matched an N project", via)
				}
				w.assertNoLeak(t, a, "resolve by "+via, r.body)
			}
		})
	})

	// adm0 with SKEIN_INSTANCE_ADMIN_ENABLED=false (instance_settings, K-2):
	// the admin branch of the relation is off, he is an ordinary user — only
	// his own "Personal" (INV-1 d, D-2).
	t.Run("admin0/flag-off", func(t *testing.T) {
		isoSetAdminFlag(t, w.pool, false)
		a := testsupport.ActorAdmin0
		missingCard := w.get(a, "/api/v1/projects/"+missingUUID()+"/")
		for _, p := range w.ordered {
			t.Run(p.Key, func(t *testing.T) {
				card := w.get(a, "/api/v1/projects/"+p.ID.String()+"/")
				if p.Key == personalKey(a) {
					if card.status != http.StatusOK {
						t.Errorf("own Personal card: %d %s, want 200", card.status, card.body)
					}
					return
				}
				assertSameAsMissing(t, "project card with the admin flag off", card, missingCard)
			})
		}
		list := w.get(a, "/api/v1/projects?limit=100")
		if list.status != http.StatusOK {
			t.Fatalf("GET projects: %d %s", list.status, list.body)
		}
		for _, p := range w.ordered {
			if p.Key == personalKey(a) {
				continue
			}
			for _, tok := range p.tokens() {
				if bytes.Contains(list.body, []byte(tok)) {
					t.Errorf("GET /projects with the admin flag off carries %q of %s", tok, p.Key)
				}
			}
		}
	})
}

// isoSetAdminFlag writes instance_settings.admin_enabled for the rest of the
// test and restores true (the value every other test runs with) after it.
// Tests of this package run one at a time here (no t.Parallel in this
// file) and packages under -p 1, so no other test reads the row meanwhile.
func isoSetAdminFlag(t *testing.T, pool *pgxpool.Pool, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE instance_settings SET admin_enabled = $1 WHERE id = 1`, enabled); err != nil {
		t.Fatalf("set the admin flag: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE instance_settings SET admin_enabled = true WHERE id = 1`); err != nil {
			t.Errorf("restore the admin flag: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Row 23 — issue: card, /issues, trees, child counters, deps, blockers, ready
// ---------------------------------------------------------------------------

// isoEpicCounts — the visible numbers of the epic per actor, from the seeded
// shape: a visible open child in A.space and a closed child in A.members.
type isoEpicCounts struct {
	open, closed int
	// blockers — the blocker ids the card may list.
	blockers []string
}

func (w *isoWorld) expectedEpic(a testsupport.Actor) isoEpicCounts {
	if st := isoCell(a, keyAMembers); st == cellVisible {
		return isoEpicCounts{open: 1, closed: 1, blockers: []string{w.blockerID}}
	}
	return isoEpicCounts{open: 1, closed: 0, blockers: nil}
}

func TestIsolationMatrix_Issue(t *testing.T) {
	w := newIsoWorld(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		// /issues across projects — paginated to the end.
		seen := map[string]json.RawMessage{}
		cursor := ""
		for page := 0; ; page++ {
			if page > 200 {
				t.Fatalf("/issues did not end after 200 pages")
			}
			path := "/api/v1/issues?q=" + url.QueryEscape(w.word)
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			r := w.get(a, path)
			if r.status != http.StatusOK {
				t.Fatalf("GET /issues: %d %s", r.status, r.body)
			}
			w.assertNoLeak(t, a, "GET /issues", r.body)
			var res struct {
				Items      []json.RawMessage `json:"items"`
				NextCursor string            `json:"nextCursor"`
				HasMore    bool              `json:"hasMore"`
			}
			decodeInto(t, r, &res)
			for _, raw := range res.Items {
				var row struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					t.Fatalf("decode /issues row %s: %v", raw, err)
				}
				seen[row.ID] = raw
			}
			if !res.HasMore {
				break
			}
			cursor = res.NextCursor
		}

		tree := w.get(a, "/api/v1/issues/tree")
		if tree.status != http.StatusOK {
			t.Fatalf("GET /issues/tree: %d %s", tree.status, tree.body)
		}
		w.assertNoLeak(t, a, "GET /issues/tree", tree.body)

		missingProjectCard := w.get(a, "/api/v1/projects/"+missingUUID()+"/issues/"+w.epicID+"/")

		w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
			base := "/api/v1/projects/" + p.ID.String()
			card := w.get(a, base+"/issues/"+p.IssueID+"/")
			if st != cellVisible {
				assertSameAsMissing(t, "issue card via its project", card, missingProjectCard)
				for _, sub := range []string{"/issues/tree", "/ready"} {
					assertSameAsMissing(t, sub, w.get(a, base+sub),
						w.get(a, "/api/v1/projects/"+missingUUID()+sub))
				}
				return
			}
			if card.status != http.StatusOK {
				t.Fatalf("card: %d %s", card.status, card.body)
			}
			w.assertNoLeak(t, a, "issue card", card.body)
			if _, ok := seen[p.IssueID]; !ok {
				t.Errorf("visible marker %s missing from /issues", p.IssueID)
			}
			if !bytes.Contains(tree.body, []byte(`"`+p.IssueID+`"`)) {
				t.Errorf("visible marker %s missing from /issues/tree", p.IssueID)
			}
			for _, sub := range []string{"/issues/tree", "/ready", "/issues/" + p.IssueID + "/dependencies"} {
				r := w.get(a, base+sub)
				if r.status != http.StatusOK {
					t.Errorf("%s: %d %s", sub, r.status, r.body)
				}
				w.assertNoLeak(t, a, sub, r.body)
			}

			// Positive controls. Ready: the open unblocked issue of the
			// project is listed — in A.space that is the visible child (the
			// epic waits on its blocker), elsewhere the marker.
			wantReady := p.IssueID
			if p.Key == keyASpace {
				wantReady = w.visibleChildID
			}
			var ready []struct {
				ID string `json:"id"`
			}
			decodeInto(t, w.get(a, base+"/ready"), &ready)
			if !slices.ContainsFunc(ready, func(r struct {
				ID string `json:"id"`
			}) bool {
				return r.ID == wantReady
			}) {
				t.Errorf("ready: %s missing", wantReady)
			}
			if p.Key == keyASpace {
				// Dependencies of the epic: the visible child's parent-child
				// edge always; the blocker edge only to those who see it.
				var deps struct {
					Outgoing []struct {
						ToID string `json:"toId"`
						Type string `json:"type"`
					} `json:"outgoing"`
					Incoming []struct {
						FromID string `json:"fromId"`
						Type   string `json:"type"`
					} `json:"incoming"`
				}
				decodeInto(t, w.get(a, base+"/issues/"+w.epicID+"/dependencies"), &deps)
				child := false
				for _, e := range deps.Incoming {
					child = child || (e.FromID == w.visibleChildID && e.Type == "parent-child")
				}
				if !child {
					t.Errorf("dependencies: the visible child's parent-child edge is missing")
				}
				if mb := isoCell(a, keyAMembers); mb == cellVisible {
					blocker := false
					for _, e := range deps.Outgoing {
						blocker = blocker || (e.ToID == w.blockerID && e.Type == "blocks")
					}
					if !blocker {
						t.Errorf("dependencies: the visible blocker edge is missing")
					}
				}
			}

			// Every N issue addressed through a project the actor sees
			// answers as a nonexistent issue of that project.
			missingHere := w.get(a, base+"/issues/"+p.IDPrefix+"-9999/")
			for _, q := range w.ordered {
				if qs := isoCell(a, q.Key); qs != cellHidden {
					continue
				}
				for _, id := range q.IssueIDs {
					assertSameAsMissing(t, "N issue "+id+" via a visible project",
						w.get(a, base+"/issues/"+id+"/"), missingHere)
				}
			}

			if p.Key != keyASpace {
				return
			}
			// The epic: visible numbers count visible children only.
			want := w.expectedEpic(a)
			var c struct {
				Blockers []struct {
					ID string `json:"id"`
				} `json:"blockers"`
				Epic *struct {
					Open   int `json:"openChildren"`
					Closed int `json:"closedChildren"`
				} `json:"epic"`
			}
			decodeInto(t, card, &c)
			if c.Epic == nil {
				t.Fatalf("epic card has no rollup: %s", card.body)
			}
			if c.Epic.Open != want.open || c.Epic.Closed != want.closed {
				t.Errorf("epic children open/closed = %d/%d, want %d/%d",
					c.Epic.Open, c.Epic.Closed, want.open, want.closed)
			}
			var gotBlockers []string
			for _, b := range c.Blockers {
				gotBlockers = append(gotBlockers, b.ID)
			}
			if !slices.Equal(gotBlockers, want.blockers) {
				t.Errorf("blockers = %v, want %v", gotBlockers, want.blockers)
			}
			var row struct {
				EpicProgress *struct {
					Closed      int `json:"closed"`
					Total       int `json:"total"`
					Unavailable int `json:"unavailable"`
				} `json:"epicProgress"`
			}
			raw, ok := seen[w.epicID]
			if !ok {
				t.Fatalf("epic %s missing from /issues", w.epicID)
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatalf("decode epic row %s: %v", raw, err)
			}
			if row.EpicProgress == nil {
				t.Fatalf("epic row in /issues has no epicProgress")
			}
			if row.EpicProgress.Closed != want.closed ||
				row.EpicProgress.Total-row.EpicProgress.Unavailable != want.open+want.closed {
				t.Errorf("epicProgress %+v: visible closed/total want %d/%d",
					*row.EpicProgress, want.closed, want.open+want.closed)
			}
		})
	})

	// parentIdCurrent: a child's card and its /issues row name the parent
	// only to a reader who sees the parent (sk-674.18 T009, decision of
	// 2026-10-12: the tree stub, the redacted edges and this field carried
	// the id of an N issue; the three are asserted by the leak scans above
	// and here). Children of the A.space epic: one in A.space, one in
	// A.members — so uc¬p and vnsh each see a child without its parent, and
	// everyone who sees both is the positive control.
	t.Run("parentIdCurrent", func(t *testing.T) {
		w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
			epicState := isoCell(a, keyASpace)
			for _, child := range []struct {
				id      string
				project string
			}{
				{w.visibleChildID, keyASpace},
				{w.hiddenChildID, keyAMembers},
			} {
				if st := isoCell(a, child.project); st != cellVisible {
					continue
				}
				t.Run(child.id, func(t *testing.T) {
					var want *string
					if epicState == cellVisible {
						want = &w.epicID
					}
					card := w.get(a, "/api/v1/projects/"+w.byKey[child.project].ID.String()+"/issues/"+child.id+"/")
					if card.status != http.StatusOK {
						t.Fatalf("card: %d %s", card.status, card.body)
					}
					w.assertNoLeak(t, a, "child card", card.body)
					var got struct {
						ParentID *string `json:"parentIdCurrent"`
					}
					decodeInto(t, card, &got)
					if !equalPtr(got.ParentID, want) {
						t.Errorf("card parentIdCurrent = %v, want %v", deref(got.ParentID), deref(want))
					}
					rows := w.get(a, "/api/v1/issues?q="+url.QueryEscape(child.id))
					if rows.status != http.StatusOK {
						t.Fatalf("/issues: %d %s", rows.status, rows.body)
					}
					var list struct {
						Items []struct {
							ID       string  `json:"id"`
							ParentID *string `json:"parentIdCurrent"`
						} `json:"items"`
					}
					decodeInto(t, rows, &list)
					found := false
					for _, it := range list.Items {
						if it.ID != child.id {
							continue
						}
						found = true
						if !equalPtr(it.ParentID, want) {
							t.Errorf("/issues parentIdCurrent = %v, want %v", deref(it.ParentID), deref(want))
						}
					}
					if !found {
						t.Errorf("/issues?q=%s: the child is not listed", child.id)
					}
				})
			}
		})
	})

	// INV-28 (4): an epic E in A.space has a child and a blocks-link target
	// in each of A.members (the reader's own space), B.members (another
	// space) and Personal(outsider) (another's "Personal"). The unavailable* numbers
	// count a hidden one only in the reader's own non-personal space; a
	// hidden one elsewhere is in no number — not in the total either.
	t.Run("unavailable-foreign-space", func(t *testing.T) {
		owner := w.f.User(t, testsupport.ActorSpaceOwner).ID
		aSpace := w.byKey[keyASpace]
		epic := aSpace.IDPrefix + "-951"
		testsupport.SeedIssue(t, w.pool, aSpace.ID, owner, epic, "isomatrix foreign-space epic", "open", 2)
		if _, err := w.pool.Exec(context.Background(), `UPDATE issues SET type = 'epic' WHERE id = $1`, epic); err != nil {
			t.Fatalf("make %s an epic: %v", epic, err)
		}
		for i, key := range []string{keyAMembers, keyBMembers, personalKey(testsupport.ActorOutsider)} {
			p := w.byKey[key]
			child := fmt.Sprintf("%s-%d", p.IDPrefix, 952+2*i)
			target := fmt.Sprintf("%s-%d", p.IDPrefix, 953+2*i)
			testsupport.SeedIssue(t, w.pool, p.ID, p.OwnerID, child, "isomatrix foreign-space child", "open", 2)
			testsupport.SeedIssue(t, w.pool, p.ID, p.OwnerID, target, "isomatrix foreign-space blocker", "open", 2)
			testsupport.SeedDep(t, w.pool, child, epic, "parent-child", owner)
			testsupport.SeedDep(t, w.pool, epic, target, "blocks", owner)
		}
		// (hidden blocks-links counted, hidden children counted, total
		// children) per reader, written from INV-28 (4) and the legend.
		want := map[testsupport.Actor][3]int{
			testsupport.ActorParticipantNoMembers: {1, 1, 1}, // A.members hidden in his own space
			testsupport.ActorOnlyA:                {0, 0, 1}, // A.members visible; B, Pers — nowhere
			testsupport.ActorBoth:                 {0, 0, 2}, // A and B visible; Pers — nowhere
			testsupport.ActorAdmin0:               {0, 0, 2}, // every non-personal visible; Pers — nowhere
		}
		for a, exp := range want {
			t.Run(string(a), func(t *testing.T) {
				card := w.get(a, "/api/v1/projects/"+aSpace.ID.String()+"/issues/"+epic+"/")
				if card.status != http.StatusOK {
					t.Fatalf("epic card: %d %s", card.status, card.body)
				}
				var got struct {
					Links int `json:"unavailableBlockingLinks"`
					Epic  *struct {
						Unavailable int `json:"unavailableChildren"`
						Total       int `json:"totalChildren"`
					} `json:"epic"`
				}
				decodeInto(t, card, &got)
				if got.Epic == nil {
					t.Fatalf("epic card without the rollup: %s", card.body)
				}
				if g := [3]int{got.Links, got.Epic.Unavailable, got.Epic.Total}; g != exp {
					t.Errorf("(unavailableBlockingLinks, unavailableChildren, totalChildren) = %v, want %v", g, exp)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Row 23a — comments and history
// ---------------------------------------------------------------------------

func TestIsolationMatrix_CommentsAndHistory(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		// The actor's own visible project, for addressing N objects through a
		// path it may use.
		var own *isoProject
		for _, p := range w.ordered {
			if st := isoCell(a, p.Key); st == cellVisible {
				own = p
				break
			}
		}
		// Only an actor whose expected set is empty (deakt) may have none;
		// for him the «through a visible project» paths do not exist.
		if own == nil && len(testsupport.TenancyVisible[a]) != 0 {
			t.Fatalf("actor %s sees no project in the fixture", a)
		}

		w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
			base := "/api/v1/projects/" + p.ID.String()
			issueBase := base + "/issues/" + p.IssueID
			if st == cellVisible {
				for _, sub := range []string{issueBase + "/comments", issueBase + "/activity", base + "/activity"} {
					r := w.get(a, sub)
					if r.status != http.StatusOK {
						t.Errorf("%s: %d %s", sub, r.status, r.body)
					}
					w.assertNoLeak(t, a, sub, r.body)
				}
				if r := w.get(a, issueBase+"/comments"); !bytes.Contains(r.body, []byte(p.CommentID.String())) {
					t.Errorf("visible comment missing from the list")
				}
				// The seeded history event of the marker is in both feeds.
				for _, sub := range []string{issueBase + "/activity", base + "/activity"} {
					var feed struct {
						Items []struct {
							Entity struct {
								ID string `json:"id"`
							} `json:"entity"`
						} `json:"items"`
					}
					decodeInto(t, w.get(a, sub), &feed)
					found := false
					for _, it := range feed.Items {
						found = found || it.Entity.ID == p.IssueID
					}
					if !found {
						t.Errorf("%s: the marker's history event is missing", sub)
					}
				}
				return
			}

			missingProject := "/api/v1/projects/" + missingUUID()
			assertSameAsMissing(t, "comments list", w.get(a, issueBase+"/comments"),
				w.get(a, missingProject+"/issues/"+p.IssueID+"/comments"))
			assertSameAsMissing(t, "issue activity", w.get(a, issueBase+"/activity"),
				w.get(a, missingProject+"/issues/"+p.IssueID+"/activity"))
			assertSameAsMissing(t, "project activity", w.get(a, base+"/activity"),
				w.get(a, missingProject+"/activity"))

			edit := map[string]any{"body": "isomatrix overwrite", "expectedVersion": 1}
			cpath := issueBase + "/comments/" + p.CommentID.String()
			missingC := missingProject + "/issues/" + p.IssueID + "/comments/" + missingUUID()
			assertSameAsMissing(t, "comment PUT via its project",
				w.do(a, http.MethodPut, cpath, edit), w.do(a, http.MethodPut, missingC, edit))
			assertSameAsMissing(t, "comment DELETE via its project",
				w.do(a, http.MethodDelete, cpath, nil), w.do(a, http.MethodDelete, missingC, nil))
			// Through the actor's own project the N issue is a nonexistent
			// issue of that project.
			if own != nil {
				ownBase := "/api/v1/projects/" + own.ID.String() + "/issues/"
				viaOwn := ownBase + p.IssueID + "/comments/" + p.CommentID.String()
				missingOwn := ownBase + own.IDPrefix + "-9999/comments/" + missingUUID()
				assertSameAsMissing(t, "comment PUT via a visible project",
					w.do(a, http.MethodPut, viaOwn, edit), w.do(a, http.MethodPut, missingOwn, edit))
				assertSameAsMissing(t, "comment DELETE via a visible project",
					w.do(a, http.MethodDelete, viaOwn, nil), w.do(a, http.MethodDelete, missingOwn, nil))
			}

			var body string
			var deleted bool
			if err := w.pool.QueryRow(ctx,
				`SELECT body, deleted_at IS NOT NULL FROM comments WHERE id = $1`, p.CommentID,
			).Scan(&body, &deleted); err != nil {
				t.Fatalf("re-read comment: %v", err)
			}
			if body != p.CommentBody || deleted {
				t.Errorf("N comment changed: body %q deleted %v", body, deleted)
			}
		})
	})
}

// ---------------------------------------------------------------------------
// Rows 24, 24a — lens counters, review
// ---------------------------------------------------------------------------

// isoCounterPlan — where an actor's trap (an N project) and bait (a visible
// project) live. A trap counted by a lens is the leak; the bait proves the
// lens counts at all. An actor who sees nothing (deakt) has no bait: the
// trap alone is checked, and no project row may be listed.
type isoCounterPlan struct {
	trap, bait string
}

var isoCounterPlans = map[testsupport.Actor]isoCounterPlan{
	testsupport.ActorOnlyA:                {trap: keyBMembers, bait: keyASpace},
	testsupport.ActorOnlyB:                {trap: keyAMembers, bait: keyBSpace},
	testsupport.ActorBoth:                 {trap: "L:outsider", bait: keyASpace},
	testsupport.ActorProjectOwner:         {trap: keyBMembers, bait: keyASpace},
	testsupport.ActorSpaceOwner:           {trap: keyBMembers, bait: keyASpace},
	testsupport.ActorParticipantNoMembers: {trap: keyAMembers, bait: keyASpace},
	testsupport.ActorExternal:             {trap: keyASpace, bait: keyAMembers},
	testsupport.ActorOutsider:             {trap: keyAMembers, bait: "L:outsider"},
	// An admin's only N projects are the "Personal" spaces of others (INV-1 d);
	// adm0 holds no role in any space project, so his bait is his own Pers.
	testsupport.ActorAdminA: {trap: "L:outsider", bait: keyASpace},
	testsupport.ActorAdmin0: {trap: "L:outsider", bait: "L:admin0"},
	// deakt sees nothing (INV-1: access only to an active user).
	testsupport.ActorDeactivated: {trap: keyASpace},
}

type isoCounts struct {
	OnMe       int  `json:"onMeOpenCount"`
	Delegated  int  `json:"delegatedOpenCount"`
	Unreviewed *int `json:"delegatedUnreviewedCount"`
}

func (w *isoWorld) counts(t *testing.T, a testsupport.Actor) isoCounts {
	t.Helper()
	r := w.get(a, "/api/v1/me/notification-counts")
	if r.status != http.StatusOK {
		t.Fatalf("notification-counts: %d %s", r.status, r.body)
	}
	var c isoCounts
	decodeInto(t, r, &c)
	if c.Unreviewed == nil {
		t.Fatalf("notification-counts without delegatedUnreviewedCount: %s", r.body)
	}
	return c
}

// seedLensIssues puts three issues into project p for actor a: one on a
// (assigned, accepted, created by someone else), one delegated by a (open),
// one unreviewed by a (closed, delegated, review required). Returns the ids.
func (w *isoWorld) seedLensIssues(t *testing.T, a testsupport.Actor, p *isoProject, n int) []string {
	t.Helper()
	ctx := context.Background()
	u := w.f.User(t, a)
	other := p.OwnerID
	otherName := testsupport.Username(t, w.pool, other)
	if other == u.ID {
		// The actor's own "Personal": the "on me" issue needs another author.
		other = w.f.User(t, testsupport.ActorOnlyA).ID
		otherName = w.f.User(t, testsupport.ActorOnlyA).Username
	}
	ids := []string{
		fmt.Sprintf("%s-%d", p.IDPrefix, n),
		fmt.Sprintf("%s-%d", p.IDPrefix, n+1),
		fmt.Sprintf("%s-%d", p.IDPrefix, n+2),
	}
	testsupport.SeedIssue(t, w.pool, p.ID, other, ids[0], "isomatrix on-me", "open", 2)
	testsupport.SetAssignee(t, w.pool, ids[0], u.Username)
	testsupport.Accept(t, w.pool, ids[0], u.ID, true)
	testsupport.SeedIssue(t, w.pool, p.ID, u.ID, ids[1], "isomatrix delegated", "open", 2)
	testsupport.SetAssignee(t, w.pool, ids[1], otherName)
	testsupport.SeedIssue(t, w.pool, p.ID, u.ID, ids[2], "isomatrix unreviewed", "closed", 2)
	testsupport.SetAssignee(t, w.pool, ids[2], otherName)
	if _, err := w.pool.Exec(ctx,
		`UPDATE issues SET review_required = true, reviewed_at = NULL WHERE id = $1`, ids[2]); err != nil {
		t.Fatalf("mark review required: %v", err)
	}
	return ids
}

// isoRowCounts — the lens counters of one project row.
type isoRowCounts struct {
	Open         int `json:"openCount"`
	OnMeOpen     int `json:"onMeOpenCount"`
	MyOutboxOpen int `json:"myOutboxOpenCount"`
}

// projectRows returns the counters of every project row the actor sees,
// keyed by project id, and fails unless each visible fixture project is
// among them (a page that dropped the row would hide the cell).
func (w *isoWorld) projectRows(t *testing.T, a testsupport.Actor) map[string]isoRowCounts {
	t.Helper()
	r := w.get(a, "/api/v1/projects?limit=100")
	if r.status != http.StatusOK {
		t.Fatalf("GET projects: %d %s", r.status, r.body)
	}
	w.assertNoLeak(t, a, "project rows", r.body)
	var res struct {
		Items []struct {
			ID string `json:"id"`
			isoRowCounts
		} `json:"items"`
		HasMore bool `json:"hasMore"`
	}
	decodeInto(t, r, &res)
	if res.HasMore {
		t.Fatalf("project list of %s spans more than one page", a)
	}
	out := make(map[string]isoRowCounts, len(res.Items))
	for _, it := range res.Items {
		out[it.ID] = it.isoRowCounts
	}
	for _, p := range w.ordered {
		if st := isoCell(a, p.Key); st == cellVisible {
			if _, ok := out[p.ID.String()]; !ok {
				t.Fatalf("visible project %s missing from the rows", p.Key)
			}
		}
	}
	return out
}

func (w *isoWorld) deleteIssues(t *testing.T, ids []string) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), `DELETE FROM issues WHERE id = ANY($1)`, ids); err != nil {
		t.Fatalf("delete issues: %v", err)
	}
}

func TestIsolationMatrix_LensCountersAndReview(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	for i, a := range testsupport.TenancyActors {
		// Every actor numbers its issues in its own range: baits stay in
		// shared projects after the subtest.
		base := 100 + 10*i
		t.Run(string(a), func(t *testing.T) {
			plan := isoCounterPlans[a]
			trapP, baitP := w.byKey[plan.trap], w.byKey[plan.bait]

			trap := w.seedLensIssues(t, a, trapP, base)
			withTrap := w.counts(t, a)

			// The unreviewed list and the review mark on the trap.
			list := w.get(a, "/api/v1/me/created-by?unreviewed=true")
			if list.status != http.StatusOK {
				t.Fatalf("created-by unreviewed: %d %s", list.status, list.body)
			}
			if bytes.Contains(list.body, []byte(`"`+trap[2]+`"`)) {
				t.Errorf("unreviewed list carries the N issue %s", trap[2])
			}
			assertSameAsMissing(t, "review of an N issue",
				w.do(a, http.MethodPost, "/api/v1/projects/"+trapP.ID.String()+"/issues/"+trap[2]+"/review", nil),
				w.do(a, http.MethodPost, "/api/v1/projects/"+missingUUID()+"/issues/"+trap[2]+"/review", nil))
			var marked bool
			if err := w.pool.QueryRow(ctx,
				`SELECT reviewed_at IS NOT NULL FROM issues WHERE id = $1`, trap[2]).Scan(&marked); err != nil {
				t.Fatalf("re-read review mark: %v", err)
			}
			if marked {
				t.Errorf("review of an N issue left a mark")
			}

			w.deleteIssues(t, trap)
			withoutTrap := w.counts(t, a)
			if withTrap.OnMe != withoutTrap.OnMe || withTrap.Delegated != withoutTrap.Delegated ||
				*withTrap.Unreviewed != *withoutTrap.Unreviewed {
				t.Errorf("N issues counted: with trap %+v / %d, without %+v / %d",
					withTrap, *withTrap.Unreviewed, withoutTrap, *withoutTrap.Unreviewed)
			}

			if baitP == nil {
				// The actor sees nothing: no row before or after a trap.
				trap2 := w.seedLensIssues(t, a, trapP, base+6)
				if rows := w.projectRows(t, a); len(rows) != 0 {
					t.Errorf("an actor who sees nothing is listed %d project rows", len(rows))
				}
				w.deleteIssues(t, trap2)
				return
			}

			rowsBefore := w.projectRows(t, a)
			w.seedLensIssues(t, a, baitP, base+3)
			withBait := w.counts(t, a)
			if withBait.OnMe != withoutTrap.OnMe+1 || withBait.Delegated != withoutTrap.Delegated+1 ||
				*withBait.Unreviewed != *withoutTrap.Unreviewed+1 {
				t.Errorf("visible issues not counted: before %+v / %d, after bait %+v / %d",
					withoutTrap, *withoutTrap.Unreviewed, withBait, *withBait.Unreviewed)
			}

			// Project rows, by field. The bait (open on-me + open delegated +
			// closed unreviewed) moves exactly its own row: openCount +2,
			// onMeOpenCount +1, myOutboxOpenCount +1; every other visible row
			// stays. Then a trap in the N project moves no visible row.
			rowsBait := w.projectRows(t, a)
			for id, before := range rowsBefore {
				want := before
				if id == baitP.ID.String() {
					want.Open += 2
					want.OnMeOpen++
					want.MyOutboxOpen++
				}
				if got := rowsBait[id]; got != want {
					t.Errorf("project row %s after the bait: %+v, want %+v", id, got, want)
				}
			}
			trap2 := w.seedLensIssues(t, a, trapP, base+6)
			rowsTrap := w.projectRows(t, a)
			for id, want := range rowsBait {
				if got := rowsTrap[id]; got != want {
					t.Errorf("project row %s moved when N issues were seeded: %+v, want %+v", id, got, want)
				}
			}
			if _, listed := rowsTrap[trapP.ID.String()]; listed {
				t.Errorf("the N trap project is listed")
			}
			w.deleteIssues(t, trap2)
		})
	}
}

// ---------------------------------------------------------------------------
// Rows 23, 24 — the page filter space= (T032)
// ---------------------------------------------------------------------------

// isoSpaceRow — a list row as the space filter is judged: its id and the
// short id of the space of its project.
type isoSpaceRow struct {
	ID      string `json:"id"`
	SpaceID string `json:"spaceId"`
}

// spaceRows pages path (which already carries its query) to the end.
func (w *isoWorld) spaceRows(t *testing.T, a testsupport.Actor, path string) map[string]isoSpaceRow {
	t.Helper()
	out := map[string]isoSpaceRow{}
	cursor := ""
	for page := 0; ; page++ {
		if page > 200 {
			t.Fatalf("%s did not end after 200 pages", path)
		}
		p := path
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		r := w.get(a, p)
		if r.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", p, r.status, r.body)
		}
		w.assertNoLeak(t, a, path, r.body)
		var res struct {
			Items      []isoSpaceRow `json:"items"`
			NextCursor string        `json:"nextCursor"`
			HasMore    bool          `json:"hasMore"`
		}
		decodeInto(t, r, &res)
		for _, it := range res.Items {
			out[it.ID] = it
		}
		if !res.HasMore {
			return out
		}
		cursor = res.NextCursor
	}
}

// TestIsolationMatrix_SpaceFilter extends rows 23 and 24 with the page
// filter `space=` of /issues, /me/created-by and /me/assigned-to (US-3,
// US-13, T032).
//
// The reference is the fixture legend, not the code: an actor's issues of a
// space are the issues of the projects of that space he sees (isoCell); a
// space he does not see, like a nonexistent short id, narrows to nothing —
// the filter never widens the horizon. An issue's space is the space of its
// project, not of its id prefix: an issue of B.space numbered with A.space's
// prefix belongs to B. On the place «space» the outbox list and the outbox
// counters of the space's project rows agree.
//
// Red if the filter lets another space's issue through, drops one of the
// space's visible issues, follows the id prefix, or widens to a space the
// actor does not see; or if /me/created-by?space= disagrees with
// myOutboxOpenCount of that space's rows.
func TestIsolationMatrix_SpaceFilter(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	shortOf := func(id uuid.UUID) string {
		var short string
		if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&short); err != nil {
			t.Fatalf("short id of space %s: %v", id, err)
		}
		return short
	}
	shortA, shortB := shortOf(w.f.SpaceA), shortOf(w.f.SpaceB)
	missing := testsupport.NewShortID(t)
	aSpace, bSpace := w.byKey[keyASpace], w.byKey[keyBSpace]

	// An issue of B.space numbered with A.space's prefix.
	foreignID := aSpace.IDPrefix + "-77"
	testsupport.SeedIssue(t, w.pool, bSpace.ID, bSpace.OwnerID, foreignID,
		"isomatrix foreign prefix "+w.word, "open", 2)
	bSpace.IssueIDs = append(bSpace.IssueIDs, foreignID)

	spaceKeys := map[string][]string{
		shortA: {keyASpace, keyAMembers},
		shortB: {keyBSpace, keyBMembers},
	}
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		q := "/api/v1/issues?limit=50&q=" + url.QueryEscape(w.word)
		for short, keys := range spaceKeys {
			got := w.spaceRows(t, a, q+"&space="+short)
			for _, row := range got {
				if row.SpaceID != short {
					t.Errorf("space=%s lists %s of space %q", short, row.ID, row.SpaceID)
				}
			}
			for _, key := range keys {
				p := w.byKey[key]
				for _, id := range p.IssueIDs {
					_, listed := got[id]
					if visible := isoCell(a, key) == cellVisible; visible != listed {
						t.Errorf("space=%s: issue %s of %s listed=%v, visible=%v", short, id, key, listed, visible)
					}
				}
			}
			for other, otherKeys := range spaceKeys {
				if other == short {
					continue
				}
				for _, key := range otherKeys {
					for _, id := range w.byKey[key].IssueIDs {
						if _, listed := got[id]; listed {
							t.Errorf("space=%s lists %s of %s", short, id, key)
						}
					}
				}
			}
		}
		if got := w.spaceRows(t, a, q+"&space="+missing); len(got) != 0 {
			t.Errorf("a nonexistent space lists %d issues", len(got))
		}
		if isoCell(a, keyBSpace) == cellVisible {
			card := w.get(a, "/api/v1/projects/"+bSpace.ID.String()+"/issues/"+foreignID+"/")
			if card.status != http.StatusOK {
				t.Fatalf("card of %s: %d %s", foreignID, card.status, card.body)
			}
			var dto isoSpaceRow
			decodeInto(t, card, &dto)
			if dto.SpaceID != shortB {
				t.Errorf("card of %s (A's prefix, B's project): spaceId %q, want %q", foreignID, dto.SpaceID, shortB)
			}
		}
	})

	// The place «space»: uAB delegates one open issue in each space.
	both := w.f.User(t, testsupport.ActorBoth)
	onlyA, onlyB := w.f.User(t, testsupport.ActorOnlyA), w.f.User(t, testsupport.ActorOnlyB)
	delegatedA, delegatedB := aSpace.IDPrefix+"-80", bSpace.IDPrefix+"-80"
	testsupport.SeedIssue(t, w.pool, aSpace.ID, both.ID, delegatedA, "isomatrix delegated A "+w.word, "open", 2)
	testsupport.SetAssignee(t, w.pool, delegatedA, onlyA.Username)
	testsupport.SeedIssue(t, w.pool, bSpace.ID, both.ID, delegatedB, "isomatrix delegated B "+w.word, "open", 2)
	testsupport.SetAssignee(t, w.pool, delegatedB, onlyB.Username)

	r := w.get(testsupport.ActorBoth, "/api/v1/projects?limit=100")
	if r.status != http.StatusOK {
		t.Fatalf("GET projects: %d %s", r.status, r.body)
	}
	var rows struct {
		Items []struct {
			SpaceID      string `json:"spaceId"`
			MyOutboxOpen int    `json:"myOutboxOpenCount"`
		} `json:"items"`
		HasMore bool `json:"hasMore"`
	}
	decodeInto(t, r, &rows)
	if rows.HasMore {
		t.Fatalf("the project list of uAB spans more than one page")
	}
	outboxOf := map[string]int{}
	for _, it := range rows.Items {
		outboxOf[it.SpaceID] += it.MyOutboxOpen
	}
	for short, own := range map[string]string{shortA: delegatedA, shortB: delegatedB} {
		t.Run("outbox-"+short, func(t *testing.T) {
			list := w.spaceRows(t, testsupport.ActorBoth, "/api/v1/me/created-by?limit=50&status=open&space="+short)
			if _, ok := list[own]; !ok {
				t.Errorf("/me/created-by?space=%s misses %s", short, own)
			}
			for _, row := range list {
				if row.SpaceID != short {
					t.Errorf("/me/created-by?space=%s lists %s of space %q", short, row.ID, row.SpaceID)
				}
			}
			if outboxOf[short] < 1 || len(list) != outboxOf[short] {
				t.Errorf("space %s: /me/created-by lists %d, the rows count myOutboxOpenCount %d",
					short, len(list), outboxOf[short])
			}
		})
	}
	for a, own := range map[testsupport.Actor]string{testsupport.ActorOnlyA: delegatedA, testsupport.ActorOnlyB: delegatedB} {
		t.Run("assigned-to-"+string(a), func(t *testing.T) {
			mine := shortA
			other := shortB
			if a == testsupport.ActorOnlyB {
				mine, other = shortB, shortA
			}
			if _, ok := w.spaceRows(t, a, "/api/v1/me/assigned-to?limit=50&space="+mine)[own]; !ok {
				t.Errorf("/me/assigned-to?space=%s misses %s", mine, own)
			}
			if got := w.spaceRows(t, a, "/api/v1/me/assigned-to?limit=50&space="+other); len(got) != 0 {
				t.Errorf("/me/assigned-to?space=%s of a space %s does not see lists %d", other, a, len(got))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Row 25 — search: kinds × modes
// ---------------------------------------------------------------------------

// TestIsolationMatrix_Search — row 25: kinds issue, comment, project and
// component × modes degraded, semantic, exact id, batch lookup. The answer of
// /search has no `total` field, so row 25's "total does not count N" has no
// subject on the current wire shape; if the field appears, assert it here.
// The "people" kind does not exist in /search — a named cell of T027.
func TestIsolationMatrix_Search(t *testing.T) {
	w := newIsoWorld(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		for _, mode := range []struct{ name, route string }{
			{"degraded", "/api/v1/search"},
			{"semantic", "/api/v1/search-semantic"},
		} {
			t.Run(mode.name, func(t *testing.T) {
				r := w.get(a, mode.route+"?limit=100&kind=issue,comment,project,component&q="+url.QueryEscape(w.word))
				if r.status != http.StatusOK {
					t.Fatalf("search: %d %s", r.status, r.body)
				}
				var res struct {
					Items []struct {
						Kind string `json:"kind"`
						ID   string `json:"id"`
					} `json:"items"`
					Degraded bool `json:"degraded"`
				}
				decodeInto(t, r, &res)
				if res.Degraded != (mode.name == "degraded") {
					t.Fatalf("degraded = %v in mode %s", res.Degraded, mode.name)
				}
				w.assertNoLeak(t, a, "search "+mode.name, r.body)
				found := map[string]bool{}
				for _, it := range res.Items {
					found[it.Kind+":"+it.ID] = true
				}
				w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
					for _, hit := range []string{
						"issue:" + p.IssueID, "comment:" + p.CommentID.String(), "project:" + p.ID.String(),
						"component:" + p.ComponentID.String(),
					} {
						if found[hit] != (st == cellVisible) {
							t.Errorf("search %s: %s found = %v, want %v", mode.name, hit, found[hit], st == cellVisible)
						}
					}
				})
			})
		}

		t.Run("exact-id", func(t *testing.T) {
			missing := w.get(a, "/api/v1/search/lookup-id?q="+url.QueryEscape("isomissing-1"))
			w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
				r := w.get(a, "/api/v1/search/lookup-id?q="+url.QueryEscape(p.IssueID))
				if st == cellVisible {
					if !bytes.Contains(r.body, []byte(`"`+p.IssueID+`"`)) {
						t.Errorf("lookup-id: visible %s missing: %d %s", p.IssueID, r.status, r.body)
					}
					return
				}
				if r.status != missing.status || !bytes.Equal(r.body, missing.body) {
					t.Errorf("lookup-id of an N issue %d %s differs from a nonexistent one %d %s",
						r.status, r.body, missing.status, missing.body)
				}
			})
		})

		t.Run("batch-lookup", func(t *testing.T) {
			var ids []string
			for _, p := range w.ordered {
				ids = append(ids, p.IssueIDs...)
			}
			r := w.do(a, http.MethodPost, "/api/v1/search/lookup-id-batch", map[string]any{"ids": ids})
			if r.status != http.StatusOK {
				t.Fatalf("lookup-id-batch: %d %s", r.status, r.body)
			}
			w.assertNoLeak(t, a, "lookup-id-batch", r.body)
			var res struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			decodeInto(t, r, &res)
			got := map[string]bool{}
			for _, it := range res.Items {
				got[it.ID] = true
			}
			w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
				for _, id := range p.IssueIDs {
					if got[id] != (st == cellVisible) {
						t.Errorf("lookup-id-batch: %s returned = %v, want %v", id, got[id], st == cellVisible)
					}
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// Row 26 — attachments
// ---------------------------------------------------------------------------

func TestIsolationMatrix_Attachments(t *testing.T) {
	w := newIsoWorld(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		missingList := w.get(a, "/api/v1/issues/isomissing-1/attachments")
		missingLink := w.get(a, "/api/v1/attachments/"+missingUUID()+"/download-url")
		w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
			list := w.get(a, "/api/v1/issues/"+p.IssueID+"/attachments")
			link := w.get(a, "/api/v1/attachments/"+p.AttachmentID.String()+"/download-url")
			if st == cellVisible {
				if list.status != http.StatusOK || !bytes.Contains(list.body, []byte(p.AttachmentID.String())) {
					t.Errorf("list: %d %s", list.status, list.body)
				}
				var u struct {
					URL string `json:"url"`
				}
				decodeInto(t, link, &u)
				if link.status != http.StatusOK || !strings.Contains(u.URL, p.AttachmentID.String()) {
					t.Errorf("download-url: %d %q, want a link to the attachment", link.status, u.URL)
				}
				return
			}
			assertSameAsMissing(t, "attachments list", list, missingList)
			assertSameAsMissing(t, "download-url", link, missingLink)
		})
	})
}

// ---------------------------------------------------------------------------
// Row 27 — my memberships
// ---------------------------------------------------------------------------

func TestIsolationMatrix_MyMemberships(t *testing.T) {
	w := newIsoWorld(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		r := w.get(a, "/api/v1/me/memberships")
		if r.status != http.StatusOK {
			t.Fatalf("memberships: %d %s", r.status, r.body)
		}
		w.assertNoLeak(t, a, "/me/memberships", r.body)
		var res struct {
			Memberships []struct {
				ProjectID string `json:"projectId"`
			} `json:"memberships"`
		}
		decodeInto(t, r, &res)
		got := map[string]bool{}
		for _, m := range res.Memberships {
			got[m.ProjectID] = true
		}
		w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
			if (st == cellVisible) != got[p.ID.String()] {
				t.Errorf("membership row present = %v, want %v", got[p.ID.String()], st == cellVisible)
			}
		})
	})
}

// ---------------------------------------------------------------------------
// Row 28 — task monitor polling
// ---------------------------------------------------------------------------

func (w *isoWorld) poll(token string) isoResp { return w.pollFrom(token, "") }

func (w *isoWorld) pollFrom(token, rawQuery string) isoResp {
	target := "/agent/v1/notifications/poll"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(auth.HeaderAPIKey, token)
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	return isoResp{status: rec.Code, body: rec.Body.Bytes()}
}

func (w *isoWorld) mintMonitor(t *testing.T, user uuid.UUID, p *isoProject) string {
	t.Helper()
	key := seedScriptAPIKey(t, w.pool, user)
	plain, _, err := monitorcreds.New(w.pool, time.Hour).Mint(context.Background(), monitorcreds.MintParams{
		UserID:           user,
		MintedByAPIKeyID: key,
		Audience:         "notifications:poll",
		Capability:       "read",
		ProjectID:        p.ID,
		ScopeIssueID:     p.IssueID,
		EventTypes:       []string{"issue.status_changed"},
	})
	if err != nil {
		t.Fatalf("mint monitor credential: %v", err)
	}
	return plain
}

// monitorRefusal is the poll's refusal for an N scope, written from the
// route's contract (agent_poll.go: @Failure 403 "visibility_lost", the
// terminal stop signal of sk-579), not taken from a run. "As for a
// nonexistent issue" (row 28) is not compared here: a credential is minted
// only for an issue the caller saw (the MCP mint for an N issue answers as for
// a nonexistent one — cmd/api parity), so the poll of an N scope can only
// follow a loss of access, and its refusal is the documented terminal one —
// the same for "never saw" and "lost", with no row or number of the issue.
var monitorRefusal = problemShape{Status: http.StatusForbidden, Code: "visibility_lost", Keys: "error"}

// monitorRevoked is the answer to a credential of a deactivated user: the
// credential itself no longer authenticates (INV-26 — deactivation makes the
// keys invalid), so the poll is refused before any issue is looked at — the
// same answer for every project, which tells nothing about any of them.
var monitorRevoked = problemShape{Status: http.StatusUnauthorized, Code: "unauthenticated", Keys: "error"}

func TestIsolationMatrix_MonitorPoll(t *testing.T) {
	w := newIsoWorld(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		u := w.f.User(t, a)
		w.eachCell(t, a, func(t *testing.T, p *isoProject, st cellState) {
			token := w.mintMonitor(t, u.ID, p)
			if st == cellVisible {
				// An event on the scope after the credential was minted is
				// what the poll must hand over. The epoch cursor is clamped up
				// to the mint time (the live-start first poll is empty by
				// design).
				eid := w.seedIssueEvent(t, p)
				r := w.pollFrom(token, "since_ts=1970-01-01T00:00:00Z&since_id=0")
				var res struct {
					Notifications []struct {
						ID int64 `json:"id"`
					} `json:"notifications"`
				}
				decodeInto(t, r, &res)
				if r.status != http.StatusOK || !slices.ContainsFunc(res.Notifications, func(n struct {
					ID int64 `json:"id"`
				}) bool {
					return n.ID == eid
				}) {
					t.Errorf("poll of a visible issue: %d %s, want event %d", r.status, r.body, eid)
				}
				return
			}
			r := w.poll(token)
			want := monitorRefusal
			if a == testsupport.ActorDeactivated {
				want = monitorRevoked
			}
			if got := shapeOf(t, r); got != want {
				t.Errorf("poll of an N issue: %+v, want %+v", got, want)
			}
			w.assertNoLeak(t, a, "monitor poll", r.body)
		})
	})

	// Losing access: the same credential stops serving the issue.
	t.Run("access-lost", func(t *testing.T) {
		p := w.byKey[keyAMembers]
		member, _ := testsupport.SeedUser(t, w.pool, "user")
		testsupport.AddProjectMember(t, w.pool, p.ID, member, "member")
		token := w.mintMonitor(t, member, p)
		if r := w.poll(token); r.status != http.StatusOK {
			t.Fatalf("poll while a member: %d %s", r.status, r.body)
		}
		if _, err := w.pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, p.ID, member); err != nil {
			t.Fatalf("remove member: %v", err)
		}
		r := w.poll(token)
		if got := shapeOf(t, r); got != monitorRefusal {
			t.Errorf("poll after losing access: %+v, want %+v", got, monitorRefusal)
		}
	})
}

// ---------------------------------------------------------------------------
// Rows 29, 30 — cells waiting for narrowing
// ---------------------------------------------------------------------------

// TestIsolationMatrix_SpaceIDs is row 30 for the kind «space» (T023):
// GET /spaces/{id} of a space the actor does not see answers exactly as a
// nonexistent short id (INV-3: status, `error` code, key set); a space he
// sees answers 200.
//
// The reference is the fixture legend (testsupport/tenancy.go), not the
// code: space A — participants uA, uAB, vP, vPr, uc¬p, admA and deakt, who
// sees nothing while deactivated (E-15); space B — uB, uAB; a "Personal" —
// its owner only, deakt excluded; both admins see every non-personal space
// with the flag on (US-6) and no other user's "Personal".
func TestIsolationMatrix_SpaceIDs(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	shortOf := func(id uuid.UUID) string {
		var short string
		if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&short); err != nil {
			t.Fatalf("short id of space %s: %v", id, err)
		}
		return short
	}
	type isoSpace struct {
		name   string
		short  string
		seenBy []testsupport.Actor
	}
	all := []isoSpace{
		{"A", shortOf(w.f.SpaceA), []testsupport.Actor{
			testsupport.ActorOnlyA, testsupport.ActorBoth, testsupport.ActorProjectOwner,
			testsupport.ActorSpaceOwner, testsupport.ActorParticipantNoMembers,
			testsupport.ActorAdminA, testsupport.ActorAdmin0}},
		{"B", shortOf(w.f.SpaceB), []testsupport.Actor{
			testsupport.ActorOnlyB, testsupport.ActorBoth, testsupport.ActorAdminA, testsupport.ActorAdmin0}},
	}
	for _, owner := range testsupport.TenancyActors {
		id, ok := w.f.PersonalSpace[owner]
		if !ok {
			continue
		}
		var seenBy []testsupport.Actor
		if owner != testsupport.ActorDeactivated {
			seenBy = []testsupport.Actor{owner}
		}
		all = append(all, isoSpace{"personal-" + string(owner), shortOf(id), seenBy})
	}
	if len(all) < 3 {
		t.Fatalf("the fixture has no personal space: %d spaces", len(all))
	}
	missing := testsupport.NewShortID(t)
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		for _, sp := range all {
			t.Run(sp.name, func(t *testing.T) {
				r := w.get(a, "/api/v1/spaces/"+sp.short)
				if slices.Contains(sp.seenBy, a) {
					if r.status != http.StatusOK {
						t.Errorf("a space the actor sees: %d %s", r.status, r.body)
					}
					return
				}
				assertSameAsMissing(t, "space "+sp.name, r, w.get(a, "/api/v1/spaces/"+missing))
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Row 29 — people (T027)
// ---------------------------------------------------------------------------

// isoPeopleSeen is the reference of row 29's people cells, written from the
// fixture (testsupport.SeedTenancy) by INV-9 before the code: a person is
// visible to a caller if he is a participant of one of the caller's spaces,
// or an external member of a project the caller can access; an external
// caller sees only the co-members of his projects; an instance admin has no
// exception (the decision of 2026-10-14). Space A: vPr (owner), uA, uAB,
// vP, uc¬p, admA, deakt; space B: uB (owner), uAB; vnsh is a member of
// A.members only, which vP (owner), vPr (implicit owner), uA, uAB, admA,
// deakt reach and uc¬p does not; adm0 reaches it by his role. Everyone sees
// himself. A deactivated account reaches no project (the access relation
// gives it nothing, INV-26), so deakt sees the participants of his space
// only, not the external member of a project.
func isoPeopleSeen() map[testsupport.Actor][]testsupport.Actor {
	spaceA := []testsupport.Actor{testsupport.ActorSpaceOwner, testsupport.ActorOnlyA, testsupport.ActorBoth,
		testsupport.ActorProjectOwner, testsupport.ActorParticipantNoMembers, testsupport.ActorAdminA,
		testsupport.ActorDeactivated}
	spaceB := []testsupport.Actor{testsupport.ActorOnlyB, testsupport.ActorBoth}
	with := func(sets ...[]testsupport.Actor) []testsupport.Actor {
		var out []testsupport.Actor
		for _, s := range sets {
			out = append(out, s...)
		}
		return out
	}
	ext := []testsupport.Actor{testsupport.ActorExternal}
	return map[testsupport.Actor][]testsupport.Actor{
		testsupport.ActorOnlyA:                with(spaceA, ext),
		testsupport.ActorOnlyB:                spaceB,
		testsupport.ActorBoth:                 with(spaceA, spaceB, ext),
		testsupport.ActorProjectOwner:         with(spaceA, ext),
		testsupport.ActorSpaceOwner:           with(spaceA, ext),
		testsupport.ActorParticipantNoMembers: spaceA,
		testsupport.ActorAdminA:               with(spaceA, ext),
		testsupport.ActorDeactivated:          spaceA,
		testsupport.ActorAdmin0:               {testsupport.ActorAdmin0, testsupport.ActorExternal},
		testsupport.ActorOutsider:             {testsupport.ActorOutsider},
		// The co-members of A.members: its owner vP, the members uA, uAB,
		// vnsh, admA, deakt, and vPr, the implicit owner of A's projects.
		testsupport.ActorExternal: {testsupport.ActorExternal, testsupport.ActorProjectOwner, testsupport.ActorOnlyA,
			testsupport.ActorBoth, testsupport.ActorAdminA, testsupport.ActorDeactivated, testsupport.ActorSpaceOwner},
	}
}

// TestIsolationMatrix_People — row 29's people cells: the directory (and
// its search, row 25's "people" kind), the profile, and the people a
// mention in an issue resolves to. A person outside the reference is N: no
// directory row for his exact username, and a profile request answered as
// for a nonexistent username (INV-3).
func TestIsolationMatrix_People(t *testing.T) {
	w := newIsoWorld(t)
	seen := isoPeopleSeen()
	missing := "zz" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		want := map[testsupport.Actor]bool{}
		for _, v := range seen[a] {
			want[v] = true
		}
		want[a] = true
		for _, v := range testsupport.TenancyActors {
			person := w.f.User(t, v)
			t.Run(string(v), func(t *testing.T) {
				// people-directory / search-people: the exact username.
				r := w.get(a, "/api/v1/users/directory?limit=100&q="+url.QueryEscape(person.Username))
				if r.status != http.StatusOK {
					t.Fatalf("directory: %d %s", r.status, r.body)
				}
				var page users.DirectoryResult
				decodeInto(t, r, &page)
				listed := false
				for _, it := range page.Items {
					listed = listed || it.ID == person.ID
					if bytes.Contains(r.body, []byte("@test.local")) {
						t.Errorf("a directory row carries an email: %s", r.body)
					}
				}
				// The directory lists active people only.
				if wantListed := want[v] && !person.Deactivated; listed != wantListed {
					t.Errorf("directory lists %s = %v, want %v", v, listed, wantListed)
				}

				// people-profile.
				pr := w.get(a, "/api/v1/users/profile/"+person.Username)
				if want[v] {
					if pr.status != http.StatusOK {
						t.Errorf("profile of %s: %d %s, want 200", v, pr.status, pr.body)
					}
					if v != a && bytes.Contains(pr.body, []byte("@test.local")) {
						t.Errorf("profile of another person carries an email (INV-27): %s", pr.body)
					}
				} else {
					assertSameAsMissing(t, "profile of "+string(v), pr, w.get(a, "/api/v1/users/profile/"+missing))
				}
			})
		}
	})

	// mention-candidates: a mention in an issue of A.members resolves to the
	// people of the issue's context — the participants of space A and its
	// external member (INV-9 "in the context of the task - in its project") and, since
	// sk-741, adm0 — the instance admin is in the context of a non-personal
	// project; a person of another space and the outsider stay text, for
	// every reader.
	p := w.byKey["A.members"]
	var text strings.Builder
	for _, v := range testsupport.TenancyActors {
		text.WriteString("@" + w.f.User(t, v).Username + " ")
	}
	if _, err := w.pool.Exec(t.Context(), `UPDATE issues SET description = $2 WHERE id = $1`, p.IssueID, text.String()); err != nil {
		t.Fatal(err)
	}
	inContext := map[testsupport.Actor]bool{testsupport.ActorExternal: true}
	for _, v := range []testsupport.Actor{testsupport.ActorSpaceOwner, testsupport.ActorOnlyA, testsupport.ActorBoth,
		testsupport.ActorProjectOwner, testsupport.ActorParticipantNoMembers, testsupport.ActorAdminA,
		testsupport.ActorDeactivated, testsupport.ActorAdmin0} {
		inContext[v] = true
	}
	r := w.get(testsupport.ActorProjectOwner, "/api/v1/projects/"+p.ID.String()+"/issues/"+p.IssueID)
	if r.status != http.StatusOK {
		t.Fatalf("issue card: %d %s", r.status, r.body)
	}
	var card struct {
		Mentions []struct {
			UserID uuid.UUID `json:"userId"`
		} `json:"mentions"`
	}
	decodeInto(t, r, &card)
	resolved := map[uuid.UUID]bool{}
	for _, m := range card.Mentions {
		resolved[m.UserID] = true
	}
	for _, v := range testsupport.TenancyActors {
		if got := resolved[w.f.User(t, v).ID]; got != inContext[v] {
			t.Errorf("mention-candidates: %s resolved = %v, want %v", v, got, inContext[v])
		}
	}
}

// TestIsolationMatrix_AssigneePick — row 29's assignee cell (T028, INV-30
// as changed by sk-741). Reference, written from the fixture by INV-30: the
// people who may work in A.members — its owner vP, the implicit owner vPr,
// the members uA, uAB, vnsh, admA (a member row; admin_member) and adm0 (no
// membership at all: the instance admin has the owner's rights in a
// non-personal project, the maintainers' decision 2026-10-17). deakt has a row
// but an inactive account reaches nothing; uc¬p, uB and the outsider may
// not. The owner vP assigns:
//
//   - by id: a person who may work is assigned; anybody else is refused 422
//     assignee_not_allowed exactly as an id that names nobody (N);
//   - by username: always stored as text; assigneeUserId is the person only
//     if he may work, absent otherwise (no N id leaks into the card).
func TestIsolationMatrix_AssigneePick(t *testing.T) {
	w := newIsoWorld(t)
	p := w.byKey[keyAMembers]
	workers := map[testsupport.Actor]bool{
		testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorOnlyA: true,
		testsupport.ActorBoth: true, testsupport.ActorExternal: true, testsupport.ActorAdminA: true,
		testsupport.ActorAdmin0: true,
	}
	patch := func(assignee string) isoResp {
		t.Helper()
		var version int
		if err := w.pool.QueryRow(t.Context(), `SELECT version FROM issues WHERE id = $1`, p.IssueID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		u := w.f.User(t, testsupport.ActorProjectOwner)
		raw, _ := json.Marshal(map[string]any{"assignee": assignee})
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+p.ID.String()+"/issues/"+p.IssueID, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", fmt.Sprint(version))
		req.Header.Set(auth.HeaderUserID, u.ID.String())
		req.Header.Set(auth.HeaderUsername, u.Username)
		req.Header.Set(auth.HeaderUserRole, u.Role)
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		return isoResp{status: rec.Code, body: rec.Body.Bytes()}
	}
	missing := patch(uuid.NewString())
	if missing.status != http.StatusUnprocessableEntity {
		t.Fatalf("an id naming nobody: %d %s, want 422", missing.status, missing.body)
	}
	if got := shapeOf(t, missing).Code; got != "assignee_not_allowed" {
		t.Fatalf("an id naming nobody: code %q, want assignee_not_allowed", got)
	}
	for _, v := range testsupport.TenancyActors {
		person := w.f.User(t, v)
		t.Run(string(v), func(t *testing.T) {
			r := patch(person.ID.String())
			if workers[v] {
				if r.status != http.StatusOK {
					t.Errorf("by id: %d %s, want 200", r.status, r.body)
				}
			} else if got, want := shapeOf(t, r), shapeOf(t, missing); got != want {
				t.Errorf("by id: %+v, want the answer of an id naming nobody %+v", got, want)
			}

			r = patch(person.Username)
			if r.status != http.StatusOK {
				t.Fatalf("by username: %d %s, want 200 (stored as text)", r.status, r.body)
			}
			var card struct {
				Assignee       *string    `json:"assignee"`
				AssigneeUserID *uuid.UUID `json:"assigneeUserId"`
			}
			decodeInto(t, r, &card)
			if card.Assignee == nil || *card.Assignee != person.Username {
				t.Errorf("by username: assignee %v, want the text %q", card.Assignee, person.Username)
			}
			resolved := card.AssigneeUserID != nil && *card.AssigneeUserID == person.ID
			if resolved != workers[v] {
				t.Errorf("by username: resolved to the person = %v, want %v (assigneeUserId %v)", resolved, workers[v], card.AssigneeUserID)
			}
			if card.AssigneeUserID != nil && *card.AssigneeUserID != person.ID {
				t.Errorf("by username: assigneeUserId %v names somebody else", *card.AssigneeUserID)
			}
		})
	}
}

// TestIsolationMatrix_Queues — row 29's queue cells (T029, INV-11): every
// actor holds, in EVERY fixture project, a feed notification carded for
// Watching, a mention notification, a subscription and a reminder — rows a
// stale fan-out or a lost access would leave behind. Inbox, Mentions,
// Watching and reminders show the rows of the projects the actor sees now
// (row 22's reference, testsupport.TenancyVisible) and no token of an N
// project; the Mentions and Watching badges count exactly the visible
// projects' rows.
func TestIsolationMatrix_Queues(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		u := w.f.User(t, a)
		visible := 0
		for _, p := range w.ordered {
			if isoCell(a, p.Key) == cellVisible {
				visible++
			}
			for _, typ := range []string{"issue.status_changed", "issue.mention"} {
				ev := w.seedIssueEvent(t, p)
				if _, err := w.pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id)
VALUES ($1, $2, $3, $4, true, CASE WHEN $3 = 'issue.status_changed' THEN $5 END)`,
					u.ID, ev, typ, p.ID, p.IssueID); err != nil {
					t.Fatalf("seed notification: %v", err)
				}
			}
			if _, err := w.pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual']::text[], now()) ON CONFLICT DO NOTHING`, p.IssueID, u.ID); err != nil {
				t.Fatalf("seed watch: %v", err)
			}
			testsupport.SeedReminder(t, w.pool, p.IssueID, u.ID)
		}
		surfaces := []struct{ name, path string }{
			{"inbox", "/api/v1/notifications?scope=all&limit=100"},
			{"mentions", "/api/v1/notifications?scope=mentions&limit=100"},
			{"watching", "/api/v1/me/watching"},
			{"reminders", "/api/v1/reminders?state=all"},
		}
		for _, sf := range surfaces {
			t.Run(sf.name, func(t *testing.T) {
				r := w.get(a, sf.path)
				if r.status != http.StatusOK {
					t.Fatalf("%s: %d %s", sf.name, r.status, r.body)
				}
				w.assertNoLeak(t, a, sf.name, r.body)
				for _, p := range w.ordered {
					if isoCell(a, p.Key) == cellVisible && !bytes.Contains(r.body, []byte(`"`+p.IssueID+`"`)) {
						t.Errorf("%s: the row of the visible %s is missing", sf.name, p.Key)
					}
				}
			})
		}
		t.Run("badges", func(t *testing.T) {
			r := w.get(a, "/api/v1/me/notification-counts")
			if r.status != http.StatusOK {
				t.Fatalf("notification-counts: %d %s", r.status, r.body)
			}
			var c struct {
				Mentions int `json:"mentions"`
				Watching int `json:"watching"`
			}
			decodeInto(t, r, &c)
			if c.Mentions != visible || c.Watching != visible {
				t.Errorf("badges mentions=%d watching=%d, want %d each (the visible projects)", c.Mentions, c.Watching, visible)
			}
		})
	})
}

// TestIsolationMatrix_HotZone — row 29's HOT ZONE cell (T030, US-13,
// INV-28, INV-23). Reference, from the fixture: an actor's reach is the
// projects he sees plus the projects of his own non-personal spaces he does
// not see — in this fixture only uc¬p has such a project, A.members (space
// A). A contributing project may appear only anonymized: its identifiers
// may stay (the accepted level, redact.Soft) but never its name, alias or
// text. Any other N project — of another space, another person's "Personal" —
// gives no token at all on any HOT ZONE surface. `space=` narrows: uAB with
// space=B gets nothing of A.
func TestIsolationMatrix_HotZone(t *testing.T) {
	w := newIsoWorld(t)
	for _, p := range w.ordered {
		w.seedIssueEvent(t, p)
	}
	contributing := map[testsupport.Actor][]string{
		testsupport.ActorParticipantNoMembers: {keyAMembers},
	}
	surfaces := []string{
		"/api/v1/dashboard/scoring?window=all-time",
		"/api/v1/dashboard/feed?limit=100",
		"/api/v1/dashboard/in-progress?limit=200",
		"/api/v1/dashboard/race?window=all",
		"/api/v1/dashboard/projects?window=all",
		"/api/v1/dashboard/agents",
	}
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		// The identifiers of a contributing project may stay; its name,
		// alias and text may not.
		var allowed []string
		for _, key := range contributing[a] {
			p := w.byKey[key]
			for _, tok := range p.tokens() {
				if tok != p.Name && tok != p.Alias && tok != p.CommentBody {
					allowed = append(allowed, tok)
				}
			}
		}
		for _, path := range surfaces {
			r := w.get(a, path)
			if r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", path, r.status, r.body)
			}
			w.assertNoLeakExcept(t, a, path, r.body, allowed)
		}
		if len(contributing[a]) > 0 {
			r := w.get(a, "/api/v1/dashboard/projects?window=all")
			if !bytes.Contains(r.body, []byte(`"redacted":true`)) {
				t.Errorf("projects: no anonymized row of the contributing project for %s: %s", a, r.body)
			}
		}
	})
	t.Run("space narrows", func(t *testing.T) {
		var bShort string
		if err := w.pool.QueryRow(t.Context(), `SELECT short_id FROM spaces WHERE id = $1`, w.f.SpaceB).Scan(&bShort); err != nil {
			t.Fatal(err)
		}
		for _, path := range surfaces {
			r := w.get(testsupport.ActorBoth, path+"&space="+bShort)
			if path == "/api/v1/dashboard/agents" {
				r = w.get(testsupport.ActorBoth, path+"?space="+bShort)
			}
			if r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", path, r.status, r.body)
			}
			for _, key := range []string{keyASpace, keyAMembers} {
				for _, tok := range w.byKey[key].tokens() {
					if bytes.Contains(r.body, []byte(tok)) {
						t.Errorf("%s with space=B carries %q of %s", path, tok, key)
					}
				}
			}
		}
	})
}

// TestIsolationMatrix_SearchCursor — row 29's search-cursor cell (T031,
// K-5, E-18). A cursor is untrusted base64; a page is served only from the
// cache entry of the same caller with the same scope:
//
//   - uA's cursor presented by uB answers exactly as a garbage cursor
//     (400 invalid_cursor) — and so does the same key with a rewritten
//     offset;
//   - after uA loses A.members between pages, his own cursor is refused the
//     same way, and a new search returns nothing of A.members.
func TestIsolationMatrix_SearchCursor(t *testing.T) {
	w := newIsoWorld(t)
	path := "/api/v1/search?limit=1&kind=issue,comment,project,component&q=" + url.QueryEscape(w.word)
	page := func(a testsupport.Actor, cursor string) isoResp {
		t.Helper()
		p := path
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		return w.get(a, p)
	}
	cursorOf := func(r isoResp) string {
		t.Helper()
		var body struct {
			NextCursor *string `json:"nextCursor"`
		}
		decodeInto(t, r, &body)
		if body.NextCursor == nil {
			t.Fatalf("no next cursor: %s", r.body)
		}
		return *body.NextCursor
	}
	garbage := page(testsupport.ActorOnlyB, "bm90LWEtY3Vyc29y")

	first := page(testsupport.ActorOnlyA, "")
	if first.status != http.StatusOK {
		t.Fatalf("first page: %d %s", first.status, first.body)
	}
	cur := cursorOf(first)

	t.Run("another user's cursor", func(t *testing.T) {
		assertSameAsMissing(t, "uA's cursor presented by uB", page(testsupport.ActorOnlyB, cur), garbage)
	})
	t.Run("the same key with a rewritten offset", func(t *testing.T) {
		raw, err := base64.RawURLEncoding.DecodeString(cur)
		if err != nil {
			raw, err = base64.StdEncoding.DecodeString(cur)
		}
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("cursor json: %v", err)
		}
		for k, v := range c {
			if _, isNum := v.(float64); isNum {
				c[k] = float64(2)
			}
		}
		forged, _ := json.Marshal(c)
		assertSameAsMissing(t, "a rewritten cursor presented by uB",
			page(testsupport.ActorOnlyB, base64.RawURLEncoding.EncodeToString(forged)), garbage)
	})
	t.Run("access lost between pages", func(t *testing.T) {
		u := w.f.User(t, testsupport.ActorOnlyA)
		p := w.byKey[keyAMembers]
		if _, err := w.pool.Exec(t.Context(), `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, p.ID, u.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = w.pool.Exec(context.Background(), `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, 'member', now(), $3, true, false) ON CONFLICT DO NOTHING`, p.ID, u.ID, p.OwnerID)
		})
		assertSameAsMissing(t, "uA's cursor after he lost A.members", page(testsupport.ActorOnlyA, cur), garbage)
		fresh := w.get(testsupport.ActorOnlyA, "/api/v1/search?limit=100&kind=issue,comment,project,component&q="+url.QueryEscape(w.word))
		if fresh.status != http.StatusOK {
			t.Fatalf("fresh search: %d %s", fresh.status, fresh.body)
		}
		for _, tok := range p.tokens() {
			if bytes.Contains(fresh.body, []byte(tok)) {
				t.Errorf("after the loss, a new search carries %q of A.members", tok)
			}
		}
	})
}

// TestIsolationMatrix_Themes is the cell «themes» of row 29 and the kind
// «theme» of row 30 (sk-674.18 T035, US-21, INV-3).
//
// The reference is US-21 and SC-023, not the code: a theme is its author's;
// every other actor neither lists it (its name and its id appear in no
// answer) nor reads, edits, deletes or selects it — each of those answers
// exactly as for a nonexistent theme id (status, `error` code, key set) and
// writes nothing. A theme name is unique only among one author's themes:
// another author creates the same name; the author's own second theme of
// that name (in another case) is refused name_taken.
//
// Red if any actor but the author sees the theme's name or id, if a read,
// edit, delete or selection of it differs from a nonexistent id or changes
// it, or if the name rule leaks across authors.
func TestIsolationMatrix_Themes(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	author := w.f.User(t, testsupport.ActorOnlyA)
	other := w.f.User(t, testsupport.ActorOnlyB)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = w.pool.Exec(c, `UPDATE users SET custom_theme_id = NULL WHERE custom_theme_id IN (SELECT id FROM themes WHERE created_by = ANY($1))`,
			[]uuid.UUID{author.ID, other.ID})
		_, _ = w.pool.Exec(c, `DELETE FROM themes WHERE created_by = ANY($1)`, []uuid.UUID{author.ID, other.ID})
	})
	name := "isotheme " + w.word
	created := w.do(testsupport.ActorOnlyA, http.MethodPost, "/api/v1/themes",
		map[string]any{"name": name, "base": "dark", "variables": map[string]string{"--bg": "#101010"}})
	if created.status != http.StatusCreated {
		t.Fatalf("the author creates a theme: %d %s", created.status, created.body)
	}
	var theme struct {
		ID string `json:"id"`
	}
	decodeInto(t, created, &theme)
	missing := uuid.NewString()

	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		list := w.get(a, "/api/v1/themes")
		if a == testsupport.ActorOnlyA {
			if list.status != http.StatusOK || !bytes.Contains(list.body, []byte(theme.ID)) {
				t.Fatalf("the author's list misses the theme: %d %s", list.status, list.body)
			}
			if r := w.get(a, "/api/v1/themes/"+theme.ID); r.status != http.StatusOK {
				t.Errorf("the author reads the theme: %d %s", r.status, r.body)
			}
			return
		}
		for _, token := range []string{theme.ID, name} {
			if bytes.Contains(list.body, []byte(token)) {
				t.Errorf("the list of %s carries %q of another author's theme", a, token)
			}
		}
		assertSameAsMissing(t, "read", w.get(a, "/api/v1/themes/"+theme.ID), w.get(a, "/api/v1/themes/"+missing))
		patch := map[string]any{"name": "hijacked " + w.word}
		assertSameAsMissing(t, "edit",
			w.do(a, http.MethodPatch, "/api/v1/themes/"+theme.ID, patch),
			w.do(a, http.MethodPatch, "/api/v1/themes/"+missing, patch))
		assertSameAsMissing(t, "delete",
			w.do(a, http.MethodDelete, "/api/v1/themes/"+theme.ID, nil),
			w.do(a, http.MethodDelete, "/api/v1/themes/"+missing, nil))
		pick := func(id string) map[string]any {
			return map[string]any{"mode": "custom", "builtinTheme": "dark", "customThemeId": id}
		}
		assertSameAsMissing(t, "select",
			w.do(a, http.MethodPut, "/api/v1/me/theme", pick(theme.ID)),
			w.do(a, http.MethodPut, "/api/v1/me/theme", pick(missing)))

		var gotName string
		var selected bool
		if err := w.pool.QueryRow(ctx, `
SELECT t.name, EXISTS (SELECT 1 FROM users u WHERE u.id = $2 AND u.custom_theme_id = t.id)
  FROM themes t WHERE t.id = $1`, theme.ID, w.f.User(t, a).ID).Scan(&gotName, &selected); err != nil {
			t.Fatalf("re-read the theme after %s: %v", a, err)
		}
		if gotName != name || selected {
			t.Errorf("%s changed another author's theme: name %q, selected %v", a, gotName, selected)
		}
	})

	t.Run("name-per-author", func(t *testing.T) {
		same := w.do(testsupport.ActorOnlyB, http.MethodPost, "/api/v1/themes",
			map[string]any{"name": name, "base": "light", "variables": map[string]string{}})
		if same.status != http.StatusCreated {
			t.Errorf("another author creates the same name: %d %s", same.status, same.body)
		}
		again := w.do(testsupport.ActorOnlyA, http.MethodPost, "/api/v1/themes",
			map[string]any{"name": strings.ToUpper(name), "base": "light", "variables": map[string]string{}})
		var refusal struct {
			Error string `json:"error"`
		}
		decodeInto(t, again, &refusal)
		if again.status != http.StatusUnprocessableEntity || refusal.Error != "name_taken" {
			t.Errorf("the author's second theme of the same name: %d %s, want 422 name_taken", again.status, again.body)
		}
	})
}

// TestIsolationMatrix_InvitationIDs is row 30 for the invitation (T040):
// every route that takes an invitation id answers an invitation the caller
// may not act on exactly as a nonexistent id — status, `error` code and key
// set — and changes nothing. Reference, written from the fixture before the
// code (INV-3; spaces/contract.go: the owner and the admin manage a space's
// invitations, the addressee answers his own): the invitation is in space B,
// to an email of nobody in the fixture. It is acted on by B's managers
// alone — its owner uB and the two effective admins (the flag is on); the
// Inbox answers are nobody's here. Through space A's path it is nobody's:
// an invitation id is looked up within the space of the route.
func TestIsolationMatrix_InvitationIDs(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	var shortA, shortB string
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT short_id FROM spaces WHERE id = $1), (SELECT short_id FROM spaces WHERE id = $2)`,
		w.f.SpaceA, w.f.SpaceB).Scan(&shortA, &shortB); err != nil {
		t.Fatalf("short ids: %v", err)
	}
	var inv uuid.UUID
	if err := w.pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day') RETURNING id`,
		w.f.SpaceB, "isoinv-"+w.word+"@nobody.test", w.f.User(t, testsupport.ActorOnlyB).ID).Scan(&inv); err != nil {
		t.Fatalf("seeding the invitation: %v", err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = $1`, inv) })
	var expires time.Time
	if err := w.pool.QueryRow(ctx, `SELECT expires_at FROM space_invitations WHERE id = $1`, inv).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	managersOfB := []testsupport.Actor{testsupport.ActorOnlyB, testsupport.ActorAdminA, testsupport.ActorAdmin0}
	missing := uuid.NewString()

	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		for _, path := range []string{"/accept", "/decline"} {
			assertSameAsMissing(t, "me"+path,
				w.do(a, http.MethodPost, "/api/v1/me/invitations/"+inv.String()+path, nil),
				w.do(a, http.MethodPost, "/api/v1/me/invitations/"+missing+path, nil))
		}
		spacesToTry := []string{shortA}
		if !slices.Contains(managersOfB, a) {
			spacesToTry = append(spacesToTry, shortB)
		}
		for _, short := range spacesToTry {
			base := "/api/v1/spaces/" + short + "/invitations/"
			assertSameAsMissing(t, "resend via "+short,
				w.do(a, http.MethodPost, base+inv.String()+"/resend", nil),
				w.do(a, http.MethodPost, base+missing+"/resend", nil))
			assertSameAsMissing(t, "revoke via "+short,
				w.do(a, http.MethodDelete, base+inv.String(), nil),
				w.do(a, http.MethodDelete, base+missing, nil))
		}
		var status string
		var gotExpires time.Time
		if err := w.pool.QueryRow(ctx, `SELECT status, expires_at FROM space_invitations WHERE id = $1`, inv).
			Scan(&status, &gotExpires); err != nil {
			t.Fatalf("re-read the invitation: %v", err)
		}
		if status != "pending" || !gotExpires.Equal(expires) {
			t.Errorf("%s changed an invitation it may not act on: status %s, expiry %v → %v", a, status, expires, gotExpires)
		}
	})
}

// TestIsolationMatrix_NarrowingCells names every cell of row 29 and the
// object kinds of row 30 that the current code does not narrow yet. Each
// narrowing task replaces its skip with the assertion against isoCell.
func TestIsolationMatrix_NarrowingCells(t *testing.T) {
	cells := []struct{ surface, task string }{
		// inbox, mentions, watching, badges and reminders are asserted since
		// T029: TestIsolationMatrix_Queues.
		// people-directory, people-profile, mention-candidates and
		// search-people are asserted since T027: TestIsolationMatrix_People.
		// assignee-pick is asserted since T028: TestIsolationMatrix_AssigneePick.
		// hot-zone is asserted since T030: TestIsolationMatrix_HotZone.
		// search-cursor is asserted since T031: TestIsolationMatrix_SearchCursor.
		// themes and inv3-theme-id are asserted since T035:
		// TestIsolationMatrix_Themes.
		// inv3-invitation-id is asserted since T040:
		// TestIsolationMatrix_InvitationIDs.
	}
	for _, c := range cells {
		t.Run(c.surface, func(t *testing.T) {
			t.Skipf("waiting for narrowing: %s", c.task)
		})
	}
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func deref(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return *s
}

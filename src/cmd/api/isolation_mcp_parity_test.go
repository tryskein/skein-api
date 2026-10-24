package main

// isolation_mcp_parity_test.go — sk-674.18 T004 (SC-002):
// REST and MCP answer the same request with the same set, on the actors of
// row 22 (fixture testsupport.SeedTenancy, T003):
//
//	skein_list crossProject ≡ GET /api/v1/issues
//	skein_search            ≡ GET /api/v1/search
//	skein_show              ≡ GET /api/v1/projects/{projectID}/issues/{issueID}
//
// The property is the EQUALITY of the channels, so it is asserted for every
// actor, admins included (since T017 both channels show an admin every
// non-personal project and no "Personal" of another). The REST
// isolation cells themselves are in internal/handler/isolation_matrix_test.go.
//
// Both channels go through the PRODUCTION router (buildRouter over
// config.Load): REST with the BFF's channel secret and user headers, MCP with
// the channel secret and the actor's own API key.
//
// Row 22's MCP half (project list and resolution by name / prefix / alias)
// is here too: the same set as REST, never an N match.
//
// The deactivated actor is refused on both channels: on REST its session
// (RequireSessionEpoch, T036/K-16 — 401 session_revoked); on MCP its key is
// revoked, as the admin deactivation path (users.Store.Deactivate) does.
// Row 28's MCP half is here too: minting a monitor credential for an N issue
// answers exactly as for a nonexistent one.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

type parityStack struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router chi.Router
	f      *testsupport.Tenancy
	word   string
	keys   map[testsupport.Actor]string
}

func newParityStack(t *testing.T) *parityStack {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-parity.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	f := testsupport.SeedTenancy(t, pool)
	s := &parityStack{
		t: t, pool: pool, f: f,
		word: "parityword" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""),
		keys: map[testsupport.Actor]string{},
	}
	for _, p := range f.Projects() {
		if _, err := pool.Exec(ctx, `UPDATE issues SET title = title || ' ' || $2 WHERE id = $1`,
			p.IssueID, s.word); err != nil {
			t.Fatalf("tag title: %v", err)
		}
	}

	for _, a := range testsupport.TenancyActors {
		u := f.User(t, a)
		plain, prefix, hash, err := auth.GenerateAPIKey(cfg.BcryptCost)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		keyID, _, err := apikeys.New(pool).Create(ctx, u.ID, "isolation parity", prefix, hash)
		if err != nil {
			t.Fatalf("create api key for %s: %v", a, err)
		}
		// Registered after the users, so it runs before their cleanup.
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM monitor_credentials WHERE user_id = $1`, u.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE user_id = $1`, u.ID)
		})
		if u.Deactivated {
			if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, keyID); err != nil {
				t.Fatalf("revoke key of %s: %v", a, err)
			}
		}
		s.keys[a] = plain
	}

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	notifStore := notifications.NewStore(pool)
	searchSvc, err := search.NewService(search.NewStore(pool), nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil,                                   // authH
		handler.NewProjectsHandler(pool, nil), // projectsH
		nil,                                   // componentsH
		handler.NewIssuesHandler(pool, projectsStore, depsStore), // issuesH
		handler.NewDependenciesHandler(pool, depsStore, issuesStore),
		handler.NewReadyHandler(pool, depsStore),
		handler.NewSearchHandler(searchSvc),
		nil,                                       // notifH
		handler.NewBulkHandler(pool, issuesStore), // bulkH
		nil, nil, nil, // apiKeysH, usersH, issuesMeH
		handler.NewIssuesAllHandler(pool, issuesStore, nil),
		nil, nil, // activityH, dashboardH
		nil,                                         // usersStore
		issuesStore,                                 // issuesStore
		depsStore,                                   // depsStore
		projectsStore,                               // projectsStore
		notifStore,                                  // notifStore
		nil,                                         // remindersStore
		searchSvc,                                   // searchSvc
		nil,                                         // s3Client
		nil, nil, nil, nil, nil, nil, nil, nil, nil, // attachmentsH…pushH
	)
	s.router = router
	return s
}

// rest performs a BFF-channel request on behalf of actor a.
func (s *parityStack) rest(a testsupport.Actor, method, path string, body any) (int, []byte) {
	s.t.Helper()
	u := s.f.User(s.t, a)
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, u.ID.String())
	req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
	req.Header.Set(auth.HeaderUsername, u.Username)
	req.Header.Set(auth.HeaderUserRole, u.Role)
	if u.External {
		req.Header.Set(auth.HeaderUserIsExternal, "true")
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// mcp posts one JSON-RPC message with actor a's key and returns the HTTP
// status, the decoded body and the session id.
func (s *parityStack) mcp(a testsupport.Actor, session, method string, params any) (int, map[string]any, string) {
	s.t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		s.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderAPIKey, s.keys[a])
	req.Header.Set(auth.HeaderSurface, auth.SurfaceInternal)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			s.t.Fatalf("%s: response is not JSON: %v\n%s", method, err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Header().Get("Mcp-Session-Id")
}

func (s *parityStack) session(a testsupport.Actor) string {
	s.t.Helper()
	code, out, sid := s.mcp(a, "", "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "isolation-parity", "version": "1"},
	})
	if code != http.StatusOK || out["error"] != nil || sid == "" {
		s.t.Fatalf("initialize as %s: %d %v", a, code, out)
	}
	return sid
}

// toolResult is one tools/call answer: the text of the first content block
// and whether the tool reported a business error.
type toolResult struct {
	text    string
	isError bool
}

func (s *parityStack) tool(a testsupport.Actor, session, name string, args map[string]any) toolResult {
	s.t.Helper()
	code, out, _ := s.mcp(a, session, "tools/call", map[string]any{"name": name, "arguments": args})
	if code != http.StatusOK || out["error"] != nil {
		s.t.Fatalf("tools/call %s as %s: %d %v", name, a, code, out)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		s.t.Fatalf("tools/call %s: no result: %v", name, out)
	}
	isErr, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	var text string
	if len(content) > 0 {
		if c, ok := content[0].(map[string]any); ok {
			text, _ = c["text"].(string)
		}
	}
	return toolResult{text: text, isError: isErr}
}

// errShape is a refusal's machine code plus its sorted key set.
func errShape(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode error %s: %v", raw, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	code, _ := m["error"].(string)
	return code + " " + strings.Join(keys, ",")
}

// parityCard is the content of an issue card both channels must agree on.
type parityCard struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	Version   int    `json:"version"`
}

func decodeCard(t *testing.T, raw []byte) parityCard {
	t.Helper()
	var c parityCard
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode REST card %s: %v", raw, err)
	}
	return c
}

func decodeShowIssue(t *testing.T, text string) parityCard {
	t.Helper()
	var r struct {
		Issue parityCard `json:"issue"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("decode skein_show %s: %v", text, err)
	}
	return r.Issue
}

func idSet(t *testing.T, raw []byte, key string) []string {
	t.Helper()
	var res map[string]json.RawMessage
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var items []map[string]any
	if err := json.Unmarshal(res["items"], &items); err != nil {
		t.Fatalf("decode items of %s: %v", raw, err)
	}
	var out []string
	for _, it := range items {
		v, _ := it[key].(string)
		if k, ok := it["kind"].(string); ok {
			v = k + ":" + v
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// pageIDs walks a cursor-paginated {items, nextCursor, hasMore} answer to
// the end and returns the sorted row ids.
func pageIDs(t *testing.T, fetch func(cursor string) []byte) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 200 {
			t.Fatalf("list did not end after 200 pages")
		}
		var res struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
			NextCursor string `json:"nextCursor"`
			HasMore    bool   `json:"hasMore"`
		}
		raw := fetch(cursor)
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decode page %s: %v", raw, err)
		}
		for _, it := range res.Items {
			ids = append(ids, it.ID)
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}
	sort.Strings(ids)
	return ids
}

// resolveAnswer is the comparable part of a resolve answer on either channel.
type resolveAnswer struct {
	match      string
	candidates string
}

func decodeResolve(t *testing.T, raw []byte) resolveAnswer {
	t.Helper()
	var r struct {
		Match *struct {
			ID string `json:"id"`
		} `json:"match"`
		Candidates []struct {
			ID string `json:"id"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode resolve %s: %v", raw, err)
	}
	var out resolveAnswer
	if r.Match != nil {
		out.match = r.Match.ID
	}
	var ids []string
	for _, c := range r.Candidates {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	out.candidates = strings.Join(ids, ",")
	return out
}

// projectParity — the MCP half of row 22: the project list and resolution
// by name / prefix / alias answer the same set on both channels, and an N
// project is never the match. The project card has no MCP tool (the agent
// reads a project through the list and resolve), so it has no MCP cell.
func (s *parityStack) projectParity(t *testing.T, a testsupport.Actor, sid string) {
	restIDs := pageIDs(t, func(cursor string) []byte {
		path := "/api/v1/projects?limit=100"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := s.rest(a, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Fatalf("REST /projects: %d %s", code, body)
		}
		return body
	})
	mcpIDs := pageIDs(t, func(cursor string) []byte {
		args := map[string]any{"limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res := s.tool(a, sid, "skein_project_list", args)
		if res.isError {
			t.Fatalf("skein_project_list: %s", res.text)
		}
		return []byte(res.text)
	})
	if len(restIDs) == 0 {
		t.Fatalf("no project on either channel — the comparison would be empty")
	}
	if !slices.Equal(restIDs, mcpIDs) {
		t.Errorf("project list: REST %v, MCP %v", restIDs, mcpIDs)
	}

	for _, p := range s.f.Projects() {
		for via, q := range map[string]string{"name": p.Name, "prefix": p.IDPrefix, "alias": p.Alias} {
			t.Run("resolve/"+p.Key()+"/"+via, func(t *testing.T) {
				code, body := s.rest(a, http.MethodGet, "/api/v1/projects/resolve?q="+url.QueryEscape(q), nil)
				if code != http.StatusOK {
					t.Fatalf("REST resolve: %d %s", code, body)
				}
				res := s.tool(a, sid, "skein_project_resolve", map[string]any{"q": q})
				if res.isError {
					t.Fatalf("skein_project_resolve: %s", res.text)
				}
				rest, mcp := decodeResolve(t, body), decodeResolve(t, []byte(res.text))
				if rest != mcp {
					t.Errorf("resolve: REST %+v, MCP %+v", rest, mcp)
				}
				switch {
				case s.f.Sees(a, p):
					if mcp.match != p.ID.String() {
						t.Errorf("MCP resolve of a visible project matched %q, want %s", mcp.match, p.ID)
					}
				default:
					if mcp.match == p.ID.String() || strings.Contains(mcp.candidates, p.ID.String()) {
						t.Errorf("MCP resolve returned an N project: %+v", mcp)
					}
				}
			})
		}
	}
}

// whoamiParity: skein_whoami lists the projects of the
// same set as REST /projects (the access relation through authz): its
// projectsTotal is that set's size, every listed project is in the set
// (never an N one), and an untruncated list is the whole set. On the old
// semantics the rule whoami used before agrees with the relation, so this
// cell is the guard for T017, when the relation's body changes.
func (s *parityStack) whoamiParity(t *testing.T, a testsupport.Actor, sid string) {
	restIDs := pageIDs(t, func(cursor string) []byte {
		path := "/api/v1/projects?limit=100"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := s.rest(a, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Fatalf("REST /projects: %d %s", code, body)
		}
		return body
	})
	res := s.tool(a, sid, "skein_whoami", map[string]any{})
	if res.isError {
		t.Fatalf("skein_whoami: %s", res.text)
	}
	var who struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
		Total     int  `json:"projectsTotal"`
		Truncated bool `json:"projectsTruncated"`
	}
	if err := json.Unmarshal([]byte(res.text), &who); err != nil {
		t.Fatalf("decode whoami: %v\n%s", err, res.text)
	}
	if len(restIDs) == 0 {
		t.Fatalf("no project on REST — the comparison would be empty")
	}
	if who.Total != len(restIDs) {
		t.Errorf("whoami projectsTotal %d, REST /projects %d", who.Total, len(restIDs))
	}
	var ids []string
	for _, p := range who.Projects {
		ids = append(ids, p.ID)
		if !slices.Contains(restIDs, p.ID) {
			t.Errorf("whoami lists %s, which REST /projects does not show", p.ID)
		}
	}
	if who.Truncated != (who.Total > len(ids)) {
		t.Errorf("projectsTruncated %v with %d of %d listed", who.Truncated, len(ids), who.Total)
	}
	if !who.Truncated {
		sort.Strings(ids)
		if !slices.Equal(ids, restIDs) {
			t.Errorf("untruncated whoami %v, REST %v", ids, restIDs)
		}
	}
}

func TestIsolationParity_RESTEqualsMCP(t *testing.T) {
	s := newParityStack(t)
	missingIssue := "isomissing-1"

	for _, a := range testsupport.TenancyActors {
		t.Run(string(a), func(t *testing.T) {
			if s.f.User(t, a).Deactivated {
				code, _, _ := s.mcp(a, "", "initialize", map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"clientInfo":      map[string]any{"name": "isolation-parity", "version": "1"},
				})
				if code != http.StatusUnauthorized {
					t.Errorf("deactivated actor's MCP call: %d, want 401", code)
				}
				// REST (T036, K-16): the session of a deactivated user is
				// refused on every path of the production router.
				for _, path := range []string{"/api/v1/projects", "/api/v1/issues?limit=5", "/api/v1/search?q=" + url.QueryEscape(s.word)} {
					code, body := s.rest(a, http.MethodGet, path, nil)
					if code != http.StatusUnauthorized || !strings.Contains(string(body), auth.CodeSessionRevoked) {
						t.Errorf("deactivated actor's REST %s: %d %s, want 401 %s", path, code, body, auth.CodeSessionRevoked)
					}
				}
				return
			}
			sid := s.session(a)

			t.Run("list", func(t *testing.T) {
				code, rest := s.rest(a, http.MethodGet, "/api/v1/issues?limit=500&q="+url.QueryEscape(s.word), nil)
				if code != http.StatusOK {
					t.Fatalf("REST /issues: %d %s", code, rest)
				}
				res := s.tool(a, sid, "skein_list", map[string]any{"crossProject": true, "q": s.word, "limit": 100})
				if res.isError {
					t.Fatalf("skein_list: %s", res.text)
				}
				restIDs, mcpIDs := idSet(t, rest, "id"), idSet(t, []byte(res.text), "id")
				if len(restIDs) == 0 {
					t.Fatalf("no fixture issue on either channel — the comparison would be empty")
				}
				if !slices.Equal(restIDs, mcpIDs) {
					t.Errorf("list: REST %v, MCP %v", restIDs, mcpIDs)
				}
			})

			t.Run("search", func(t *testing.T) {
				code, rest := s.rest(a, http.MethodGet, "/api/v1/search?limit=100&q="+url.QueryEscape(s.word), nil)
				if code != http.StatusOK {
					t.Fatalf("REST /search: %d %s", code, rest)
				}
				res := s.tool(a, sid, "skein_search", map[string]any{"query": s.word, "limit": 100})
				if res.isError {
					t.Fatalf("skein_search: %s", res.text)
				}
				restIDs, mcpIDs := idSet(t, rest, "id"), idSet(t, []byte(res.text), "id")
				if len(restIDs) == 0 {
					t.Fatalf("no fixture hit on either channel — the comparison would be empty")
				}
				if !slices.Equal(restIDs, mcpIDs) {
					t.Errorf("search: REST %v, MCP %v", restIDs, mcpIDs)
				}
			})

			t.Run("project", func(t *testing.T) { s.projectParity(t, a, sid) })
			t.Run("whoami", func(t *testing.T) { s.whoamiParity(t, a, sid) })

			missingShow := s.tool(a, sid, "skein_show", map[string]any{"id": missingIssue})
			missingMint := s.tool(a, sid, "skein_get_monitor_script", map[string]any{"id": missingIssue})
			missingCode, missingCard := s.rest(a, http.MethodGet, "/api/v1/projects/"+uuid.NewString()+"/issues/"+missingIssue+"/", nil)

			for _, p := range s.f.Projects() {
				t.Run("show/"+p.IssueID, func(t *testing.T) {
					code, card := s.rest(a, http.MethodGet, "/api/v1/projects/"+p.ID.String()+"/issues/"+p.IssueID+"/", nil)
					show := s.tool(a, sid, "skein_show", map[string]any{"id": p.IssueID})
					restOK := code == http.StatusOK
					if restOK == show.isError {
						t.Fatalf("channels disagree: REST %d, MCP isError=%v (%s)", code, show.isError, show.text)
					}
					if restOK {
						// Both serve: the same issue, the same card.
						rc, mc := decodeCard(t, card), decodeShowIssue(t, show.text)
						if rc.ID != p.IssueID {
							t.Errorf("REST card is %q, want the requested %s", rc.ID, p.IssueID)
						}
						if rc != mc {
							t.Errorf("cards differ: REST %+v, MCP %+v", rc, mc)
						}
						return
					}
					// Both refuse: each exactly as for a nonexistent issue —
					// status, machine code and key set within the channel.
					// The two channels' codes are not compared with each other:
					// each channel has its own vocabulary, and row 31's property
					// is that both refuse the same set.
					if got, want := errShape(t, []byte(show.text)), errShape(t, []byte(missingShow.text)); got != want {
						t.Errorf("skein_show of an N issue: %s, nonexistent %s", got, want)
					}
					if code != missingCode {
						t.Errorf("REST card of an N issue: status %d, nonexistent %d", code, missingCode)
					}
					if got, want := errShape(t, card), errShape(t, missingCard); got != want {
						t.Errorf("REST card of an N issue: %s, nonexistent %s", got, want)
					}
					// Row 28: no monitor credential for an N issue, and the
					// refusal is the nonexistent one.
					mint := s.tool(a, sid, "skein_get_monitor_script", map[string]any{"id": p.IssueID})
					if !mint.isError {
						t.Fatalf("monitor credential minted for an N issue")
					}
					if got, want := errShape(t, []byte(mint.text)), errShape(t, []byte(missingMint.text)); got != want {
						t.Errorf("monitor mint for an N issue: %q, nonexistent %q", got, want)
					}
					var n int
					if err := s.pool.QueryRow(context.Background(),
						`SELECT count(*) FROM monitor_credentials WHERE scope_issue_id = $1`, p.IssueID).Scan(&n); err != nil {
						t.Fatalf("count credentials: %v", err)
					}
					if n != 0 {
						t.Errorf("%d monitor credentials exist for an N issue", n)
					}
				})
			}
		})
	}
}

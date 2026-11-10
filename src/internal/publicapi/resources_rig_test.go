package publicapi_test

// resources_rig_test.go — the two channels of the parity tests of the
// public resources (sk-878.3.1 T022–T028): the product route table of the
// public API behind the real request chain, and the MCP server with the
// same tools the product registers, behind the same API key authentication
// the product puts in front of /mcp. A request on either channel is made
// with an API key of a seeded user, so both run as that key: same owner,
// same grant, same scope.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/idempotency"
	"skein-api/internal/issues"
	mcppkg "skein-api/internal/mcp"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/publicapi"
	"skein-api/internal/search"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// resourceRig is the two channels over one database.
type resourceRig struct {
	t        *testing.T
	pool     *pgxpool.Pool
	restH    http.Handler
	mcpH     http.Handler
	sessions map[string]string
}

// newResourceRig builds both channels over pool, with the stores the
// product builds and the configuration of the test stack.
func newResourceRig(t *testing.T, pool *pgxpool.Pool) *resourceRig {
	t.Helper()
	projectsStore := projects.NewStore(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	searchSvc, err := search.NewService(search.NewStore(pool), nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	chain := publicapi.NewChain(publicapi.ChainOptions{
		Authenticator: auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost),
		Executors:     publicapi.PoolExecutors{Pool: pool},
		ClientIP:      publicapi.NewClientIP(nil),
		WarmKeys:      publicapi.NewWarmKeys(15*time.Minute, 100, time.Now),
		AuthFailures:  publicapi.NewWindowLimiter(100000, time.Minute, 100, time.Now),
		Requests:      publicapi.NewWindowLimiter(100000, time.Minute, 100, time.Now),
	})
	doc, err := publicapi.NewOpenAPIDocument("")
	if err != nil {
		t.Fatalf("NewOpenAPIDocument: %v", err)
	}
	rest := publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second, Chain: chain,
		Versions: publicapi.IssueVersions{Pool: pool, Issues: issuesStore}, Idempotency: idempotency.NewMiddleware(pool, 30*time.Second)},
		publicapi.ProductRoutes(publicapi.Deps{
			Doc: doc, Pool: pool, Projects: projectsStore, Users: usersStore,
			DefaultVisibility: spaces.VisibilityMembers,
			Issues:            issuesStore, Dependencies: depsStore, Search: searchSvc,
		}))
	mcpHandler, _ := mcppkg.Setup(pool, 0, func(s *server.MCPServer) {
		tools.RegisterWhoami(s, pool)
		tools.RegisterProjectList(s, projectsStore, nil)
		tools.RegisterProjectResolve(s, projectsStore)
		tools.RegisterProjectCreate(s, pool, projectsStore, spaces.VisibilityMembers)
		tools.RegisterProjectUpdate(s, pool, projectsStore)
		tools.RegisterProjectMembers(s, pool, projectsStore, usersStore)
		tools.RegisterComponent(s, pool, projectsStore)
		tools.RegisterShow(s, pool, issuesStore, depsStore)
		tools.RegisterList(s, pool, issuesStore, projectsStore, nil)
		tools.RegisterReady(s, pool, depsStore, projectsStore)
		tools.RegisterSearch(s, pool, searchSvc, projectsStore)
		tools.RegisterMove(s, pool, issuesStore, projectsStore)
		tools.RegisterCreate(s, pool, issuesStore, depsStore, projectsStore)
		tools.RegisterEpicCreate(s, pool, issuesStore, projectsStore)
		tools.RegisterUpdate(s, pool, issuesStore)
		tools.RegisterClaim(s, pool, issuesStore)
		tools.RegisterUnclaim(s, pool, issuesStore)
		tools.RegisterClose(s, pool, issuesStore)
		tools.RegisterAccept(s, pool, issuesStore)
		tools.RegisterReparent(s, pool, depsStore, issuesStore)
		tools.RegisterDropDelegated(s, pool, issuesStore)
		tools.RegisterResumeDelegated(s, pool, issuesStore)
		tools.RegisterCommitsAttach(s, pool, issuesStore)
		tools.RegisterCommitsDetach(s, pool, issuesStore)
	})
	return &resourceRig{
		t: t, pool: pool, restH: rest,
		mcpH:     auth.RequireAPIKey(pool, auth.MinBcryptCost, "")(mcpHandler),
		sessions: map[string]string{},
	}
}

// restCall is one answer of the public API.
type restCall struct {
	status int
	header http.Header
	body   []byte
}

// json decodes the answer into a map.
func (c restCall) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.body, &m); err != nil {
		t.Fatalf("answer %d is not a JSON object: %v (%s)", c.status, err, c.body)
	}
	return m
}

// code is the code member of a problem answer, "" for any other answer.
func (c restCall) code() string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(c.body, &p)
	return p.Code
}

// rest makes a request to the public API with key. body nil sends none;
// a string is sent as is; anything else as JSON.
func (rg *resourceRig) rest(key, method, path string, body any, header map[string]string) restCall {
	rg.t.Helper()
	var rd io.Reader = http.NoBody
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			rg.t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set(auth.HeaderAPIKey, key)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	rg.restH.ServeHTTP(rec, req)
	return restCall{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// toolAnswer is one tools/call answer: the text of the result and the code
// of a tool error ("" on success).
type toolAnswer struct {
	text string
	code string
}

// json decodes the text of a successful answer.
func (a toolAnswer) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(a.text), &m); err != nil {
		t.Fatalf("tool answer is not a JSON object: %v (%s)", err, a.text)
	}
	return m
}

// rpc posts one JSON-RPC message to /mcp with key.
func (rg *resourceRig) rpc(key, session, method string, params any) (map[string]any, string) {
	rg.t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		rg.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(auth.HeaderAPIKey, key)
	req.Header.Set(auth.HeaderSurface, auth.SurfaceInternal)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	rec := httptest.NewRecorder()
	rg.mcpH.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		rg.t.Fatalf("%s: MCP answered %d: %s", method, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		rg.t.Fatalf("%s: MCP answer is not JSON: %v (%s)", method, err, rec.Body.String())
	}
	return out, rec.Header().Get("Mcp-Session-Id")
}

// tool calls an MCP tool with key, in a session of that key.
func (rg *resourceRig) tool(key, name string, args map[string]any) toolAnswer {
	rg.t.Helper()
	session, ok := rg.sessions[key]
	if !ok {
		out, sid := rg.rpc(key, "", "initialize", map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "public-api-parity", "version": "1"},
		})
		if out["error"] != nil || sid == "" {
			rg.t.Fatalf("initialize: %v", out)
		}
		session = sid
		rg.sessions[key] = sid
	}
	out, _ := rg.rpc(key, session, "tools/call", map[string]any{"name": name, "arguments": args})
	if out["error"] != nil {
		rg.t.Fatalf("tools/call %s: %v", name, out["error"])
	}
	result, _ := out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	var text string
	if len(content) > 0 {
		if c, ok := content[0].(map[string]any); ok {
			text, _ = c["text"].(string)
		}
	}
	a := toolAnswer{text: text}
	if isErr, _ := result["isError"].(bool); isErr {
		var e struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &e); err != nil || e.Error == "" {
			rg.t.Fatalf("tools/call %s: an error without a code: %s", name, text)
		}
		a.code = e.Error
	}
	return a
}

// rigKey mints a key of user with grant; it and its executors go with the
// test.
func rigKey(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, g apikeys.Grant) string {
	t.Helper()
	plain, _ := mintKey(t, pool, user, g)
	return plain
}

// lastEventID is the id of the latest row of events, the baseline of
// eventsSince.
func lastEventID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(max(id), 0) FROM events`).Scan(&id); err != nil {
		t.Fatalf("read the last event: %v", err)
	}
	return id
}

// eventsSince lists "type/actor_type" of the events of project written
// after the event id since, in order.
func eventsSince(t *testing.T, pool *pgxpool.Pool, project uuid.UUID, since int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT type, actor_type FROM events WHERE project_id = $1 AND id > $2 ORDER BY id`, project, since)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var typ, actor string
		if err := rows.Scan(&typ, &actor); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, typ+"/"+actor)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return out
}

// seedOwnSpace is a regular space owned by owner, owner a participant.
func seedOwnSpace(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := testsupport.SeedSpace(t, pool, owner)
	var short string
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&short); err != nil {
		t.Fatalf("read the short id of a space: %v", err)
	}
	return id, short
}

// projectCleanup removes a project created through an API, with what hangs
// on it, after the test.
func projectCleanup(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM events WHERE project_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	})
}

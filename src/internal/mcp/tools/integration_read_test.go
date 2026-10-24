package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/search"
)

// TestIntegration_ReadToolsViaInProcessClient is a registration smoke
// test against the in-process mcp-go server. It verifies tool
// registration, schema-handler wiring, and the JSON wire shape for
// each read tool. The full ctx-propagation seam (mcppkg.Setup +
// WithHTTPContextFunc + wrapAgentRegistry) is exercised by the
// higher-level skein-web integration tests; this test intentionally
// injects auth values directly to keep DB churn low.
//
// Skips when TEST_DATABASE_URL is unset.
func TestIntegration_ReadToolsViaInProcessClient(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "intg")
	for i := 1; i <= 2; i++ {
		seedIssue(t, pool, pid, userID, prefix+"-"+strconv.Itoa(i),
			"alpha bravo "+strconv.Itoa(i), "open", 2)
	}

	// Build all stores + services the tools need.
	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	searchStore := search.NewStore(pool)
	searchSvc, err := search.NewService(searchStore, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}

	// Build the MCP server identically to mcppkg.Setup but without the
	// HTTP transport — we'll use the in-process client.
	s := server.NewMCPServer("skein-test", "0.1.0",
		server.WithToolCapabilities(false))
	RegisterShow(s, pool, issuesStore, depsStore)
	RegisterList(s, pool, issuesStore, pStore, nil)
	RegisterSearch(s, pool, searchSvc, pStore)
	RegisterReady(s, pool, depsStore, pStore)
	RegisterProjectList(s, pStore, nil)
	RegisterProjectResolve(s, pStore)
	RegisterComponent(s, pool, pStore)
	RegisterPrime(s, pool, issuesStore, depsStore, pStore)
	// sk-242.1 US-4/7 — comment get by id.
	RegisterCommentGet(s, pool, issuesStore)

	// Wrap so the tool handlers see the same auth ctx the production
	// middleware injects. The in-process client bypasses
	// WithHTTPContextFunc; we provide a context func that mirrors what
	// the http path does.
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	defer c.Close()

	// Background context augmented with auth values — every CallTool
	// uses this so RequireUser succeeds. We also inject an Mcp-Session-Id
	// the way auth.middleware does from the header: skein_prime elides the
	// `agent` block (json:"agent,omitempty") when SessionIDFrom(ctx) is
	// empty, and the assertion below requires that block. The in-process
	// client bypasses the HTTP middleware that would normally populate
	// this, so the test stands in for it. The value is an opaque echo key
	// (prime echoes it verbatim, no agents-table row required).
	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, "admin")
	ctx = auth.WithSessionID(ctx, "itest-session-"+pid.String())

	if err := c.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "tests", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// tools/list — every tool we registered should appear.
	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	wantNames := map[string]bool{
		"skein_show":            false,
		"skein_list":            false,
		"skein_search":          false,
		"skein_ready":           false,
		"skein_project_list":    false,
		"skein_project_resolve": false,
		"skein_component":       false,
		"skein_prime":           false,
		"skein_comment_get":     false,
	}
	for _, tool := range listRes.Tools {
		if _, ok := wantNames[tool.Name]; ok {
			wantNames[tool.Name] = true
		}
	}
	for n, found := range wantNames {
		if !found {
			t.Errorf("tools/list missing %q", n)
		}
	}

	// Each tool gets a smoke call. We check IsError and the absence of
	// transport-level error — wire-shape assertions live in the per-
	// tool unit tests above.
	calls := []struct {
		name string
		args map[string]any
	}{
		{"skein_project_list", map[string]any{}},
		{"skein_project_resolve", map[string]any{"q": "no-such-thing-xyz"}},
		{"skein_component", map[string]any{"action": "list", "project": pid.String()}},
		{"skein_list", map[string]any{"project": pid.String()}},
		{"skein_ready", map[string]any{"project": pid.String()}},
		{"skein_search", map[string]any{"query": "alpha"}},
		{"skein_show", map[string]any{"id": prefix + "-1"}},
		{"skein_prime", map[string]any{"project": pid.String()}},
		// Missing comment → comment_not_found is a business error with
		// IsError=true; smoke only tools that succeed. Schema presence
		// is asserted via wantNames above.
	}
	for _, tc := range calls {
		toolName := tc.name
		req := mcpgo.CallToolRequest{}
		req.Params.Name = toolName
		req.Params.Arguments = tc.args
		got, err := c.CallTool(ctx, req)
		if err != nil {
			t.Errorf("CallTool(%s): transport error: %v", toolName, err)
			continue
		}
		if got == nil {
			t.Errorf("CallTool(%s): nil result", toolName)
			continue
		}
		if got.IsError {
			// The smoke calls SHOULD succeed for every tool except the
			// project_resolve "no-such-thing-xyz" case, which still
			// returns IsError=false (it returns match:null + empty
			// candidates as a successful resolve).
			//
			// If we see IsError=true here, surface the body so the test
			// failure is actionable.
			if len(got.Content) > 0 {
				if txt, ok := got.Content[0].(mcpgo.TextContent); ok {
					t.Errorf("CallTool(%s) IsError=true: %s", toolName, txt.Text)
					continue
				}
			}
			t.Errorf("CallTool(%s) IsError=true with no body", toolName)
			continue
		}

		// Defense-in-depth: skein_prime is the agent's session-priming
		// surface and its wire shape is a stable contract. Assert that
		// every documented top-level section is present on the response
		// — a future refactor of ToolJSON or the response struct that
		// drops a section silently would otherwise only get caught by
		// the unit tests in prime_test.go and not by the in-process
		// MCP plumbing exercised here.
		if toolName == "skein_prime" {
			if len(got.Content) == 0 {
				t.Errorf("CallTool(skein_prime): empty content")
				continue
			}
			txt, ok := got.Content[0].(mcpgo.TextContent)
			if !ok {
				t.Errorf("CallTool(skein_prime): content[0] not TextContent (%T)", got.Content[0])
				continue
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(txt.Text), &body); err != nil {
				t.Errorf("CallTool(skein_prime): unmarshal body: %v\n%s", err, txt.Text)
				continue
			}
			for _, key := range []string{
				"user", "agent", "scope",
				"myInProgress", "readyForMe", "blocked",
				"recentActivity", "summary",
			} {
				if _, present := body[key]; !present {
					t.Errorf("CallTool(skein_prime): response missing %q section", key)
				}
			}
		}
	}
}

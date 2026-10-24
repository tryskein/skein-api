package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// metaSearchHint mirrors the _meta key stamped by
// internal/mcp/tools.addTool. Duplicated as a literal on purpose: this
// test asserts the wire contract Claude Code reads, so it must not
// depend on the constant it is checking.
const metaSearchHint = "anthropic/searchHint"

// TestSearchHints_EveryTool is the guard that keeps tool discovery
// working as the surface grows. With server-side alwaysLoad off, a
// deferred tool reaches the agent only if Tool Search ranks it, and the
// hint is the highest-weighted signal in that ranking — so a tool
// registered without one is a tool that is hard to find at all.
//
// The list is taken from the production registration closure, not from
// a hand-maintained copy, so adding a Register* call to mcpRegisterToolsFn
// without adding a hint to the tools.searchHints table fails here.
func TestSearchHints_EveryTool(t *testing.T) {
	tools := listRegisteredTools(t)
	if len(tools) == 0 {
		t.Fatalf("no tools registered — registration wiring broke")
	}

	// Hints must also be distinct: two tools sharing a hint are two
	// tools the search cannot tell apart, which is the failure mode the
	// hint exists to prevent.
	seen := make(map[string]string, len(tools))
	for i := range tools {
		tool := tools[i]
		if tool.Meta == nil {
			t.Errorf("%s: no _meta — %s is required on every tool", tool.Name, metaSearchHint)
			continue
		}
		hint, _ := tool.Meta.AdditionalFields[metaSearchHint].(string)
		if hint == "" {
			t.Errorf("%s: %s missing or empty — add an entry to tools.searchHints",
				tool.Name, metaSearchHint)
			continue
		}
		if prev, dup := seen[hint]; dup {
			t.Errorf("%s: %s %q duplicates %s — search cannot separate them",
				tool.Name, metaSearchHint, hint, prev)
			continue
		}
		seen[hint] = tool.Name
	}
}

// TestAlwaysLoad_BootstrapTools pins the set of tools that bypass
// deferred loading. The server `instructions` order a three-step
// bootstrap (whoami → echo sessionId → get_instructions) and prime is
// the first thing a working session calls after it; each of those must
// be in the prompt already, or the agent burns a Tool Search round-trip
// before it can obey its own startup rules. Nothing else earns the
// context: every added name here is schema bytes every conversation
// pays for.
func TestAlwaysLoad_BootstrapTools(t *testing.T) {
	want := map[string]bool{
		"skein_whoami":           true,
		"skein_get_instructions": true,
		"skein_prime":            true,
	}

	tools := listRegisteredTools(t)
	for i := range tools {
		tool := tools[i]
		var got bool
		if tool.Meta != nil {
			got, _ = tool.Meta.AdditionalFields["anthropic/alwaysLoad"].(bool)
		}
		if got != want[tool.Name] {
			t.Errorf("%s: anthropic/alwaysLoad = %v, want %v", tool.Name, got, want[tool.Name])
		}
	}
}

// TestActionEnum_MatchesDispatchAllowlist is the cross-cutting guard for
// consolidated tools (sk-255.3): the `enum` an agent reads in tools/list
// and the allowlist the dispatcher actually enforces must be the same
// list, in the same order.
//
// Why it has to be a test and not a code review note: the two live in
// different places by necessity — the enum is baked into the tool
// descriptor, the allowlist is the handler map bound at registration —
// and a drift between them is silent in the worst possible direction. An
// action present in the enum but not in the map is a tool advertising an
// operation that always answers "unknown action"; the reverse is an
// operation no agent can discover. Neither breaks a build, neither breaks
// any behavioural test, and both are exactly the mistake a mechanical
// wave-by-wave consolidation makes.
//
// The check goes through the wire in both directions: the enum is read
// off the tools/list payload, and the allowlist is obtained by CALLING
// the tool with a deliberately impossible action and reading
// allowedActions out of the validation_error. That is the same pair of
// artefacts an agent sees, so nothing here can pass by consulting a
// shared constant that is itself the bug.
//
// It also runs against a zero-valued mcpDeps: the dispatcher must reject
// an unknown action before it reaches any handler (and therefore before
// it touches a nil pool), which is a property worth pinning on its own.
func TestActionEnum_MatchesDispatchAllowlist(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	mcpRegisterToolsFn(mcpDeps{})(s)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	consolidated := 0
	for i := range listRes.Tools {
		tool := listRes.Tools[i]
		schemaEnum, ok := actionEnum(tool)
		if !ok {
			continue
		}
		consolidated++

		t.Run(tool.Name, func(t *testing.T) {
			if len(schemaEnum) == 0 {
				t.Fatalf("`action` is declared without an enum — agents have nothing to pick from")
			}
			// Required: an optional discriminator is a tool that can be
			// called with no action at all and looks valid to the client.
			if !slices.Contains(tool.InputSchema.Required, "action") {
				t.Errorf("`action` is not in required%v", tool.InputSchema.Required)
			}

			callReq := mcpgo.CallToolRequest{}
			callReq.Params.Name = tool.Name
			callReq.Params.Arguments = map[string]any{
				"action": "__definitely_not_an_action__",
			}
			res, err := c.CallTool(ctx, callReq)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("an impossible action was accepted: %+v", res)
			}

			body := errorEnvelope(t, res)
			if body["error"] != "validation_error" {
				t.Errorf("error = %v, want \"validation_error\"", body["error"])
			}
			raw, ok := body["allowedActions"].([]any)
			if !ok {
				t.Fatalf("the rejection carries no allowedActions (%#v) — "+
					"an agent that guessed wrong cannot recover without re-running Tool Search",
					body["allowedActions"])
			}
			allowed := make([]string, 0, len(raw))
			for _, v := range raw {
				s, _ := v.(string)
				allowed = append(allowed, s)
			}
			if !slices.Equal(allowed, schemaEnum) {
				t.Errorf("enum and dispatch allowlist disagree:\n  schema enum: %v\n  dispatcher:  %v",
					schemaEnum, allowed)
			}
		})
	}

	// Non-vacuity: this guard exists because consolidated tools exist. If
	// the registration ever stops producing any, that is either a
	// regression or a rename — and silently passing is how the guard
	// would rot.
	if consolidated == 0 {
		t.Error("no tool declares an `action` parameter — the guard checked nothing")
	}
}

// actionEnum returns the declared enum of a tool's `action` parameter.
func actionEnum(tool mcpgo.Tool) ([]string, bool) {
	prop, ok := tool.InputSchema.Properties["action"]
	if !ok {
		return nil, false
	}
	m, ok := prop.(map[string]any)
	if !ok {
		return nil, true
	}
	raw, ok := m["enum"].([]any)
	if !ok {
		return nil, true
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out, true
}

// errorEnvelope decodes the JSON error envelope out of a tool result.
func errorEnvelope(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	for _, content := range res.Content {
		text, ok := content.(mcpgo.TextContent)
		if !ok {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
			continue
		}
		return body
	}
	t.Fatalf("no JSON envelope in result %+v", res)
	return nil
}

// listRegisteredTools wires the production tool set into an MCP server
// and reads it back through an in-process client, so the assertions run
// against the tools/list payload a real client receives.
//
// mcpDeps is zero-valued: every Register* function only builds the tool
// descriptor and closes over its dependencies, so no store is touched
// at registration time.
func listRegisteredTools(t *testing.T) []mcpgo.Tool {
	t.Helper()

	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	mcpRegisterToolsFn(mcpDeps{})(s)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return listRes.Tools
}

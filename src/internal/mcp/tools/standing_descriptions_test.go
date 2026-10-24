package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestStandingToolDescriptions asserts the sk-118 standing contract is
// surfaced in the registered MCP tool descriptions / parameter schemas, so
// the agent-facing documentation can't silently drift from the implemented
// behaviour. Registration only builds the tool schema (handlers are never
// invoked here), so passing nil stores/pool is safe — no DB required.
func TestStandingToolDescriptions(t *testing.T) {
	s := server.NewMCPServer("skein-desc-test", "0.1.0",
		server.WithToolCapabilities(false))

	RegisterCreate(s, nil, nil, nil, nil)
	RegisterUpdate(s, nil, nil)
	RegisterClaim(s, nil, nil)
	RegisterClose(s, nil, nil)
	RegisterList(s, nil, nil, nil, nil)
	RegisterSearch(s, nil, nil, nil)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "tests", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := make(map[string]mcpgo.Tool, len(listRes.Tools))
	for _, tl := range listRes.Tools {
		tools[tl.Name] = tl
	}

	// schemaString renders a tool's description + its full input schema into
	// one searchable blob (param descriptions + enum values live in the
	// schema, not in tool.Description).
	schemaString := func(name string) string {
		tl, ok := tools[name]
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		raw, err := tl.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal tool %q: %v", name, err)
		}
		return string(raw)
	}

	cases := []struct {
		tool  string
		wants []string
	}{
		// skein_create exposes the boolean `standing` flag + the nested ban.
		{"skein_create", []string{"standing", "nested_standing_forbidden"}},
		// skein_update documents promote/demote + the rejected legs/codes.
		{"skein_update", []string{
			"standing",
			"cannot_promote_locked_to_standing",
			"cannot_work_standing_directly",
			"cannot_defer_standing",
			"nested_standing_forbidden",
		}},
		// skein_claim documents the claim rejection.
		{"skein_claim", []string{"cannot_claim_standing"}},
		// skein_close documents the retire-block.
		{"skein_close", []string{"standing_has_open_children"}},
		// skein_list / skein_search carry standing in the statuses enum.
		{"skein_list", []string{"standing"}},
		{"skein_search", []string{"standing"}},
	}

	for _, tc := range cases {
		blob := schemaString(tc.tool)
		for _, w := range tc.wants {
			if !strings.Contains(blob, w) {
				t.Errorf("%s description/schema missing %q", tc.tool, w)
			}
		}
	}

	// The standing status MUST be an accepted enum value on the writable /
	// filterable status surfaces, not just mentioned in prose.
	for _, tool := range []string{"skein_update", "skein_list", "skein_search"} {
		blob := schemaString(tool)
		if !strings.Contains(blob, `"standing"`) {
			t.Errorf("%s status enum does not include the quoted \"standing\" value", tool)
		}
	}
}

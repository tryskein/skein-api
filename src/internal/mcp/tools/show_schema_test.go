package tools

// show_schema_test.go — INV-5 for skein_show, checked against the BINDING
// rather than against prose.
//
// It catches a schema whose declared default the tool itself refuses: it
// takes every default the SCHEMA declares, materialises
// them the way an MCP client does, and requires the tool's own input
// binding to accept the result.
//
// The defect it is written for: includeDeps declared DefaultBool(true)
// while resolveDepsRequest refuses includeDeps:true without depsTypes.
// Any client that fills in declared defaults would have sent that on every
// call and broken every skein_show.

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// showToolSchema returns skein_show exactly as tools/list renders it.
// Registration builds descriptors only — the nil stores are never touched
// because no handler is invoked.
func showToolSchema(t *testing.T) mcpgo.Tool {
	t.Helper()

	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterShow(s, nil, nil, nil)

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
	res, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for i := range res.Tools {
		if res.Tools[i].Name == "skein_show" {
			return res.Tools[i]
		}
	}
	t.Fatalf("skein_show is not in the listing (%d tools) — registration wiring broke", len(res.Tools))
	return mcpgo.Tool{}
}

// TestShowSchema_DeclaredDefaultsAreAcceptedByTheBinding materialises the
// declared defaults and runs them through the dependency binding.
func TestShowSchema_DeclaredDefaultsAreAcceptedByTheBinding(t *testing.T) {
	tool := showToolSchema(t)

	args := map[string]any{"id": "sk-1"}
	declared := make([]string, 0, len(tool.InputSchema.Properties))
	for name, raw := range tool.InputSchema.Properties {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if v, present := prop["default"]; present {
			args[name] = v
			declared = append(declared, name)
		}
	}
	if len(declared) == 0 {
		t.Fatal("skein_show declares no default at all — either registration broke or the " +
			"schema stopped publishing them, and this guard is vacuous")
	}

	if _, err := resolveDepsRequest(args); err != nil {
		t.Errorf("materialising skein_show's declared defaults %v produces a REFUSAL from the "+
			"binding: %v (field %q). A default is what a client sends when the caller says "+
			"nothing — declaring one the binding rejects breaks every call from any client "+
			"that fills defaults in. Either the binding must accept it, or the parameter must "+
			"declare no default and describe what omitting it means.",
			declared, err.Message, err.Field)
	}
}

// TestShowSchema_IncludeDepsDeclaresNoDefault pins the specific shape the
// fix took, so a later edit cannot quietly reintroduce the trap by
// declaring a default whose refusal the prose no longer mentions.
func TestShowSchema_IncludeDepsDeclaresNoDefault(t *testing.T) {
	tool := showToolSchema(t)

	prop, ok := tool.InputSchema.Properties["includeDeps"].(map[string]any)
	if !ok {
		t.Fatalf("skein_show has no includeDeps parameter: %#v", tool.InputSchema.Properties["includeDeps"])
	}
	if v, present := prop["default"]; present {
		t.Errorf("includeDeps declares default %v. The effective default of the dependency "+
			"block — counters, no edges — is the ABSENCE of the key: includeDeps:true without "+
			"depsTypes is refused, so no boolean spells it. A declared default here is a value "+
			"the caller must not send.", v)
	}
}

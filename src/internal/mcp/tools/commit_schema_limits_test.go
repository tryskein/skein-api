package tools

// commit_schema_limits_test.go — sk-977.7.69: the url, message and author
// limits of a commit ref (2048, 1000, 256 characters) apply after
// surrounding whitespace is trimmed, so a value at the limit plus a trailing
// space is accepted by the server. The commit item schema that tools/list
// serves must accept it too: a client that validates its arguments by the
// schema would otherwise refuse a call the server takes.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestCommitItemSchema_AcceptsPaddedValuesAtTheLimit(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterCommitsAttach(s, nil, nil)
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
	var raw []byte
	for _, tool := range res.Tools {
		if tool.Name == "skein_commits_attach" {
			raw, err = json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal input schema: %v", err)
			}
		}
	}
	if raw == nil {
		t.Fatal("skein_commits_attach is not in the listing")
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("input schema is not JSON: %v", err)
	}
	commits, _ := parsed.(map[string]any)["properties"].(map[string]any)["commits"].(map[string]any)
	item, _ := commits["items"].(map[string]any)
	props, _ := item["properties"].(map[string]any)
	for _, field := range []string{"url", "message", "author"} {
		if props[field] == nil {
			t.Errorf("commits[].%s is not declared", field)
		}
	}
	const schemaURL = "https://example.invalid/skein_commits_attach.json"
	comp := jsonschema.NewCompiler()
	comp.DefaultDraft(jsonschema.Draft2020)
	if err := comp.AddResource(schemaURL, parsed); err != nil {
		t.Fatalf("add the input schema: %v", err)
	}
	schema, err := comp.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile the input schema: %v", err)
	}
	// A whole arguments object, validated with JSON Schema 2020-12, so the
	// constraints of the enclosing object and array take part.
	args := map[string]any{
		"issueId": "demo-1",
		"commits": []any{map[string]any{
			"hash":    "abcdef1",
			"url":     "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/")) + " ",
			"message": strings.Repeat("m", 1000) + " ",
			"author":  strings.Repeat("a", 256) + " ",
		}},
	}
	if err := schema.Validate(args); err != nil {
		t.Errorf("arguments with commit fields at the limit plus a trailing space are refused by the schema: %v", err)
	}
}

package tools

// commit_schema_limits_test.go — sk-977.7.69: the url, message and author
// limits of a commit ref (2048, 1000, 256 characters) apply after
// surrounding whitespace is trimmed, so a value at the limit plus a trailing
// space is accepted by the server. The commit item schema that tools/list
// serves must accept it too: a client that validates its arguments by the
// schema would otherwise refuse a call the server takes.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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
			raw, err = json.Marshal(tool.InputSchema.Properties["commits"])
			if err != nil {
				t.Fatalf("marshal commits schema: %v", err)
			}
		}
	}
	if raw == nil {
		t.Fatal("skein_commits_attach with a commits property is not in the listing")
	}
	var commits openapi3.Schema
	if err := json.Unmarshal(raw, &commits); err != nil {
		t.Fatalf("commits schema: %v", err)
	}
	if commits.Items == nil || commits.Items.Value == nil {
		t.Fatal("commits declares no item schema")
	}
	item := commits.Items.Value
	for field, value := range map[string]string{
		"url":     "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/")) + " ",
		"message": strings.Repeat("m", 1000) + " ",
		"author":  strings.Repeat("a", 256) + " ",
	} {
		prop := item.Properties[field]
		if prop == nil || prop.Value == nil {
			t.Errorf("%s is not in the commit item schema", field)
			continue
		}
		if err := prop.Value.VisitJSON(value); err != nil {
			t.Errorf("%s of %d characters with a trailing space is refused by the schema: %v", field, len(value), err)
		}
	}
}

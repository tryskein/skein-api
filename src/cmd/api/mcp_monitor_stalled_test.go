package main

// mcp_monitor_stalled_test.go — sk-669: the monitorStalled response layer
// (sk-579 US-5) is switched off by the owner. On the server
// mcpRegisterToolsFn builds, a monitor that the layer would have reported
// (issued by the calling session, never polled, far past the stall
// threshold) leaves no trace in any tool response — neither in a JSON one
// (skein_show) nor in a text one (skein_get_instructions) — and nothing is
// recorded as shown. The middleware itself is still covered by its unit tests
// in internal/mcp/tools.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/testsupport"
)

func TestMCPRegisterTools_NoMonitorStalled(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "mrs")
	root := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "stalled hint", "open", 2)
	_, keyPrefix, hash, err := auth.GenerateAPIKey(4)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	keyID, _, err := apikeys.New(pool).Create(context.Background(), owner, "sk-669", keyPrefix, hash)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	// Runs before the user/project cleanups: the key's credentials cascade.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
	})

	const session = "sess-sk-669"
	_, cred, err := monitorcreds.New(pool, time.Hour).Mint(context.Background(), monitorcreds.MintParams{
		UserID: owner, MintedByAPIKeyID: keyID, Audience: "notifications:poll", Capability: "read",
		ProjectID: project, ScopeIssueID: root, EventTypes: []string{"comment.created"},
		IssuedSessionID: session, TokenFilePath: "/tmp/skein-mon-sk669.token",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Stalled by the sk-579 definition: minted 10 minutes ago, never polled —
	// past the default 3m threshold and past any threshold config.Load allows
	// below 10m.
	if _, err := pool.Exec(context.Background(),
		`UPDATE monitor_credentials SET created_at = now() - interval '10 minutes' WHERE id = $1`, cred.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	deps := routingDeps(pool)
	deps.agentPublicURL = "https://skein-sk669.test"
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	mcpRegisterToolsFn(deps)(s)

	ctx := context.Background()
	ctx = auth.WithUserID(ctx, owner)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithAPIKeyID(ctx, keyID)
	ctx = auth.WithSessionID(ctx, session)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"skein_show", map[string]any{"id": root}},
		{"skein_get_instructions", map[string]any{}},
	} {
		texts := callProduction(t, s, ctx, tc.tool, tc.args)
		// The layer answered a text tool with an extra JSON block.
		if len(texts) != 1 {
			t.Errorf("%s: %d content blocks, want 1: %q", tc.tool, len(texts), texts)
		}
		for _, text := range texts {
			for _, key := range stalledKeys(text) {
				t.Errorf("%s: response carries %q: %s", tc.tool, key, text)
			}
		}
		// Should a show have been recorded, move it past the 5-minute
		// throttle so the next tool meets the monitor as fresh.
		if _, err := pool.Exec(context.Background(),
			`UPDATE monitor_stall_notices SET shown_at = shown_at - interval '6 minutes' WHERE monitor_id = $1`, cred.ID); err != nil {
			t.Fatalf("rewind notice: %v", err)
		}
	}

	// The guide must not describe a field no response carries.
	guide := callProduction(t, s, ctx, "skein_get_instructions", map[string]any{})
	if strings.Contains(strings.Join(guide, "\n"), "monitorStalled") {
		t.Errorf("skein_get_instructions still describes monitorStalled")
	}

	var shown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM monitor_stall_notices WHERE monitor_id = $1`, cred.ID).Scan(&shown); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	if shown != 0 {
		t.Errorf("monitor_stall_notices rows for the monitor = %d, want 0 (nothing is shown)", shown)
	}
}

// callProduction runs one tools/call through HandleMessage and returns the
// text of every content block of a successful result.
func callProduction(t *testing.T, s *server.MCPServer, ctx context.Context, tool string, args map[string]any) []string {
	t.Helper()
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := json.Marshal(s.HandleMessage(ctx, msg))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var resp struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Result == nil {
		t.Fatalf("%s: unexpected response (%v): %s", tool, err, raw)
	}
	if resp.Result.IsError {
		t.Fatalf("%s: isError: %s", tool, raw)
	}
	var texts []string
	for _, c := range resp.Result.Content {
		texts = append(texts, c.Text)
	}
	return texts
}

// stalledKeys returns the hint keys present at the top level of a JSON-object
// block; a block that is not a JSON object has none.
func stalledKeys(text string) []string {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &obj) != nil {
		return nil
	}
	var found []string
	for _, key := range []string{"monitorStalled", "monitorStalledOmitted"} {
		if _, ok := obj[key]; ok {
			found = append(found, key)
		}
	}
	return found
}

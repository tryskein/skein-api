package tools

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// callAction builds a tools/call request for the consolidated tool and
// runs it through the dispatcher only — no server, no auth context. The
// point is which handler the `action` selects, not what that handler
// then does; the handlers' own behaviour is covered by attachment_test.go
// (which calls them directly and was NOT touched by the consolidation —
// that it still compiles and passes is the load-bearing evidence that
// folding six tools into one changed no behaviour).
func callAction(t *testing.T, set actionSet, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "skein_attachments"
	req.Params.Arguments = args
	res, err := set.dispatch("skein_attachments")(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch returned transport error: %v", err)
	}
	if res == nil {
		t.Fatal("dispatch returned nil result")
	}
	return res
}

// envelope decodes the JSON error envelope out of a tool result.
func envelope(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	for _, c := range res.Content {
		text, ok := c.(mcpgo.TextContent)
		if !ok {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
			continue
		}
		return body
	}
	t.Fatalf("no JSON body in result %+v", res)
	return nil
}

// TestAttachmentsDispatch_EachActionReachesItsHandler asserts the routing
// table, and asserts it by identity rather than by behaviour: each action
// is bound to a distinct sentinel handler and the dispatcher must land on
// exactly the one named. A test that only checked "some handler ran"
// would stay green if two actions were swapped in the map literal — which
// is the single most likely typo in a six-entry table.
func TestAttachmentsDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	handlers := make(map[string]server.ToolHandlerFunc, len(attachmentActions.order))
	for _, name := range attachmentActions.order {
		name := name
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			got = name
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := attachmentActions.with(handlers)

	// Every action the production registration binds must be routable.
	want := []string{"upload_url", "confirm", "list", "metadata", "get_url", "delete"}
	if len(want) != len(attachmentActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(attachmentActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callAction(t, set, map[string]any{"action": action})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestAttachmentsDispatch_BadAction covers the two ways an agent gets the
// discriminator wrong. Both must come back as a structured
// validation_error carrying allowedActions — that list is the whole
// compensation for moving the verb out of the tool name, because it lets
// an agent that guessed wrong correct itself in the next turn instead of
// re-running Tool Search.
func TestAttachmentsDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(attachmentActions.order))
	for _, name := range attachmentActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := attachmentActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"empty", map[string]any{"action": ""}},
		{"whitespace", map[string]any{"action": "   "}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_attachment_delete"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callAction(t, set, tc.args)
			if !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			body := envelope(t, res)
			if body["error"] != ErrCodeValidationError {
				t.Errorf("error = %v, want %q", body["error"], ErrCodeValidationError)
			}
			allowed, ok := body["allowedActions"].([]any)
			if !ok {
				t.Fatalf("allowedActions missing or not a list: %#v", body["allowedActions"])
			}
			if len(allowed) != len(attachmentActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(attachmentActions.order))
			}
			for i, v := range allowed {
				if v != attachmentActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, attachmentActions.order[i])
				}
			}
		})
	}
}

// TestActionSetLabel_BoundedByAllowlist is the guard on the one change in
// this consolidation that cannot be walked back in production: the
// `action` Prometheus label. A raw caller string reaching the label mints
// a series per typo and the cardinality never comes down, so anything off
// the allowlist must collapse to a single constant.
func TestActionSetLabel_BoundedByAllowlist(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(attachmentActions.order))
	for _, name := range attachmentActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := attachmentActions.with(handlers)

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "list"}, "list"},
		{map[string]any{"action": " list "}, "list"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": ""}, actionInvalid},
		{map[string]any{"action": "LIST"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"action": "skein_attachment_list"}, actionInvalid},
		{map[string]any{"action": "'; DROP SERIES --"}, actionInvalid},
		{map[string]any{"action": 42}, actionInvalid}, // wrong JSON type
	}
	for _, tc := range cases {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = tc.args
		if got := set.label(req); got != tc.want {
			t.Errorf("label(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestActionSet_WithPanicsOnMismatch pins the registration-time guard.
// The mismatch it catches (an action in the enum with no handler bound)
// would otherwise ship as a tool advertising an action that always
// returns "unknown action" — a defect no agent can work around and no
// existing test would notice.
func TestActionSet_WithPanicsOnMismatch(t *testing.T) {
	noop := func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	cases := map[string]map[string]server.ToolHandlerFunc{
		"missing handler": {"a": noop},
		"extra handler":   {"a": noop, "b": noop, "c": noop},
		"wrong key":       {"a": noop, "typo": noop},
	}
	for name, handlers := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("with(%v) did not panic", handlers)
				}
			}()
			_ = actionSet{order: []string{"a", "b"}}.with(handlers)
		})
	}
}

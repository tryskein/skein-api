package tools

import (
	"context"
	"slices"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestProjectMembersDispatch_EachActionReachesItsHandler pins the routing
// table by identity: each action is bound to a distinct sentinel and must
// land on exactly the one named. Identity matters here because `add` and
// `remove` are opposites guarded by different permission checks — a
// transposed entry in the map literal is "revoked access when asked to
// grant it", and a test that only asserted "no error" would stay green
// through it.
func TestProjectMembersDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := projectMemberActions.with(sentinelHandlers(projectMemberActions, &got))

	want := []string{"list", "add", "remove"}
	if len(want) != len(projectMemberActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(projectMemberActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_project_members", set, map[string]any{
				"action": action, "project": "p",
			})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestProjectMembersDispatch_BadAction: a missing or unknown discriminator
// must come back as a structured validation_error carrying allowedActions.
//
// Note the "project only" case. `project` is required alongside `action`
// in this tool's schema, and it would be easy to assume that a request
// carrying the required-looking half gets further. It must not: the
// dispatcher rejects on `action` before any handler — and therefore before
// any nil store — is touched.
func TestProjectMembersDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(projectMemberActions.order))
	for _, name := range projectMemberActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := projectMemberActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"project only", map[string]any{"project": "skein"}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_project_member_add"}},
		{"leave is not an action", map[string]any{"action": "leave", "project": "skein"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_project_members", set, tc.args)
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
			if len(allowed) != len(projectMemberActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(projectMemberActions.order))
			}
			for i, v := range allowed {
				if v != projectMemberActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, projectMemberActions.order[i])
				}
			}
		})
	}
}

// TestProjectMembersLabel_BoundedByAllowlist guards the `action`
// Prometheus label for this group: anything off the allowlist collapses to
// one constant instead of minting a series from caller-controlled text.
func TestProjectMembersLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := projectMemberActions.with(sentinelHandlers(projectMemberActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "remove"}, "remove"},
		{map[string]any{"action": " add "}, "add"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "List"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"action": "skein_project_member_remove"}, actionInvalid},
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

// TestProjectMembersSchema_ProjectRequiredTargetOptional reads the shape
// decision back off the wire.
//
// `project` is required next to `action` because all three actions need
// it — that is what lets it sit in the plain `required` ARRAY instead of
// needing the top-level oneOf the platform rejects outright. The target
// pair must NOT be required: `userId` and `username` are mutually
// exclusive alternatives enforced in the handler, and list needs neither,
// so marking either required would make a legal call fail client-side
// validation before it ever reaches the server.
func TestProjectMembersSchema_ProjectRequiredTargetOptional(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterProjectMembers(s, nil, nil, nil)

	tool := registeredTool(t, s, "skein_project_members")
	if want := []string{"action", "project"}; !slices.Equal(tool.InputSchema.Required, want) {
		t.Errorf("required = %v, want %v", tool.InputSchema.Required, want)
	}

	props := tool.InputSchema.Properties
	for _, name := range []string{"action", "project", "userId", "username", "role"} {
		if _, ok := props[name]; !ok {
			t.Errorf("skein_project_members has no %q parameter (properties: %v)", name, keysOf(props))
		}
	}

	// `role` keeps its closed enum. Losing it during the fold would be
	// invisible to every handler test (the handler re-checks the value and
	// returns validation_error), but it is what tells the agent which two
	// strings exist without a round trip.
	role, ok := props["role"].(map[string]any)
	if !ok {
		t.Fatalf("`role` is not an object schema: %#v", props["role"])
	}
	enum, _ := role["enum"].([]any)
	wantEnum := []string{"owner", "member"}
	if len(enum) != len(wantEnum) {
		t.Fatalf("role enum = %v, want %v", enum, wantEnum)
	}
	for i, v := range enum {
		if v != wantEnum[i] {
			t.Errorf("role enum[%d] = %v, want %q", i, v, wantEnum[i])
		}
	}
}

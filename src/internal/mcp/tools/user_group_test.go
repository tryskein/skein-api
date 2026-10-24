package tools

import (
	"context"
	"slices"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestUserDispatch_EachActionReachesItsHandler pins the routing table by
// identity: each action is bound to a distinct sentinel and must land on
// exactly the one named. A test that only checked "some handler ran"
// would stay green if `list` and `update` were transposed in the map
// literal — a read swapped for an admin write.
func TestUserDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := userActions.with(sentinelHandlers(userActions, &got))

	// Spelled out rather than derived from userActions.order: this list is
	// the test's independent statement of the surface, so dropping an
	// action from the table fails here instead of silently shrinking what
	// is checked.
	want := []string{"list", "create", "update", "space_add"}
	if len(want) != len(userActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(userActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_user", set, map[string]any{"action": action})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestUserDispatch_BadAction: a missing or unknown discriminator must come
// back as a structured validation_error carrying allowedActions, so an
// agent that guessed wrong can correct itself in the next turn instead of
// re-running Tool Search.
//
// The "old tool name" case is the specific mistake this consolidation
// invites: an agent that learned skein_user_create before the fold will
// reach for that string, and the rejection has to teach it the new
// vocabulary rather than just saying no.
func TestUserDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(userActions.order))
	for _, name := range userActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := userActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"payload without action", map[string]any{"email": "a@b.local", "role": "user"}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_user_create"}},
		{"deactivate is not an action", map[string]any{"action": "deactivate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_user", set, tc.args)
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
			if len(allowed) != len(userActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(userActions.order))
			}
			for i, v := range allowed {
				if v != userActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, userActions.order[i])
				}
			}
		})
	}
}

// TestUserLabel_BoundedByAllowlist guards the `action` Prometheus label
// for this group: anything off the allowlist collapses to one constant
// instead of minting a series from caller-controlled text.
func TestUserLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := userActions.with(sentinelHandlers(userActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "create"}, "create"},
		{map[string]any{"action": " list "}, "list"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "Update"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"action": "skein_user_update"}, actionInvalid},
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

// TestUserSchema_OnlyActionIsRequired states the shape decision of this
// group through the wire, because it is the one that differs from its two
// siblings in this wave. skein_component and skein_project_members both
// put `project` in the `required` array next to `action` — legitimately,
// since every one of their actions needs it. skein_user has no such
// field: `userId` is update-only, `email` is create-only, and list takes
// nothing at all. Marking any of them required would make two of the
// three actions uncallable through a client that validates the schema.
//
// The test also pins that the parameters those actions actually need are
// still declared — a schema that is "not required" by being absent would
// pass a naive required-check.
func TestUserSchema_OnlyActionIsRequired(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterUser(s, nil, nil, nil)

	tool := registeredTool(t, s, "skein_user")
	if want := []string{"action"}; !slices.Equal(tool.InputSchema.Required, want) {
		t.Errorf("required = %v, want %v — no field is needed by all three actions",
			tool.InputSchema.Required, want)
	}

	props := tool.InputSchema.Properties
	for _, name := range []string{
		"action", "email", "username", "name", "password", "role",
		"is_external", "userId", "deactivated", "archived", "search",
		"limit", "cursor",
	} {
		if _, ok := props[name]; !ok {
			t.Errorf("skein_user has no %q parameter (properties: %v)", name, keysOf(props))
		}
	}

	// `deactivated` must stay a plain boolean with NO default. The handler
	// reads it presence-based (GetArguments()["deactivated"]) precisely so
	// that "absent" and "false" are different requests; a default in the
	// schema invites a client to send false on every patch, which would
	// turn every name change into an implicit reactivation.
	deact, ok := props["deactivated"].(map[string]any)
	if !ok {
		t.Fatalf("`deactivated` is not an object schema: %#v", props["deactivated"])
	}
	if deact["type"] != "boolean" {
		t.Errorf("deactivated type = %v, want \"boolean\"", deact["type"])
	}
	if _, hasDefault := deact["default"]; hasDefault {
		t.Errorf("deactivated declares a default (%v) — absent and false must stay distinct requests",
			deact["default"])
	}
}

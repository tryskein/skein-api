package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestComponentDispatch_EachActionReachesItsHandler pins the routing table
// by identity: each action is bound to a distinct sentinel and must land
// on exactly the one named. A test that only checked "some handler ran"
// would stay green if `list` and `archive` were transposed in the map
// literal — a read swapped for a mutation.
func TestComponentDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := componentActions.with(sentinelHandlers(componentActions, &got))

	want := []string{"list", "create", "update", "archive"}
	if len(want) != len(componentActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(componentActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_component", set, map[string]any{
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

// TestComponentDispatch_BadAction: a missing or unknown discriminator must
// come back as a structured validation_error carrying allowedActions, so
// an agent that guessed wrong can correct itself in the next turn.
//
// Note the "project only" case. `project` is required alongside `action`
// in this tool's schema, and it would be easy to assume that a request
// carrying the required-looking half gets further. It must not: the
// dispatcher rejects on `action` before any handler — and therefore before
// any nil store — is touched.
func TestComponentDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(componentActions.order))
	for _, name := range componentActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := componentActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"project only", map[string]any{"project": "skein"}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_component_archive"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_component", set, tc.args)
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
			if len(allowed) != len(componentActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(componentActions.order))
			}
			for i, v := range allowed {
				if v != componentActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, componentActions.order[i])
				}
			}
		})
	}
}

// TestComponentLabel_BoundedByAllowlist guards the `action` Prometheus
// label for this group: anything off the allowlist collapses to one
// constant instead of minting a series from caller-controlled text.
func TestComponentLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := componentActions.with(sentinelHandlers(componentActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "archive"}, "archive"},
		{map[string]any{"action": " create "}, "create"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "List"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"action": "skein_component_list"}, actionInvalid},
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

// TestComponentSchema_ArchivedTypeSplit is the guard on the one rename in
// this consolidation that was forced by the type system rather than by
// taste. The folded tools disagreed on what `archived` IS: a three-valued
// string enum selecting which rows to list, and a boolean choosing
// archive-vs-unarchive. JSON Schema allows one type per property name, so
// the read filter had to become `archivedFilter`.
//
// The test asserts both halves — the enum on the filter and the boolean on
// the mutation — because collapsing them back into one property is a change
// that compiles, passes every handler test (the handlers read the names
// they read), and only breaks at the wire, where the agent is.
//
// `project` being required alongside `action` is checked here too: it is
// required in all four actions, which is what lets it sit in the plain
// `required` array without reaching for the top-level oneOf the platform
// rejects outright.
func TestComponentSchema_ArchivedTypeSplit(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterComponent(s, nil, nil)

	tool := registeredTool(t, s, "skein_component")
	for _, want := range []string{"action", "project"} {
		if !slices.Contains(tool.InputSchema.Required, want) {
			t.Errorf("required = %v, must contain %q", tool.InputSchema.Required, want)
		}
	}

	props := tool.InputSchema.Properties

	filter, ok := props["archivedFilter"].(map[string]any)
	if !ok {
		t.Fatalf("skein_component has no `archivedFilter` parameter (properties: %v)", keysOf(props))
	}
	if filter["type"] != "string" {
		t.Errorf("archivedFilter type = %v, want \"string\"", filter["type"])
	}
	enum, _ := filter["enum"].([]any)
	wantEnum := []string{"false", "true", "all"}
	if len(enum) != len(wantEnum) {
		t.Fatalf("archivedFilter enum = %v, want %v", enum, wantEnum)
	}
	for i, v := range enum {
		if v != wantEnum[i] {
			t.Errorf("archivedFilter enum[%d] = %v, want %q", i, v, wantEnum[i])
		}
	}

	archived, ok := props["archived"].(map[string]any)
	if !ok {
		t.Fatalf("skein_component has no `archived` parameter (properties: %v)", keysOf(props))
	}
	if archived["type"] != "boolean" {
		t.Errorf("archived type = %v, want \"boolean\" — it is the archive/unarchive switch, "+
			"not the list filter", archived["type"])
	}
}

// TestComponentGroup_StaleArchivedArgumentIsRejected pins the read side of
// the one rename this consolidation could not avoid.
//
// `archived` still exists in this flat schema — as the BOOLEAN
// action="archive" reads — so a caller sending the pre-sk-255.3
// archived:"true" to action="list" hits a name that IS in the schema and,
// ignored, gets the ACTIVE components back while believing it asked for
// the archived ones. An inverted answer to a read is quieter than an
// inverted write and no less wrong, and the same rule already governs the
// mark_read family (see mark_read.go).
//
// Runs without a database: the rejection must precede project resolution.
func TestComponentGroup_StaleArchivedArgumentIsRejected(t *testing.T) {
	list := handleComponentList(nil, nil)
	ctx := withCtx(t, uuid.Must(uuid.NewRandom()), "user")

	for _, stale := range []any{"true", "false", "all", true} {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "skein_component"
		req.Params.Arguments = map[string]any{
			"action": "list", "project": "whatever", "archived": stale,
		}
		res, err := list(ctx, req)
		if err != nil {
			t.Fatalf("handler: transport error %v", err)
		}
		if !res.IsError {
			t.Fatalf("archived=%v was accepted by action=\"list\": %+v", stale, res)
		}
		body := envelope(t, res)
		if body["error"] != ErrCodeValidationError {
			t.Errorf("archived=%v: error = %v, want %q", stale, body["error"], ErrCodeValidationError)
		}
		msg, _ := body["message"].(string)
		if !strings.Contains(msg, "archivedFilter") {
			t.Errorf("archived=%v: message = %q, want it to name archivedFilter", stale, msg)
		}
	}
}

// TestComponentGroup_ArchivedFilterDefaultIsNotInjected is the assumption
// the guard above rests on: the schema declares defaults for both
// `archivedFilter` and `archived`, and if the server materialised them into
// every request, action="list" would reject itself on the default. mcp-go
// does not — defaults are a hint to the client — and this pins that.
func TestComponentGroup_ArchivedFilterDefaultIsNotInjected(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterComponent(s, nil, nil)

	var seen map[string]any
	set := componentActions.with(map[string]server.ToolHandlerFunc{
		"list": func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			seen = req.GetArguments()
			return mcpgo.NewToolResultText("ok"), nil
		},
		"create":  func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return nil, nil },
		"update":  func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return nil, nil },
		"archive": func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return nil, nil },
	})
	callGroupAction(t, "skein_component", set, map[string]any{"action": "list", "project": "p"})

	if _, injected := seen["archived"]; injected {
		t.Errorf("the request carries an `archived` the caller never sent (%v) — "+
			"schema defaults are being materialised server-side, which makes the "+
			"stale-argument guard reject every list call", seen)
	}
}

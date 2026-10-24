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

// TestThemeDispatch_EachActionReachesItsHandler pins the routing table by
// identity: each action is bound to a distinct sentinel and must land on
// exactly the one named. Identity is what matters in this group above
// all: `preview_variables` and `set_variables` take the SAME `variables`
// argument and differ only in whether the change is persisted, so a
// transposed entry in the map literal writes a half-finished experiment
// into a saved theme every user of it can see — and a test that only
// asserted "no error" would stay green through it.
func TestThemeDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := themeActions.with(sentinelHandlers(themeActions, &got))

	want := []string{
		"instructions", "describe", "fork",
		"preview_variables", "set_variables", "inspect_clear",
	}
	if len(want) != len(themeActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(themeActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_theme", set, map[string]any{"action": action})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestThemeDispatch_BadAction: a missing or unknown discriminator must
// come back as a structured validation_error carrying allowedActions.
//
// The two "old tool name" shapes are the mistakes this fold invites — an
// agent that learned skein_theme_fork before the fold, and one that
// half-remembers it and sends the suffix with the wrong separator.
func TestThemeDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(themeActions.order))
	for _, name := range themeActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := themeActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"variables without action", map[string]any{"variables": map[string]any{"--brand": "#000"}}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_theme_fork"}},
		{"preview without prefix", map[string]any{"action": "preview"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_theme", set, tc.args)
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
			if len(allowed) != len(themeActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(themeActions.order))
			}
			for i, v := range allowed {
				if v != themeActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, themeActions.order[i])
				}
			}
		})
	}
}

// TestThemeLabel_BoundedByAllowlist guards the `action` Prometheus label
// for this group: anything off the allowlist collapses to one constant
// instead of minting a series from caller-controlled text.
func TestThemeLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := themeActions.with(sentinelHandlers(themeActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "set_variables"}, "set_variables"},
		{map[string]any{"action": " describe "}, "describe"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "Fork"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"action": "skein_theme_describe"}, actionInvalid},
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

// TestThemeInstructions_RawMarkdownStaysOK is the check the consolidation
// plan asked for by name, done against the real handler rather than by
// reading the classifier and agreeing with it.
//
// action="instructions" is the only response on the entire MCP surface
// that is not a JSON object: it answers with the embedded markdown guide
// via NewToolResultText. The metrics classifier decides business_error vs
// ok by probing the body for an error envelope, and if that probe were
// loose enough to look inside arbitrary text, this one action would be
// mis-scored on every call — a permanently red slice of
// skein_mcp_tool_errors_total that no code change caused.
//
// It is not: toolErrorCode only parses bodies that start with `{`. The
// test runs the production handler through the production dispatcher (no
// database is needed — the handler's only precondition is a user in ctx)
// and asserts the classification of the actual result.
func TestThemeInstructions_RawMarkdownStaysOK(t *testing.T) {
	set := themeActions.with(map[string]server.ToolHandlerFunc{
		"instructions":      handleThemeInstructions(),
		"describe":          unreachableHandler(t, "describe"),
		"fork":              unreachableHandler(t, "fork"),
		"preview_variables": unreachableHandler(t, "preview_variables"),
		"set_variables":     unreachableHandler(t, "set_variables"),
		"inspect_clear":     unreachableHandler(t, "inspect_clear"),
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "skein_theme"
	req.Params.Arguments = map[string]any{"action": "instructions"}
	ctx := withCtx(t, uuid.Must(uuid.NewRandom()), "user")

	// Go through the instrumented wrapper, not the bare dispatcher, so the
	// answer is read off the Prometheus series an operator would look at
	// rather than inferred from the classifier's return.
	okBefore := callsForAction(t, "skein_theme", "instructions", outcomeOK)
	errBefore := errorsForAction(t, "skein_theme", "instructions", codeOther)
	instrumented := instrumentActionTool("skein_theme", set, set.dispatch("skein_theme"))

	res, err := instrumented(ctx, req)
	if err != nil {
		t.Fatalf("dispatch returned transport error: %v", err)
	}

	if got := callsForAction(t, "skein_theme", "instructions", outcomeOK) - okBefore; got != 1 {
		t.Errorf("calls{action=\"instructions\",outcome=\"ok\"} delta = %v, want 1", got)
	}
	if got := errorsForAction(t, "skein_theme", "instructions", codeOther) - errBefore; got != 0 {
		t.Errorf("errors{action=\"instructions\",code=\"other\"} delta = %v, want 0 — "+
			"the markdown body was scored a failure", got)
	}

	// The premise: the body really is raw markdown, not a JSON object. If
	// this ever changes the reasoning below stops applying, so assert it
	// rather than assume it.
	body := firstTextContent(t, res)
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("instructions body now starts with `{` — it is JSON, and the "+
			"classifier reasoning in theme_group.go no longer holds: %.60s", body)
	}
	if !strings.Contains(body, "# Theme customization") {
		t.Errorf("instructions body is not the embedded guide: %.60s", body)
	}

	if code := toolErrorCode(res); code != "" {
		t.Errorf("toolErrorCode = %q, want \"\" — markdown was read as an error envelope", code)
	}
	outcome, code := classifyToolCall(res, err)
	if outcome != outcomeOK || code != "" {
		t.Errorf("classifyToolCall = (%q, %q), want (%q, \"\") — the one non-JSON "+
			"action on the surface must not be scored an error", outcome, code, outcomeOK)
	}
}

// unreachableHandler fails the test if it is ever dispatched to. Used to
// fill an action table where exactly one entry is the real handler under
// test.
func unreachableHandler(t *testing.T, action string) server.ToolHandlerFunc {
	t.Helper()
	return func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		t.Errorf("dispatched to %q, which this test should never reach", action)
		return mcpgo.NewToolResultText("ok"), nil
	}
}

// TestThemeSchema_OnlyActionIsRequired reads the shape decision back off
// the wire. Three of the six actions (instructions, describe,
// inspect_clear) take no arguments at all, so `action` is the only thing
// that can sit in the `required` array — marking `variables` or `themeId`
// required would make half the tool uncallable through a client that
// validates the schema before sending.
//
// `variables` is also pinned as an object: it is the one parameter shared
// by two actions, and the handlers decode it with a map type assertion
// that produces "variables must be an object" — a schema that declared it
// a string would push that failure onto the agent for every preview.
func TestThemeSchema_OnlyActionIsRequired(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterTheme(s, nil, nil, nil)

	tool := registeredTool(t, s, "skein_theme")
	if want := []string{"action"}; !slices.Equal(tool.InputSchema.Required, want) {
		t.Errorf("required = %v, want %v — three actions take no arguments",
			tool.InputSchema.Required, want)
	}

	props := tool.InputSchema.Properties
	for _, name := range []string{"action", "source", "name", "themeId", "variables"} {
		if _, ok := props[name]; !ok {
			t.Errorf("skein_theme has no %q parameter (properties: %v)", name, keysOf(props))
		}
	}
	vars, ok := props["variables"].(map[string]any)
	if !ok {
		t.Fatalf("`variables` is not an object schema: %#v", props["variables"])
	}
	if vars["type"] != "object" {
		t.Errorf("variables type = %v, want \"object\"", vars["type"])
	}
}

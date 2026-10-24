package tools

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registeredToolProperties reads one registered tool's input-schema
// properties back off the wire.
func registeredToolProperties(t *testing.T, s *server.MCPServer, name string) map[string]any {
	t.Helper()
	return registeredTool(t, s, name).InputSchema.Properties
}

// registeredTool reads one tool descriptor back through the same
// tools/list payload a client receives — so assertions run against what
// the agent actually gets, not against the local variable that built it.
func registeredTool(t *testing.T, s *server.MCPServer, name string) mcpgo.Tool {
	t.Helper()

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
	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for i := range listRes.Tools {
		if listRes.Tools[i].Name == name {
			return listRes.Tools[i]
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return mcpgo.Tool{}
}

// keysOf renders a property map for a failure message.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// callGroupAction runs a tools/call request through one consolidated
// tool's dispatcher only — no server, no auth context, no database. What
// is under test is which handler the `action` selects; what those handlers
// then do is covered by the per-handler tests that call them directly
// (watch_test.go, component_*_test.go), which the consolidation left in
// place on purpose.
func callGroupAction(t *testing.T, tool string, set actionSet, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := set.dispatch(tool)(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch returned transport error: %v", err)
	}
	if res == nil {
		t.Fatal("dispatch returned nil result")
	}
	return res
}

// sentinelHandlers binds every action in the table to a handler that
// records its own name, so a test can assert routing by identity instead
// of by "some handler ran".
func sentinelHandlers(set actionSet, got *string) map[string]server.ToolHandlerFunc {
	handlers := make(map[string]server.ToolHandlerFunc, len(set.order))
	for _, name := range set.order {
		name := name
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			*got = name
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	return handlers
}

// TestWatchersDispatch_EachActionReachesItsHandler pins the routing table
// by identity: each action is bound to a distinct sentinel and must land
// on exactly the one named. Identity matters more here than in any other
// group — `watch`/`unwatch` and `project_watch`/`project_unwatch` are two
// pairs of opposites, so a transposed entry in the map literal would be a
// tool that unsubscribes you when you asked to subscribe, and a test that
// only asserted "no error" would stay green through it.
func TestWatchersDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := watcherActions.with(sentinelHandlers(watcherActions, &got))

	// Spelled out rather than derived from watcherActions.order: this list
	// is the test's independent statement of the surface, so dropping an
	// action from the table fails here instead of silently shrinking what
	// is checked.
	want := []string{
		"watch", "unwatch",
		"project_watch", "project_unwatch",
		"filter_create", "filter_delete", "filter_list",
		"list",
		"monitor_stop",
	}
	if len(want) != len(watcherActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(watcherActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_watchers", set, map[string]any{"action": action})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestWatchersDispatch_BadAction covers the ways an agent gets the
// discriminator wrong, including the two that are specific to this group:
// sending the flattened sub-discriminators the folded tools used to take
// (`op="create"`, `unwatch=true`) instead of a top-level action. Both must
// come back as a structured validation_error carrying allowedActions —
// that list is what lets an agent correct itself in the next turn instead
// of re-running Tool Search.
func TestWatchersDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(watcherActions.order))
	for _, name := range watcherActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := watcherActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_filter_watch"}},
		// The pre-sk-255.3 shapes: the inner discriminators are gone, and
		// sending one without a top-level action must not silently do
		// anything.
		{"stale op discriminator", map[string]any{"op": "create", "name": "x"}},
		{"stale unwatch flag", map[string]any{"unwatch": true, "project": "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_watchers", set, tc.args)
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
			if len(allowed) != len(watcherActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(watcherActions.order))
			}
			for i, v := range allowed {
				if v != watcherActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, watcherActions.order[i])
				}
			}
		})
	}
}

// TestWatchersLabel_BoundedByAllowlist guards the `action` Prometheus
// label for this group — the one change in the consolidation that cannot
// be walked back in production, because a caller-controlled string in a
// label mints a series per typo and the cardinality never comes down.
func TestWatchersLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := watcherActions.with(sentinelHandlers(watcherActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "project_unwatch"}, "project_unwatch"},
		{map[string]any{"action": " filter_list "}, "filter_list"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "Watch"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"op": "list"}, actionInvalid},      // the old inner discriminator
		{map[string]any{"action": "skein_watch"}, actionInvalid},
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

// TestWatchersActions_NoIdentifierNameCollision is a schema-level guard on
// the rename this group was forced into. The folded tools used `id` for
// two unrelated identifier kinds — a hierarchical issue id in skein_watch,
// a custom-watcher UUID in skein_filter_watch — and in one flat schema
// that is a parameter an agent cannot fill correctly without guessing
// which action it is in. The consolidated tool must therefore expose
// `issueId` and `filterId` and must NOT expose a bare `id`.
func TestWatchersActions_NoIdentifierNameCollision(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterWatchers(s, nil, nil, nil, nil)

	props := registeredToolProperties(t, s, "skein_watchers")
	for _, want := range []string{"issueId", "filterId", "monitorId"} {
		if _, ok := props[want]; !ok {
			t.Errorf("skein_watchers has no %q parameter (properties: %v)", want, keysOf(props))
		}
	}
	if _, bad := props["id"]; bad {
		t.Errorf("skein_watchers still exposes a bare `id` — it meant two different " +
			"identifier kinds in the folded tools, which is why it was split")
	}
	// The flattened sub-discriminators must be gone from the schema too,
	// or an agent can send a shape the dispatcher will never honour.
	for _, gone := range []string{"op", "unwatch"} {
		if _, bad := props[gone]; bad {
			t.Errorf("skein_watchers still exposes %q — it was flattened into the action enum", gone)
		}
	}
}

// TestWatchersGroup_StaleDiscriminatorsAreRejected pins the rule
// mark_read.go states and this group has to obey for the same reason: a
// discriminator that moved into the `action` enum must be REJECTED where
// it used to live, not ignored.
//
// skein_project_watch carried a boolean `unwatch` until sk-255.3, and
// `eventTypes` was only ever read by the watch direction. Both are exactly
// what a cached schema, an out-of-date consumer or a model prior still
// sends, and silently ignoring them inverts the caller's intent:
// action="project_watch" + unwatch=true reads as "unsubscribe" and
// SUBSCRIBES. On this pair the guess is not merely wrong, it is backwards.
//
// Runs without a database: every rejection here must happen before project
// resolution, so a nil pool is the proof that it does.
func TestWatchersGroup_StaleDiscriminatorsAreRejected(t *testing.T) {
	watch := handleProjectWatch(nil, nil, nil, false)
	unwatch := handleProjectWatch(nil, nil, nil, true)
	ctx := withCtx(t, uuid.Must(uuid.NewRandom()), "user")

	cases := []struct {
		name    string
		handler server.ToolHandlerFunc
		args    map[string]any
		want    string
	}{
		{
			"project_watch with the old unwatch:true flag",
			watch,
			map[string]any{"action": "project_watch", "project": "whatever", "unwatch": true},
			"action=\"project_unwatch\"",
		},
		{
			// unwatch:false is just as stale — it named a direction that is
			// no longer a parameter at all.
			"project_watch with the old unwatch:false flag",
			watch,
			map[string]any{"action": "project_watch", "project": "whatever", "unwatch": false},
			"unwatch is no longer a parameter",
		},
		{
			"project_unwatch with the old unwatch flag",
			unwatch,
			map[string]any{"action": "project_unwatch", "project": "whatever", "unwatch": true},
			"unwatch is no longer a parameter",
		},
		{
			// Valid event types on the unsubscribe direction used to be
			// accepted and silently dropped — the caller believed they had
			// narrowed a subscription they had actually removed.
			"project_unwatch with valid eventTypes",
			unwatch,
			map[string]any{"action": "project_unwatch", "project": "whatever", "eventTypes": []any{"issue.created"}},
			"only read by project_watch",
		},
		{
			// Invalid event types blocked the unsubscribe outright: the
			// decode ran before the direction branch.
			"project_unwatch with malformed eventTypes",
			unwatch,
			map[string]any{"action": "project_unwatch", "project": "whatever", "eventTypes": "not-an-array"},
			"only read by project_watch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Name = "skein_watchers"
			req.Params.Arguments = tc.args
			res, err := tc.handler(ctx, req)
			if err != nil {
				t.Fatalf("handler: transport error %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected a rejection, got %+v", res)
			}
			body := envelope(t, res)
			if body["error"] != ErrCodeValidationError {
				t.Errorf("error = %v, want %q", body["error"], ErrCodeValidationError)
			}
			msg, _ := body["message"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

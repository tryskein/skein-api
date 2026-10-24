package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
)

// metricActionInvalid mirrors tools.actionInvalid — the bounded `action`
// label value everything off a tool's allowlist collapses to. Duplicated
// as a literal on purpose, for the same reason metaSearchHint is: these
// tests assert the observable contract, so they must not be written in
// terms of the constant they are checking.
const metricActionInvalid = "__invalid__"

// consolidatedTools returns the registered tools that declare an `action`
// enum, keyed by name, with the enum as the value.
//
// "Declares an `action` enum" is the definition of consolidated used by
// every guard in this file, and it is deliberately read off the tools/list
// payload rather than from a list maintained here: a ninth folded family
// then arrives inside each guard automatically instead of silently
// bypassing all of them.
func consolidatedTools(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, tool := range listRegisteredTools(t) {
		if enum, ok := actionEnum(tool); ok && len(enum) > 0 {
			out[tool.Name] = enum
		}
	}
	if len(out) == 0 {
		t.Fatal("no tool declares an `action` enum — either the fold was reverted or the probe broke; " +
			"every guard in this file would be checking nothing")
	}
	return out
}

// TestSearchHints_Budget enforces the word budgets search_hints.go states
// in prose: 3–8 words for a single-verb tool, 10–18 for a consolidated one.
//
// The budgets are the whole reason the hints stay useful. Under budget, a
// folded tool loses the verbs that only survive in its hint (the NAME no
// longer has them); over budget, the hint dilutes into prose and every
// term's weight drops. The rule was written down when the fold landed and
// nothing checked it — which is how the author of a rule ends up breaking
// it in the same change.
//
// Consolidated-ness is derived from the surface, not declared here, so a
// new folded tool gets the wider budget without anyone remembering to
// grant it.
func TestSearchHints_Budget(t *testing.T) {
	const (
		simpleMin, simpleMax             = 3, 8
		consolidatedMin, consolidatedMax = 10, 18
	)

	consolidated := consolidatedTools(t)
	for _, tool := range listRegisteredTools(t) {
		if tool.Meta == nil {
			continue // TestSearchHints_EveryTool owns the missing case.
		}
		hint, _ := tool.Meta.AdditionalFields[metaSearchHint].(string)
		if hint == "" {
			continue
		}
		words := len(strings.Fields(hint))
		lo, hi := simpleMin, simpleMax
		kind := "single-verb"
		if _, folded := consolidated[tool.Name]; folded {
			lo, hi, kind = consolidatedMin, consolidatedMax, "consolidated"
		}
		if words < lo || words > hi {
			t.Errorf("%s: searchHint is %d words, the budget for a %s tool is %d–%d — %q",
				tool.Name, words, kind, lo, hi, hint)
		}
	}
}

// TestConsolidatedTools_MetricLabelIsTheAction is the guard on the
// invariant action_dispatch.go and metrics.go both state and neither
// enforced: a consolidated tool is registered through addActionTool, so
// its calls carry the resolved `action` metric label, and `action=""`
// means "this tool is not folded".
//
// Nothing structural stops a Register* function from calling
// addTool(s, tool, set.dispatch(name)) instead. Dispatch still works, the
// enum guard still passes, the whole suite stays green — and the tool
// reports every call as action="" and looks unfolded in every dashboard
// built on the label, which is the one artefact of this consolidation that
// cannot be reconstructed after the fact.
//
// The probe uses an impossible action on purpose: it needs no database
// (the dispatcher rejects before any handler runs) yet still proves which
// wrapper is installed, because addActionTool labels the rejection
// "__invalid__" where addTool labels it "".
func TestConsolidatedTools_MetricLabelIsTheAction(t *testing.T) {
	consolidated := consolidatedTools(t)

	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	mcpRegisterToolsFn(mcpDeps{})(s)
	c := startInProcess(t, s)
	ctx := context.Background()

	for name := range consolidated {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = map[string]any{"action": "__definitely_not_an_action__"}
		if _, err := c.CallTool(ctx, req); err != nil {
			t.Fatalf("%s: CallTool: %v", name, err)
		}
	}

	series := toolCallSeries(t)
	for name := range consolidated {
		actions := series[name]
		if len(actions) == 0 {
			t.Errorf("%s: no skein_mcp_tool_calls_total series after a call — the tool is not instrumented at all", name)
			continue
		}
		if _, blank := actions[""]; blank {
			t.Errorf("%s: reports calls with action=\"\" — that value means \"not a consolidated tool\", "+
				"so this tool was registered through addTool instead of addActionTool", name)
		}
		if _, invalid := actions[metricActionInvalid]; !invalid {
			t.Errorf("%s: an unknown action was not recorded as action=%q (recorded: %v)",
				name, metricActionInvalid, sortedKeys(actions))
		}
	}
}

// toolCallSeries reads skein_mcp_tool_calls_total back out of the default
// registry as tool → set of `action` label values seen.
func toolCallSeries(t *testing.T) map[string]map[string]struct{} {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]map[string]struct{}{}
	for _, family := range families {
		if family.GetName() != "skein_mcp_tool_calls_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			var tool, action string
			var actionSeen bool
			for _, label := range metric.GetLabel() {
				switch label.GetName() {
				case "tool":
					tool = label.GetValue()
				case "action":
					action, actionSeen = label.GetValue(), true
				}
			}
			if !actionSeen {
				t.Fatalf("skein_mcp_tool_calls_total has no `action` label: %v", metric.GetLabel())
			}
			if out[tool] == nil {
				out[tool] = map[string]struct{}{}
			}
			out[tool][action] = struct{}{}
		}
	}
	return out
}

// sortedKeys renders a set deterministically, for a readable failure message.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// startInProcess brings up an in-process client against s, initialized and
// ready for tool calls. The context passed to CallTool reaches the handler
// untouched (InProcessTransport hands it straight to MCPServer.HandleMessage),
// which is what lets a caller-authenticated context be threaded in.
func startInProcess(t *testing.T, s *server.MCPServer) *client.Client {
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
	return c
}

package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
)

// actionParam is the discriminator every consolidated tool takes.
//
// The name is not arbitrary: Anthropic's own "Define tools" guidance
// spells this pattern out and calls the parameter `action` ("rather than
// creating a separate tool for every action, group them into a single
// tool with an `action` parameter"). Matching the name the model was
// taught costs nothing and is the one part of the schema we can align
// with the trained prior for free. It is NOT `op` (what the pre-existing
// skein_filter_watch chose) and NOT `method` (what GitHub's MCP server
// chose).
const actionParam = "action"

// actionInvalid is the `action` metric label value for anything the
// allowlist does not contain — a typo, a stale action name from before a
// rename, or the parameter missing entirely.
//
// This constant is the whole reason the label is safe. The raw argument
// is caller-controlled text, so putting it in a Prometheus label would
// mint a new time series per typo — the textbook unbounded-cardinality
// hole metrics.go warns about, and the one change in this consolidation
// that cannot be undone in production once the series exist. Everything
// off actionSet.order collapses here; the actual string an agent sent is
// recoverable from the WARN line dispatch writes, which costs nothing per
// scrape.
const actionInvalid = "__invalid__"

// actionSet is the action table of a consolidated tool.
//
// One object feeds three things that must agree or the tool lies to its
// caller: the `enum` of the `action` parameter in the schema, the runtime
// allowlist the dispatcher matches against, and the `allowedActions` list
// in the validation error an agent reads to self-correct. Deriving all
// three from `order` is what makes "the enum and the validation cannot
// drift apart" a structural property rather than a review convention —
// TestActionEnum_MatchesDispatchAllowlist (cmd/api) checks it end to end
// against the registered surface anyway, because a structural property
// nobody tests is a comment.
//
// Declaration is two-phase on purpose. The `order` half is a package-level
// var so the schema can name the enum without holding any dependency; the
// handler half arrives at registration time, when deps exist, via with().
type actionSet struct {
	order    []string
	handlers map[string]server.ToolHandlerFunc
}

// with binds handlers to the action table, panicking when the two do not
// match key for key.
//
// Panic, not an error return: with() runs inside mcpRegisterToolsFn at
// process start, so a mismatch is a programmer error that must stop the
// binary rather than surface as one dead action discovered by an agent in
// production. It is also caught in CI without a database — the
// registration closure is executed by TestSearchHints_EveryTool against a
// zero-valued mcpDeps.
func (a actionSet) with(handlers map[string]server.ToolHandlerFunc) actionSet {
	if len(handlers) != len(a.order) {
		panic(fmt.Sprintf("actionSet: %d handlers for %d actions %v — "+
			"every action in the enum needs exactly one handler and vice versa",
			len(handlers), len(a.order), a.order))
	}
	for _, name := range a.order {
		if handlers[name] == nil {
			panic(fmt.Sprintf("actionSet: no handler bound for action %q (actions: %v)", name, a.order))
		}
	}
	return actionSet{order: a.order, handlers: handlers}
}

// enum returns the action names in declaration order, for mcpgo.Enum.
func (a actionSet) enum() []string { return a.order }

// list renders the actions for a human-readable error message.
func (a actionSet) list() string { return strings.Join(a.order, ", ") }

// label maps a request onto the bounded `action` metric label: the action
// itself when it is in the allowlist, actionInvalid for everything else
// (including a missing parameter).
func (a actionSet) label(req mcpgo.CallToolRequest) string {
	raw := strings.TrimSpace(req.GetString(actionParam, ""))
	if _, ok := a.handlers[raw]; ok {
		return raw
	}
	return actionInvalid
}

// dispatch returns the tool handler: it resolves `action` to one of the
// bound handlers, or returns a structured validation_error carrying the
// full allowlist.
//
// The error shape is the point. An agent that guessed wrong gets
// `allowedActions` in the same response and can correct itself in one
// turn instead of re-running Tool Search — which is the compensation for
// this whole consolidation moving the verb out of the tool name and into
// a parameter.
//
// tool is used for the WARN line only: the metric records the miss as
// action="__invalid__" by design, so the log is the only place the string
// the agent actually sent survives. That pair — bounded label, unbounded
// detail in logs — is deliberate.
func (a actionSet) dispatch(tool string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		raw := strings.TrimSpace(req.GetString(actionParam, ""))
		if raw == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"action is required — one of: "+a.list(),
				map[string]any{"allowedActions": a.order}), nil
		}
		handler, ok := a.handlers[raw]
		if !ok {
			slog.WarnContext(ctx, "mcp unknown action",
				"tool", tool, "action", raw, "allowedActions", a.order)
			return skmcp.ToolError(ErrCodeValidationError,
				"unknown action — expected one of: "+a.list(),
				map[string]any{"action": raw, "allowedActions": a.order}), nil
		}
		return handler(ctx, req)
	}
}

// addActionTool is addTool's sibling for consolidated tools: same
// searchHint stamping, same usage counters, but the counters carry the
// resolved `action` label so a folded tool stays as legible in Prometheus
// as the N tools it replaced.
//
// addTool is deliberately left untouched rather than growing an optional
// actionSet argument. It stays the single funnel for every tool that has
// no action parameter, which is what keeps `action=""` an honest marker
// of "this tool is not folded" instead of a default that a folded tool
// could fall back to by forgetting an argument.
func addActionTool(s *server.MCPServer, tool mcpgo.Tool, actions actionSet) {
	stampSearchHint(&tool)
	s.AddTool(tool, instrumentActionTool(tool.Name, actions, actions.dispatch(tool.Name)))
}

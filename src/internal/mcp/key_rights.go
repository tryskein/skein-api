package mcp

import (
	"context"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/errcode"
)

// KeyRights is the tool-handler middleware that applies the key-rights
// matrix (authz.MCPToolRequirement) to every MCP call made with an API key,
// before the tool runs — so before any object is looked up, and a refusal
// never reveals whether the object exists.
//
// A call without an API key in its context (an in-process server of a test
// or a future non-key entry) is not a call by key and passes. A call with a
// key id but no grant is refused: the safe default for a context that lost
// its grant. A consolidated tool called with an action outside its set
// passes to the tool's dispatcher, which refuses it with validation_error
// and runs nothing; every action the dispatcher does run has a row in the
// matrix (tools.mustHaveKeyRights checks it at registration).
func KeyRights(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if refused := keyRightsRefusal(ctx, req); refused != nil {
			return refused, nil
		}
		return next(ctx, req)
	}
}

// keyRightsRefusal is the decision of KeyRights: nil to let the call run,
// otherwise the insufficient_scope envelope.
func keyRightsRefusal(ctx context.Context, req mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	if _, byKey := auth.APIKeyIDFrom(ctx); !byKey {
		return nil
	}
	tool := req.Params.Name
	action := ""
	if authz.MCPToolHasActions(tool) {
		action = strings.TrimSpace(req.GetString("action", ""))
	}
	force := req.GetBool("force", false)
	need, known := authz.MCPToolRequirement(tool, action, force)
	if !known {
		if authz.MCPToolHasActions(tool) {
			// An action outside the tool's set: the dispatcher answers.
			return nil
		}
		return insufficientScope(tool, action, "", false)
	}
	grant, ok := auth.KeyGrantFrom(ctx)
	if !ok || !authz.KeyMeets(grant, need) {
		return insufficientScope(tool, action, need.Right.String(), need.WideScope)
	}
	return nil
}

// insufficientScope is the refusal of a call the key may not make: the code,
// the right the call requires, and the tool (and action) it was refused on.
func insufficientScope(tool, action, required string, wideScope bool) *mcpgo.CallToolResult {
	fields := map[string]any{"tool": tool}
	if action != "" {
		fields["action"] = action
	}
	msg := "this API key may not call this tool"
	if required != "" {
		fields["required"] = required
		msg = "this API key lacks the right this call requires: " + required
	}
	if wideScope {
		fields["requiredScope"] = "spaces or all"
		msg += ", with a scope of spaces or all"
	}
	return ToolError(errcode.InsufficientScope.String(), msg+"; nothing was done", fields)
}

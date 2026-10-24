// Package mcp wires the Streamable-HTTP MCP server (R2.1).
//
// The /mcp endpoint sits at the root level of skein-api, mounted at the
// same level as /healthz and /metrics. It is gated by:
//
//   - RequireInternalSecret — channel auth (same as /api/v1/*); the BFF
//     adds the secret on its proxy.
//   - auth.RequireAPIKey — user auth via X-Skein-Api-Key header.
//   - mcp.AgentRegistryMiddleware — UPSERTs the (user_id, session_id)
//     pair into the agents table when a session id is present.
//
// Tool handlers receive auth context via mcp-go's WithHTTPContextFunc,
// which copies values out of the http.Request context into the per-call
// context the handlers see.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
)

// ToolJSON marshals v as compact JSON and returns it as a TextContent
// success result. The marshal error is wrapped — handlers should treat
// it as "this tool is broken" rather than try to recover.
func ToolJSON(v any) (*mcpgo.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// ToolError encodes a stable error code + optional structured fields as a
// JSON tool error. The shape is `{"error":"<code>","message":"<msg>",...}`,
// matching the rest of the Skein API's ProblemDetails-style payloads so a
// caller has one parsing pattern across REST and MCP.
//
// We intentionally do NOT use outputSchema/structuredContent here — Claude
// Code regressed structured content support in 2.1.88 and recovery has
// not been confirmed. Plain JSON in the text content is the lowest-common-
// denominator that every MCP client honours today.
func ToolError(code, message string, fields map[string]any) *mcpgo.CallToolResult {
	// Fast path: fixed envelope only — typed struct avoids map+interface
	// boxing on the common "code + message" error path.
	if len(fields) == 0 {
		b, err := json.Marshal(toolErrorBody{Error: code, Message: message})
		if err != nil {
			// Fallback: plain string; never leave the agent without an
			// `error` code to branch on. Log original fields for ops.
			slog.Error("mcp ToolError marshal failed",
				"code", code, "message", message, "error", err)
			return mcpgo.NewToolResultError(`{"error":"internal_error","message":"failed to marshal error"}`)
		}
		return mcpgo.NewToolResultError(string(b))
	}
	// Open extra fields (field, index, currentVersion, …) still need a
	// map merge — keep the prior shape byte-for-byte for callers.
	body := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		body[k] = v
	}
	body["error"] = code
	if message != "" {
		body["message"] = message
	}
	b, err := json.Marshal(body)
	if err != nil {
		// Fallback: plain string; never leave the agent without an
		// `error` code to branch on. Log original fields for ops.
		slog.Error("mcp ToolError marshal failed",
			"code", code, "message", message, "fields", fields, "error", err)
		return mcpgo.NewToolResultError(`{"error":"internal_error","message":"failed to marshal error"}`)
	}
	return mcpgo.NewToolResultError(string(b))
}

// toolErrorBody is the fixed portion of ToolError's JSON envelope.
type toolErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// RequireUser is the identity guard every tool handler should call before
// touching the database. Returns the user id on success, or an error
// CallToolResult on failure that the handler should return immediately.
//
// Failure mode is `unauthenticated` — never reveals whether the request
// was missing the X-Skein-Api-Key header, the key was wrong, or the
// session resolved to a deleted user. (In practice the middleware would
// have 401'd before we got here; this is defence in depth.)
func RequireUser(ctx context.Context) (uuid.UUID, *mcpgo.CallToolResult) {
	id, ok := auth.UserIDFrom(ctx)
	if !ok || id == uuid.Nil {
		return uuid.Nil, ToolError("unauthenticated",
			"X-Skein-Api-Key did not resolve to a user", nil)
	}
	return id, nil
}

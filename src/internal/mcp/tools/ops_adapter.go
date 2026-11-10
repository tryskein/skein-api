package tools

// ops_adapter.go — the MCP side of the transport-neutral operations of
// issueops (sk-878.3.1 T024a): a refusal of an operation is the tool error
// of the same code, message and members.

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
)

// toolFailure is the tool error of f; nil for nil.
func toolFailure(f *issueops.Failure) *mcpgo.CallToolResult {
	if f == nil {
		return nil
	}
	return skmcp.ToolError(f.Code, f.Message, f.Fields)
}

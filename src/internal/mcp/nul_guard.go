package mcp

import (
	"context"
	"errors"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/textvalidate"
)

// RefuseNUL is the tool-handler middleware that refuses a call whose
// arguments carry U+0000 in any string — a value or a member name, at any
// depth — before the tool runs: the database cannot store the character
// (textvalidate/nul.go). The refusal is the validation envelope of the write
// tools: {"error":"validation_error","code":"nul_character","field":…,
// "message":…}.
func RefuseNUL(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var nulErr *textvalidate.NULError
		if err := textvalidate.FindNULInValue(req.GetArguments()); errors.As(err, &nulErr) {
			fields := map[string]any{"code": textvalidate.NULCode}
			if nulErr.Field != "" {
				fields["field"] = nulErr.Field
			}
			return ToolError("validation_error", nulErr.Error()+"; nothing was done", fields), nil
		}
		return next(ctx, req)
	}
}

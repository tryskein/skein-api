package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
)

// handleAttachmentGetURL backs skein_attachments(action="get_url") —
// mints a short-lived presigned GET URL (10 min by default) for a 'ready'
// attachment, embedding a Content-Disposition so the browser saves the
// file under its original name.
func handleAttachmentGetURL(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.AttachmentGetURL(ctx, deps.ops(), userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(res)
	}
}

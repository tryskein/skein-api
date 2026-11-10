package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
)

type deleteResponse struct {
	Deleted bool   `json:"deleted"`
	ID      string `json:"id"`
}

// handleAttachmentDelete backs skein_attachments(action="delete") —
// hard-deletes the row and best-effort removes the S3 object. Anyone with
// project visibility can delete (no uploader-only restriction); a missing
// row returns attachment_not_found. The attachment.removed event is
// written for audit but is NOT in the notifiable set, so no inbox rows
// are produced.
func handleAttachmentDelete(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.AttachmentDelete(ctx, deps.ops(), userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(deleteResponse{Deleted: true, ID: res.String()})
	}
}

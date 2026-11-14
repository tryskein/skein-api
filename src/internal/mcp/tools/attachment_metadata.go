package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
)

type metadataResponse struct {
	Attachment *attachments.Attachment `json:"attachment"`
}

// handleAttachmentMetadata backs skein_attachments(action="metadata") —
// returns the row metadata for a single attachment (filename,
// contentType, sizeBytes, status, uploader) without minting a download
// URL.
func handleAttachmentMetadata(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.AttachmentMetadata(ctx, deps.ops(), userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(metadataResponse{Attachment: res})
	}
}

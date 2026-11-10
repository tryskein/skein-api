package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
)

type confirmResponse struct {
	Attachment *attachments.Attachment `json:"attachment"`
}

// handleAttachmentConfirm backs skein_attachments(action="confirm") —
// confirms an upload landed in object storage and flips the row from
// 'pending' to 'ready'.
//
// Inputs: attachmentId (required, string) — UUID returned by upload_url.
// Output: { attachment } — the row in 'ready' state.
//
// Idempotent: a second call against an already-ready row returns the same
// DTO without re-emitting the attachment.added event.
func handleAttachmentConfirm(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.AttachmentConfirm(ctx, deps.ops(), userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(confirmResponse{Attachment: res})
	}
}

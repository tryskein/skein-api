package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
)

// handleAttachmentUploadURL backs skein_attachments(action="upload_url").
//
// Inputs:
//   - issueId   (required, string) — full hierarchical id of the parent issue.
//   - filename  (required, string) — original filename for download.
//   - contentType (required, string) — MIME type the client will upload.
//   - sizeBytes (required, number) — declared file size; the presigned
//     policy is bound to the EXACT byte count.
//   - commentId (optional, string) — UUID of a comment to scope the
//     attachment to. Spec supports comment-scoped attachments end to end;
//     R4.2 frontend ships only issue-level UI.
//
// Output: { attachmentId, url, fields, expiresAt, sizeBytes, maxSizeBytes }
//
// Writes (creates a pending row), not idempotent (each call inserts a new
// row), not destructive.
func handleAttachmentUploadURL(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.AttachmentUploadURL(ctx, deps.ops(), userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(res)
	}
}

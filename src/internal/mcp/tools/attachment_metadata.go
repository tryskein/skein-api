package tools

import (
	"context"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
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
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		raw, err := req.RequireString("attachmentId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "attachmentId is required", nil), nil
		}
		id, errResp := parseAttachmentID(raw)
		if errResp != nil {
			return errResp, nil
		}

		att, _, _, errResp := loadAttachmentChecked(ctx, deps.pool, deps.store, id, userID, role)
		if errResp != nil {
			return errResp, nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_attachments", "action", "metadata",
			"userId", userID.String(), "attachmentId", id.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(metadataResponse{Attachment: att})
	}
}

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

// handleAttachmentGetURL backs skein_attachments(action="get_url") —
// mints a short-lived presigned GET URL (10 min by default) for a 'ready'
// attachment, embedding a Content-Disposition so the browser saves the
// file under its original name.
func handleAttachmentGetURL(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)
		if r := deps.requireAttachmentsAvailable(); r != nil {
			return r, nil
		}

		raw, err := req.RequireString("attachmentId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "attachmentId is required", nil), nil
		}
		id, errResp := parseAttachmentID(raw)
		if errResp != nil {
			return errResp, nil
		}
		att, _, s3Key, errResp := loadAttachmentChecked(ctx, deps.pool, deps.store, id, userID, role)
		if errResp != nil {
			return errResp, nil
		}
		if att.Status != attachments.StatusReady {
			return skmcp.ToolError(ErrCodeAttachmentNotReady,
				"attachment is still pending upload confirmation", nil), nil
		}

		url, err := deps.s3.PresignedGetURL(ctx, s3Key, att.Filename, deps.urlTTL)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_attachments", "action", "get_url",
				"reason", "presign failed",
				"userId", userID.String(), "attachmentId", id.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mint download URL", nil), nil
		}
		resp := attachments.DownloadURLResponse{
			URL:       url,
			ExpiresAt: time.Now().UTC().Add(deps.urlTTL),
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_attachments", "action", "get_url",
			"userId", userID.String(), "attachmentId", id.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(resp)
	}
}

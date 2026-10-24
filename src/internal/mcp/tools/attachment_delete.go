package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
	"skein-api/internal/authz"
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

		// Visibility check FIRST — we don't want to leak the existence
		// of an attachment in another project by surfacing a "delete
		// failed" path that depends on whether the row was found.
		_, _, _, errResp = loadAttachmentChecked(ctx, deps.pool, deps.store, id, userID, role, authz.ActionWrite)
		if errResp != nil {
			return errResp, nil
		}

		actor := AttachmentsAgentActor(ctx, deps.pool, userID)
		bucket, s3Key, err := deps.store.Delete(ctx, id, userID, role, actor)
		if err != nil {
			if errors.Is(err, attachments.ErrNotFound) {
				return skmcp.ToolError(ErrCodeAttachmentNotFound,
					"attachment does not exist or is not visible",
					map[string]any{"attachmentId": id.String()}), nil
			}
			if errResp := mapAttachmentAccessErr(err); errResp != nil {
				return errResp, nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_attachments", "action", "delete",
				"reason", "delete failed",
				"userId", userID.String(), "attachmentId", id.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to delete attachment", nil), nil
		}

		// Best-effort S3 delete — unreachable S3 leaves the object as
		// an orphan, which the cleanup goroutine reconciles next pass.
		if deps.s3 != nil {
			if err := deps.s3.RemoveObject(ctx, s3Key); err != nil {
				slog.WarnContext(ctx, "best-effort S3 delete failed",
					"bucket", bucket, "key", s3Key, "error", err.Error())
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_attachments", "action", "delete",
			"userId", userID.String(), "attachmentId", id.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(deleteResponse{Deleted: true, ID: id.String()})
	}
}

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

		att, _, s3Key, errResp := loadAttachmentChecked(ctx, deps.pool, deps.store, id, userID, role, authz.ActionWrite)
		if errResp != nil {
			return errResp, nil
		}
		// Only the uploader can confirm — same rule as REST.
		if att.UploadedBy != userID {
			return skmcp.ToolError(ErrCodeAttachmentNotFound,
				"attachment does not exist or is not visible",
				map[string]any{"attachmentId": id.String()}), nil
		}

		// S3 existence probe (skip if already ready).
		if att.Status == attachments.StatusPending {
			if err := deps.s3.StatObject(ctx, s3Key); err != nil {
				return skmcp.ToolError(ErrCodeAttachmentObjectMissing,
					"the file has not been uploaded to storage", nil), nil
			}
		}

		actor := AttachmentsAgentActor(ctx, deps.pool, userID)
		out, err := deps.store.Confirm(ctx, id, userID, role, actor)
		if err != nil {
			if errors.Is(err, attachments.ErrNotFound) {
				return skmcp.ToolError(ErrCodeAttachmentNotFound,
					"attachment does not exist or is not visible",
					map[string]any{"attachmentId": id.String()}), nil
			}
			if errResp := mapAttachmentAccessErr(err); errResp != nil {
				return errResp, nil
			}
			var quota *attachments.ErrStorageQuota
			if errors.As(err, &quota) {
				// sk-674.18 T026, SC-012: the code and the fields REST answers
				// (the aggregates to a participant only).
				return skmcp.ToolError(ErrCodeStorageQuota,
					"the space has used up its file storage; free space by deleting files", quota.Extra()), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_attachments", "action", "confirm",
				"reason", "confirm failed",
				"userId", userID.String(), "attachmentId", id.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to confirm attachment", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_attachments", "action", "confirm",
			"userId", userID.String(), "attachmentId", id.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(confirmResponse{Attachment: out})
	}
}

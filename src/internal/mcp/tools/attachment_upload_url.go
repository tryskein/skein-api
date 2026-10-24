package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
	"skein-api/internal/authz"
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
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)
		if r := deps.requireAttachmentsAvailable(); r != nil {
			return r, nil
		}

		rawIssue, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		issueID := strings.TrimSpace(rawIssue)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": rawIssue}), nil
		}

		rawFilename, err := req.RequireString("filename")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "filename is required", nil), nil
		}
		filename, err := attachments.ValidateFilename(rawFilename)
		if err != nil {
			return mapAttachmentValidationToolError(err), nil
		}
		rawCT, err := req.RequireString("contentType")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "contentType is required", nil), nil
		}
		contentType, err := attachments.ValidateContentType(rawCT)
		if err != nil {
			return mapAttachmentValidationToolError(err), nil
		}
		size := req.GetInt("sizeBytes", 0)
		if size <= 0 {
			return skmcp.ToolError(ErrCodeValidationError,
				"sizeBytes must be a positive integer", nil), nil
		}
		if err := attachments.ValidateSize(int64(size), deps.maxSize); err != nil {
			return mapAttachmentValidationToolError(err), nil
		}

		var commentID *uuid.UUID
		if c := strings.TrimSpace(req.GetString("commentId", "")); c != "" {
			id, perr := uuid.Parse(c)
			if perr != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"commentId must be a valid UUID", nil), nil
			}
			commentID = &id
		}

		// Visibility — must see the parent issue's project.
		pid, errResp := loadAndCheckIssueVisibility(ctx, deps.pool, issueID, userID, role, authz.ActionWrite)
		if errResp != nil {
			return errResp, nil
		}

		id, err := uuid.NewV7()
		if err != nil {
			return skmcp.ToolError(ErrCodeInternalError, "uuid generation failed", nil), nil
		}
		s3Key := attachments.BuildS3Key(pid, issueID, id, filename)

		_, err = deps.store.CreatePending(ctx, id, issueID, commentID,
			filename, contentType, int64(size), deps.bucket, s3Key, userID)
		if err != nil {
			switch {
			case errors.Is(err, attachments.ErrIssueNotFound):
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"issueId": issueID}), nil
			case errors.Is(err, attachments.ErrCommentNotForIssue):
				return skmcp.ToolError(ErrCodeValidationError,
					"the supplied comment does not belong to this issue", nil), nil
			}
			var lim *attachments.ErrLimitExceeded
			if errors.As(err, &lim) {
				return skmcp.ToolError(ErrCodeAttachmentLimit,
					"this issue already has the maximum number of attachments",
					map[string]any{"cap": lim.Cap}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_attachments", "action", "upload_url",
				"reason", "create pending failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to create attachment row", nil), nil
		}

		policy, err := deps.s3.PresignedPostPolicy(ctx, s3Key, contentType, int64(size), deps.urlTTL)
		if err != nil {
			// Clean up the pending row so a retry doesn't pile up garbage.
			_ = deps.store.DeleteByID(ctx, id)
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_attachments", "action", "upload_url",
				"reason", "presign failed",
				"userId", userID.String(), "attachmentId", id.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mint presigned URL", nil), nil
		}

		resp := attachments.UploadURLResponse{
			AttachmentID: id,
			URL:          policy.URL,
			Fields:       policy.Fields,
			ExpiresAt:    time.Now().UTC().Add(deps.urlTTL),
			SizeBytes:    int64(size),
			MaxSizeBytes: deps.maxSize,
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_attachments", "action", "upload_url",
			"userId", userID.String(), "issueId", issueID,
			"attachmentId", id.String(), "sizeBytes", size,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(resp)
	}
}

// mapAttachmentValidationToolError adapts attachments.ValidationError to
// the MCP tool-error shape with the right error code.
func mapAttachmentValidationToolError(err error) *mcpgo.CallToolResult {
	ve, ok := attachments.IsValidationError(err)
	if !ok {
		return skmcp.ToolError(ErrCodeValidationError, err.Error(), nil)
	}
	switch ve.Code {
	case "file_too_large":
		return skmcp.ToolError(ErrCodeAttachmentTooLarge, ve.Message, nil)
	case "mime_blocked":
		return skmcp.ToolError(ErrCodeAttachmentMIMEBlocked, ve.Message, nil)
	default:
		return skmcp.ToolError(ErrCodeValidationError, ve.Message,
			map[string]any{"code": ve.Code})
	}
}

package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/attachments"
	"skein-api/internal/authz"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// attachmentDeps groups the things every R4.2 MCP tool needs. The
// s3Access pointer is allowed to be nil — when S3 is disabled the
// tools return ErrCodeAttachmentsUnavailable so the agent gets a stable,
// distinguishable error code (instead of a generic 500).
type attachmentDeps struct {
	pool    *pgxpool.Pool
	store   *attachments.Store
	s3      attachments.S3Access
	bucket  string
	maxSize int64
	urlTTL  time.Duration
}

// AttachmentDepsFor is the only public constructor — used by main.go to
// build the attachments deps bundle without importing the package's
// unexported struct directly.
func AttachmentDepsFor(
	pool *pgxpool.Pool,
	store *attachments.Store,
	s3 attachments.S3Access,
	bucket string,
	maxSize int64,
	urlTTL time.Duration,
) attachmentDeps {
	return attachmentDeps{
		pool: pool, store: store, s3: s3,
		bucket: bucket, maxSize: maxSize, urlTTL: urlTTL,
	}
}

// requireAttachmentsAvailable returns an MCP tool error if the
// attachments subsystem is disabled (S3_URL empty). Otherwise nil.
func (d *attachmentDeps) requireAttachmentsAvailable() *mcpgo.CallToolResult {
	if d.s3 == nil {
		return skmcp.ToolError(ErrCodeAttachmentsUnavailable,
			"attachments are disabled on this server (S3 not configured)", nil)
	}
	return nil
}

// loadAndCheckIssueVisibility resolves the issue's project, runs the
// caller's access check, and returns the project UUID. With no actions it
// checks visibility; with actions it is the gate of the action table of
// US-6 (sk-674.18 T066: uploading is ActionWrite). On failure returns the
// right tool-error result: issue_not_found when the project is out of
// reach, forbidden when it is visible but the level does not allow the
// action.
func loadAndCheckIssueVisibility(
	ctx context.Context,
	pool *pgxpool.Pool,
	issueID string,
	userID uuid.UUID,
	role string,
	actions ...authz.Action,
) (uuid.UUID, *mcpgo.CallToolResult) {
	pid, err := attachments.ProjectIDForIssue(ctx, pool, issueID)
	if err != nil {
		if errors.Is(err, attachments.ErrIssueNotFound) {
			return uuid.Nil, skmcp.ToolError(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"issueId": issueID})
		}
		slog.ErrorContext(ctx, "mcp tool error",
			"reason", "load issue project failed",
			"issueId", issueID, "error", err.Error())
		return uuid.Nil, skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil)
	}
	if _, err := projects.EnsureProjectAction(ctx, pool, pid, userID, actions...); err != nil {
		if errors.Is(err, projects.ErrProjectNotVisible) || errors.Is(err, projects.ErrNotFound) {
			return uuid.Nil, skmcp.ToolError(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"issueId": issueID})
		}
		if errors.Is(err, projects.ErrProjectActionForbidden) {
			return uuid.Nil, skmcp.ToolError(ErrCodeForbidden, msgActionForbidden,
				map[string]any{"issueId": issueID})
		}
		return uuid.Nil, skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil)
	}
	return pid, nil
}

// loadAttachmentChecked loads an attachment row and ensures the caller
// can see the parent issue's project — and, with actions, may perform them
// there (US-6; confirming and deleting are ActionWrite, sk-674.18 T066). On
// failure returns the right tool-error result: attachment_not_found when
// the row is missing or its project out of reach, forbidden when the
// project is visible but the level does not allow the action; on success
// returns the row + bucket + key.
func loadAttachmentChecked(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *attachments.Store,
	id uuid.UUID,
	userID uuid.UUID,
	role string,
	actions ...authz.Action,
) (*attachments.Attachment, string, string, *mcpgo.CallToolResult) {
	att, bucket, key, err := store.GetWithS3(ctx, id)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			return nil, "", "", skmcp.ToolError(ErrCodeAttachmentNotFound,
				"attachment does not exist or is not visible",
				map[string]any{"attachmentId": id.String()})
		}
		return nil, "", "", skmcp.ToolError(ErrCodeInternalError, "failed to load attachment", nil)
	}
	if _, errResp := loadAndCheckIssueVisibility(ctx, pool, att.IssueID, userID, role, actions...); errResp != nil {
		if errResp.IsError && toolErrorCode(errResp) == ErrCodeForbidden {
			return nil, "", "", skmcp.ToolError(ErrCodeForbidden, msgActionForbidden,
				map[string]any{"attachmentId": id.String()})
		}
		// Hide attachment existence — same anti-leak rule as elsewhere.
		return nil, "", "", skmcp.ToolError(ErrCodeAttachmentNotFound,
			"attachment does not exist or is not visible",
			map[string]any{"attachmentId": id.String()})
	}
	return att, bucket, key, nil
}

// mapAttachmentAccessErr handles errors that bubble up from the store's
// in-tx visibility check (the post-TOCTOU pattern in store.Confirm /
// store.Delete). When the caller's project membership has been revoked
// between the upload-url mint and the confirm/delete, store returns
// projects.ErrProjectNotVisible — we surface it as attachment_not_found
// to mirror the anti-leak rule used elsewhere. Returns nil on any other
// error (caller logs + falls through to InternalError).
func mapAttachmentAccessErr(err error) *mcpgo.CallToolResult {
	if errors.Is(err, projects.ErrProjectNotVisible) {
		return skmcp.ToolError(ErrCodeAttachmentNotFound,
			"attachment does not exist or is not visible", nil)
	}
	if errors.Is(err, projects.ErrProjectActionForbidden) {
		return skmcp.ToolError(ErrCodeForbidden, msgActionForbidden, nil)
	}
	return nil
}

// parseAttachmentID is a small helper around uuid.Parse that maps any
// failure onto a stable validation tool-error.
func parseAttachmentID(raw string) (uuid.UUID, *mcpgo.CallToolResult) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, skmcp.ToolError(ErrCodeValidationError,
			"attachmentId must be a valid UUID", nil)
	}
	return id, nil
}

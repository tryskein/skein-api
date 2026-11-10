package issueops

// attachments.go — the attachment operations (sk-878.3.1 T026, the owner's
// decision 01a1c640): the six actions of the MCP tool skein_attachments,
// moved out of it with their checks and refusal codes unchanged. A file never
// passes through the server: an upload and a download go to storage by
// presigned URLs.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/attachments"
	"skein-api/internal/authz"
	"skein-api/internal/projects"
)

// AttachmentDeps are what the attachment operations read and write through.
// S3 is nil when storage is not configured: the actions that need it refuse
// with attachments_unavailable, the metadata ones still answer.
type AttachmentDeps struct {
	Pool    *pgxpool.Pool
	Store   *attachments.Store
	S3      attachments.S3Access
	Bucket  string
	MaxSize int64
	URLTTL  time.Duration
}

const (
	attachmentTool      = "skein_attachments"
	msgAttachmentHidden = "attachment does not exist or is not visible"
)

func (d AttachmentDeps) requireAvailable() *Failure {
	if d.S3 == nil {
		return Fail(ErrCodeAttachmentsUnavailable, "attachments are disabled on this server (S3 not configured)", nil)
	}
	return nil
}

// attachmentIssueGate resolves the issue's project and checks the caller's
// access there: with no actions visibility, with actions the gate of the
// action table of US-6. issue_not_found when the project is out of reach,
// forbidden when it is visible but the level does not allow the action.
func attachmentIssueGate(ctx context.Context, pool *pgxpool.Pool, issueID string, userID uuid.UUID, actions ...authz.Action) (uuid.UUID, *Failure) {
	pid, err := attachments.ProjectIDForIssue(ctx, pool, issueID)
	if err != nil {
		if errors.Is(err, attachments.ErrIssueNotFound) {
			return uuid.Nil, Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible",
				map[string]any{"issueId": issueID})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"reason", "load issue project failed",
			"issueId", issueID, "error", err.Error())
		return uuid.Nil, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if _, err := projects.EnsureProjectAction(ctx, pool, pid, userID, actions...); err != nil {
		if errors.Is(err, projects.ErrProjectNotVisible) || errors.Is(err, projects.ErrNotFound) {
			return uuid.Nil, Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible",
				map[string]any{"issueId": issueID})
		}
		if errors.Is(err, projects.ErrProjectActionForbidden) {
			return uuid.Nil, Fail(ErrCodeForbidden, MsgActionForbidden, map[string]any{"issueId": issueID})
		}
		return uuid.Nil, Fail(ErrCodeInternalError, "visibility check failed", nil)
	}
	return pid, nil
}

// loadAttachment reads an attachment and checks the caller's access to its
// issue's project (with actions: that he may perform them there). A missing
// attachment and one of a project out of reach are one answer,
// attachment_not_found; forbidden when the project is visible but the level
// does not allow the action.
func loadAttachment(ctx context.Context, d AttachmentDeps, id, userID uuid.UUID, actions ...authz.Action) (*attachments.Attachment, string, *Failure) {
	hidden := Fail(ErrCodeAttachmentNotFound, msgAttachmentHidden, map[string]any{"attachmentId": id.String()})
	att, _, key, err := d.Store.GetWithS3(ctx, id)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			return nil, "", hidden
		}
		return nil, "", Fail(ErrCodeInternalError, "failed to load attachment", nil)
	}
	if _, f := attachmentIssueGate(ctx, d.Pool, att.IssueID, userID, actions...); f != nil {
		if f.Code == ErrCodeForbidden {
			return nil, "", Fail(ErrCodeForbidden, MsgActionForbidden, map[string]any{"attachmentId": id.String()})
		}
		return nil, "", hidden
	}
	return att, key, nil
}

// attachmentAccessFailure maps the store's in-transaction re-check (the
// caller lost the project between the read and the write) to the same
// answers as loadAttachment; nil for any other error.
func attachmentAccessFailure(err error) *Failure {
	if errors.Is(err, projects.ErrProjectNotVisible) {
		return Fail(ErrCodeAttachmentNotFound, msgAttachmentHidden, nil)
	}
	if errors.Is(err, projects.ErrProjectActionForbidden) {
		return Fail(ErrCodeForbidden, MsgActionForbidden, nil)
	}
	return nil
}

func attachmentIDArg(a Args) (uuid.UUID, *Failure) {
	raw, err := a.RequireString("attachmentId")
	if err != nil {
		return uuid.Nil, Fail(ErrCodeValidationError, "attachmentId is required", nil)
	}
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, Fail(ErrCodeValidationError, "attachmentId must be a valid UUID", nil)
	}
	return id, nil
}

func attachmentIssueArg(a Args) (string, *Failure) {
	raw, err := a.RequireString("issueId")
	if err != nil {
		return "", Fail(ErrCodeValidationError, "issueId is required", nil)
	}
	issueID := strings.TrimSpace(raw)
	if !LooksLikeHierarchicalID(issueID) {
		return "", Fail(ErrCodeValidationError,
			"issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": raw})
	}
	return issueID, nil
}

// AttachmentValidationFailure maps an attachments validator's refusal:
// file_too_large and mime_blocked are their own codes.
func AttachmentValidationFailure(err error) *Failure {
	ve, ok := attachments.IsValidationError(err)
	if !ok {
		return Fail(ErrCodeValidationError, err.Error(), nil)
	}
	switch ve.Code {
	case "file_too_large":
		return Fail(ErrCodeAttachmentTooLarge, ve.Message, nil)
	case "mime_blocked":
		return Fail(ErrCodeAttachmentMIMEBlocked, ve.Message, nil)
	}
	return Fail(ErrCodeValidationError, ve.Message, map[string]any{"code": ve.Code})
}

// AttachmentUploadURL creates a pending attachment of an issue (or of one of
// its comments) and mints the presigned upload policy for it (action
// upload_url; createAttachmentUpload). The policy binds the size and the
// content type; the file lands in storage directly, and confirm makes the
// attachment visible.
func AttachmentUploadURL(ctx context.Context, d AttachmentDeps, userID uuid.UUID, a Args) (attachments.UploadURLResponse, *Failure) {
	start := time.Now()
	var none attachments.UploadURLResponse
	if f := d.requireAvailable(); f != nil {
		return none, f
	}
	issueID, f := attachmentIssueArg(a)
	if f != nil {
		return none, f
	}
	rawFilename, err := a.RequireString("filename")
	if err != nil {
		return none, Fail(ErrCodeValidationError, "filename is required", nil)
	}
	filename, err := attachments.ValidateFilename(rawFilename)
	if err != nil {
		return none, AttachmentValidationFailure(err)
	}
	rawCT, err := a.RequireString("contentType")
	if err != nil {
		return none, Fail(ErrCodeValidationError, "contentType is required", nil)
	}
	contentType, err := attachments.ValidateContentType(rawCT)
	if err != nil {
		return none, AttachmentValidationFailure(err)
	}
	size := a.GetInt("sizeBytes", 0)
	if size <= 0 {
		return none, Fail(ErrCodeValidationError, "sizeBytes must be a positive integer", nil)
	}
	if err := attachments.ValidateSize(int64(size), d.MaxSize); err != nil {
		return none, AttachmentValidationFailure(err)
	}
	var commentID *uuid.UUID
	if c := strings.TrimSpace(a.GetString("commentId", "")); c != "" {
		id, perr := uuid.Parse(c)
		if perr != nil {
			return none, Fail(ErrCodeValidationError, "commentId must be a valid UUID", nil)
		}
		commentID = &id
	}

	pid, f := attachmentIssueGate(ctx, d.Pool, issueID, userID, authz.ActionWrite)
	if f != nil {
		return none, f
	}
	id, err := uuid.NewV7()
	if err != nil {
		return none, Fail(ErrCodeInternalError, "uuid generation failed", nil)
	}
	s3Key := attachments.BuildS3Key(pid, issueID, id, filename)
	if _, err := d.Store.CreatePending(ctx, id, issueID, commentID,
		filename, contentType, int64(size), d.Bucket, s3Key, userID); err != nil {
		switch {
		case errors.Is(err, attachments.ErrIssueNotFound):
			return none, Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible",
				map[string]any{"issueId": issueID})
		case errors.Is(err, attachments.ErrCommentNotForIssue):
			return none, Fail(ErrCodeValidationError, "the supplied comment does not belong to this issue", nil)
		}
		var lim *attachments.ErrLimitExceeded
		if errors.As(err, &lim) {
			return none, Fail(ErrCodeAttachmentLimit, "this issue already has the maximum number of attachments",
				map[string]any{"cap": lim.Cap})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", attachmentTool, "action", "upload_url",
			"reason", "create pending failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return none, Fail(ErrCodeInternalError, "failed to create attachment row", nil)
	}
	policy, err := d.S3.PresignedPostPolicy(ctx, s3Key, contentType, int64(size), d.URLTTL)
	if err != nil {
		// The pending row goes, so a retry does not pile up garbage.
		_ = d.Store.DeleteByID(ctx, id)
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", attachmentTool, "action", "upload_url",
			"reason", "presign failed",
			"userId", userID.String(), "attachmentId", id.String(), "error", err.Error())
		return none, Fail(ErrCodeInternalError, "failed to mint presigned URL", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", attachmentTool, "action", "upload_url",
		"userId", userID.String(), "issueId", issueID,
		"attachmentId", id.String(), "sizeBytes", size,
		"latencyMs", time.Since(start).Milliseconds())
	return attachments.UploadURLResponse{
		AttachmentID: id,
		URL:          policy.URL,
		Fields:       policy.Fields,
		ExpiresAt:    time.Now().UTC().Add(d.URLTTL),
		SizeBytes:    int64(size),
		MaxSizeBytes: d.MaxSize,
	}, nil
}

// AttachmentConfirm marks an uploaded attachment ready (action confirm;
// confirmAttachment): only its uploader confirms, the object must be in
// storage, and the space's storage quota is charged. Confirming a ready
// attachment is a no-op success.
func AttachmentConfirm(ctx context.Context, d AttachmentDeps, userID uuid.UUID, a Args) (*attachments.Attachment, *Failure) {
	start := time.Now()
	if f := d.requireAvailable(); f != nil {
		return nil, f
	}
	id, f := attachmentIDArg(a)
	if f != nil {
		return nil, f
	}
	att, s3Key, f := loadAttachment(ctx, d, id, userID, authz.ActionWrite)
	if f != nil {
		return nil, f
	}
	hidden := Fail(ErrCodeAttachmentNotFound, msgAttachmentHidden, map[string]any{"attachmentId": id.String()})
	if att.UploadedBy != userID {
		return nil, hidden
	}
	if att.Status == attachments.StatusPending {
		if err := d.S3.StatObject(ctx, s3Key); err != nil {
			return nil, Fail(ErrCodeAttachmentObjectMissing, "the file has not been uploaded to storage", nil)
		}
	}
	actor := AttachmentsAgentActor(ctx, d.Pool, userID)
	out, err := d.Store.Confirm(ctx, id, userID, CallerRole(ctx), actor)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			return nil, hidden
		}
		if f := attachmentAccessFailure(err); f != nil {
			return nil, f
		}
		var quota *attachments.ErrStorageQuota
		if errors.As(err, &quota) {
			// sk-674.18 T026, SC-012: the aggregates go to a participant only.
			return nil, Fail(ErrCodeStorageQuota,
				"the space has used up its file storage; free space by deleting files", quota.Extra())
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", attachmentTool, "action", "confirm",
			"reason", "confirm failed",
			"userId", userID.String(), "attachmentId", id.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to confirm attachment", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", attachmentTool, "action", "confirm",
		"userId", userID.String(), "attachmentId", id.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return out, nil
}

// AttachmentList lists the confirmed attachments of an issue (action list;
// listAttachments). It reads metadata only and works with storage
// unavailable.
func AttachmentList(ctx context.Context, d AttachmentDeps, userID uuid.UUID, a Args) (*attachments.ListResponse, *Failure) {
	start := time.Now()
	issueID, f := attachmentIssueArg(a)
	if f != nil {
		return nil, f
	}
	if _, f := attachmentIssueGate(ctx, d.Pool, issueID, userID); f != nil {
		return nil, f
	}
	res, err := d.Store.ListByIssue(ctx, issueID)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", attachmentTool, "action", "list",
			"reason", "list failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to list attachments", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", attachmentTool, "action", "list",
		"userId", userID.String(), "issueId", issueID,
		"count", len(res.Items),
		"latencyMs", time.Since(start).Milliseconds())
	return res, nil
}

// AttachmentMetadata reads one attachment (action metadata; getAttachment).
func AttachmentMetadata(ctx context.Context, d AttachmentDeps, userID uuid.UUID, a Args) (*attachments.Attachment, *Failure) {
	start := time.Now()
	id, f := attachmentIDArg(a)
	if f != nil {
		return nil, f
	}
	att, _, f := loadAttachment(ctx, d, id, userID)
	if f != nil {
		return nil, f
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", attachmentTool, "action", "metadata",
		"userId", userID.String(), "attachmentId", id.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return att, nil
}

// AttachmentGetURL mints a presigned download URL of a ready attachment
// (action get_url; getAttachmentDownloadUrl). A URL minted before a key was
// revoked or narrowed works until it expires.
func AttachmentGetURL(ctx context.Context, d AttachmentDeps, userID uuid.UUID, a Args) (attachments.DownloadURLResponse, *Failure) {
	start := time.Now()
	var none attachments.DownloadURLResponse
	if f := d.requireAvailable(); f != nil {
		return none, f
	}
	id, f := attachmentIDArg(a)
	if f != nil {
		return none, f
	}
	att, s3Key, f := loadAttachment(ctx, d, id, userID)
	if f != nil {
		return none, f
	}
	if att.Status != attachments.StatusReady {
		return none, Fail(ErrCodeAttachmentNotReady, "attachment is still pending upload confirmation", nil)
	}
	url, err := d.S3.PresignedGetURL(ctx, s3Key, att.Filename, d.URLTTL)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", attachmentTool, "action", "get_url",
			"reason", "presign failed",
			"userId", userID.String(), "attachmentId", id.String(), "error", err.Error())
		return none, Fail(ErrCodeInternalError, "failed to mint download URL", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", attachmentTool, "action", "get_url",
		"userId", userID.String(), "attachmentId", id.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return attachments.DownloadURLResponse{URL: url, ExpiresAt: time.Now().UTC().Add(d.URLTTL)}, nil
}

// AttachmentDelete hard-deletes an attachment (action delete;
// deleteAttachment) and removes its object from storage on a best-effort
// basis: an object left behind is reconciled by the cleanup worker. The
// access check comes first, so the answer does not tell an attachment of
// another project from a missing one.
func AttachmentDelete(ctx context.Context, d AttachmentDeps, userID uuid.UUID, a Args) (uuid.UUID, *Failure) {
	start := time.Now()
	id, f := attachmentIDArg(a)
	if f != nil {
		return uuid.Nil, f
	}
	if _, _, f := loadAttachment(ctx, d, id, userID, authz.ActionWrite); f != nil {
		return uuid.Nil, f
	}
	actor := AttachmentsAgentActor(ctx, d.Pool, userID)
	bucket, s3Key, err := d.Store.Delete(ctx, id, userID, CallerRole(ctx), actor)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			return uuid.Nil, Fail(ErrCodeAttachmentNotFound, msgAttachmentHidden, map[string]any{"attachmentId": id.String()})
		}
		if f := attachmentAccessFailure(err); f != nil {
			return uuid.Nil, f
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", attachmentTool, "action", "delete",
			"reason", "delete failed",
			"userId", userID.String(), "attachmentId", id.String(), "error", err.Error())
		return uuid.Nil, Fail(ErrCodeInternalError, "failed to delete attachment", nil)
	}
	if d.S3 != nil {
		if err := d.S3.RemoveObject(ctx, s3Key); err != nil {
			slog.WarnContext(ctx, "best-effort S3 delete failed",
				"bucket", bucket, "key", s3Key, "error", err.Error())
		}
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", attachmentTool, "action", "delete",
		"userId", userID.String(), "attachmentId", id.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return id, nil
}

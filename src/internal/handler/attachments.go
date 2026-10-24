package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/attachments"
	"skein-api/internal/authz"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
)

// AttachmentsHandler bundles the dependencies for every attachments
// endpoint. s3Access is nil when S3_URL is unset — every handler
// short-circuits with 503 in that case.
type AttachmentsHandler struct {
	pool     *pgxpool.Pool
	store    *attachments.Store
	s3Access attachments.S3Access
	bucket   string
	maxSize  int64
	urlTTL   time.Duration
}

// NewAttachmentsHandler wires the handler. s3Access may be nil
// (attachments disabled). bucket / maxSize / urlTTL are pulled from
// config — passing them here keeps config out of the handler imports.
// SetStorageQuotas sets SKEIN_QUOTA_PERSONAL_STORAGE_BYTES and
// SKEIN_QUOTA_SPACE_STORAGE_BYTES (nil = none) for confirmations
// (sk-674.18 T026).
func (h *AttachmentsHandler) SetStorageQuotas(personal, space *int64) {
	h.store.SetStorageQuotas(personal, space)
}

func NewAttachmentsHandler(
	pool *pgxpool.Pool,
	s3Access attachments.S3Access,
	bucket string,
	maxSize int64,
	urlTTLSeconds int,
) *AttachmentsHandler {
	return &AttachmentsHandler{
		pool:     pool,
		store:    attachments.NewStore(pool),
		s3Access: s3Access,
		bucket:   bucket,
		maxSize:  maxSize,
		urlTTL:   time.Duration(urlTTLSeconds) * time.Second,
	}
}

// attachmentsAvailable returns true iff the subsystem is up. Handlers
// that need S3 bail early when it returns false.
func (h *AttachmentsHandler) attachmentsAvailable(w http.ResponseWriter) bool {
	if h.s3Access == nil {
		writeProblem(w, http.StatusServiceUnavailable, "attachments_unavailable",
			"attachments are disabled (S3 not configured)", nil)
		return false
	}
	return true
}

// mapAttachmentValidation maps validation errors to 400/422 problem
// responses. file_too_large + mime_blocked are 422 (business rule);
// invalid_filename / invalid_content_type / invalid_size are 400
// (client supplied bad data).
func mapAttachmentValidation(w http.ResponseWriter, err error) bool {
	if ve, ok := attachments.IsValidationError(err); ok {
		switch ve.Code {
		case "file_too_large", "mime_blocked":
			writeProblem(w, http.StatusUnprocessableEntity, ve.Code, ve.Message, nil)
		default:
			writeProblem(w, http.StatusBadRequest, ve.Code, ve.Message, nil)
		}
		return true
	}
	return false
}

// UploadURL handles POST /api/v1/issues/{issueID}/attachments/upload-url.
//
// Algorithm:
//  1. Decode body, validate filename + content-type + size.
//  2. Resolve the issue's project + check visibility.
//  3. Generate attachment UUIDv7 + S3 key.
//  4. Insert attachments row in 'pending' state.
//  5. Mint a presigned POST policy bound to (key, content-type, size).
//  6. Return {attachmentId, url, fields, expiresAt}.
//
// Failure modes:
//   - 400 invalid_json / invalid_filename / invalid_content_type / invalid_size
//   - 422 file_too_large / mime_blocked / attachment_limit_exceeded
//   - 404 not_found (issue missing or invisible)
//   - 503 attachments_unavailable (S3 disabled)
//   - 500 internal_error otherwise
//
// @Summary Mint a presigned upload URL for an attachment
// @Description Validates the requested filename / content-type / size, inserts a row in 'pending' state, and returns a S3 presigned POST policy the browser uses to upload the file directly. The policy binds (key, content-type, size) — the browser cannot upload a different file. After the upload completes the client must call /attachments/{id}/confirm to flip status to 'ready'.
// @Tags Attachments
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param issueID path string true "Issue ID (hierarchical, e.g. demo-42)"
// @Param body body attachments.UploadURLRequest true "Upload request"
// @Success 200 {object} attachments.UploadURLResponse "Presigned policy + attachment id"
// @Failure 400 {object} map[string]any "invalid_json | invalid_filename | invalid_content_type | invalid_size"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; project ownership)"
// @Failure 404 {object} map[string]any "Issue not found or not visible"
// @Failure 422 {object} map[string]any "file_too_large | mime_blocked | attachment_limit_exceeded | comment_wrong_issue"
// @Failure 503 {object} map[string]any "Attachments subsystem disabled (S3 not configured)"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/issues/{issueID}/attachments/upload-url [post]
func (h *AttachmentsHandler) UploadURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.attachmentsAvailable(w) {
		return
	}
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_id", "issueID is required", nil)
		return
	}

	var req attachments.UploadURLRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	filename, err := attachments.ValidateFilename(req.Filename)
	if err != nil {
		mapAttachmentValidation(w, err)
		return
	}
	contentType, err := attachments.ValidateContentType(req.ContentType)
	if err != nil {
		mapAttachmentValidation(w, err)
		return
	}
	if err := attachments.ValidateSize(req.SizeBytes, h.maxSize); err != nil {
		mapAttachmentValidation(w, err)
		return
	}

	// Resolve the issue's project and check visibility.
	pid, err := attachments.ProjectIDForIssue(ctx, h.pool, issueID)
	if err != nil {
		if errors.Is(err, attachments.ErrIssueNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "upload-url: resolve project failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	// Generate ids + key.
	id, err := uuid.NewV7()
	if err != nil {
		slog.ErrorContext(ctx, "upload-url: uuid v7 failed", "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	s3Key := attachments.BuildS3Key(pid, issueID, id, filename)

	// Insert pending row.
	att, err := h.store.CreatePending(ctx, id, issueID, req.CommentID,
		filename, contentType, req.SizeBytes, h.bucket, s3Key, c.UserID)
	if err != nil {
		switch {
		case errors.Is(err, attachments.ErrIssueNotFound):
			writeProblem(w, http.StatusNotFound, "not_found", "issue not found", nil)
		case errors.Is(err, attachments.ErrCommentNotForIssue):
			writeProblem(w, http.StatusUnprocessableEntity, "comment_wrong_issue",
				"the supplied comment does not belong to this issue", nil)
		default:
			var lim *attachments.ErrLimitExceeded
			if errors.As(err, &lim) {
				writeProblem(w, http.StatusUnprocessableEntity, "attachment_limit_exceeded",
					"this issue already has the maximum number of attachments",
					map[string]any{"cap": lim.Cap})
				return
			}
			slog.ErrorContext(ctx, "upload-url: create pending failed",
				"issueId", issueID, "userId", c.UserID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		}
		return
	}

	// Mint presigned POST policy.
	policy, err := h.s3Access.PresignedPostPolicy(ctx, s3Key, contentType, req.SizeBytes, h.urlTTL)
	if err != nil {
		// Best-effort: remove the just-inserted pending row so we don't
		// leak it. The cleanup goroutine would catch it eventually but
		// the user retry should not pile up garbage.
		_ = h.store.DeleteByID(ctx, id)
		slog.ErrorContext(ctx, "upload-url: presign failed",
			"issueId", issueID, "attachmentId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	resp := attachments.UploadURLResponse{
		AttachmentID: att.ID,
		URL:          policy.URL,
		Fields:       policy.Fields,
		ExpiresAt:    time.Now().UTC().Add(h.urlTTL),
		SizeBytes:    req.SizeBytes,
		MaxSizeBytes: h.maxSize,
	}
	slog.InfoContext(ctx, "attachment upload-url issued",
		"userId", c.UserID.String(), "issueId", issueID,
		"attachmentId", id.String(), "sizeBytes", req.SizeBytes)
	writeJSON(w, http.StatusOK, resp)
}

// Confirm handles POST /api/v1/attachments/{id}/confirm.
//
// Algorithm:
//  1. Load the attachment row + verify caller is the uploader.
//  2. Re-check the object exists in S3 (defends against the
//     browser receiving a presigned URL but never POSTing).
//  3. Flip status to 'ready'.
//  4. Emit attachment.added event + fan-out to watchers.
//
// Idempotent: a second call against an already-ready row returns the
// existing state without re-emitting the event.
//
// @Summary Confirm an upload completed and flip status to 'ready'
// @Description Verifies the object actually exists in S3, flips the row from 'pending' to 'ready', emits an attachment.added event, and fans out notifications to issue + project watchers. Only the original uploader can call this. Idempotent — a second call against a 'ready' row is a no-op.
// @Tags Attachments
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Attachment UUID"
// @Success 200 {object} attachments.Attachment "Attachment in 'ready' state"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; project ownership)"
// @Failure 404 {object} map[string]any "Attachment not found, not owned by caller, or project not visible"
// @Failure 422 {object} map[string]any "object_not_uploaded — the file never landed in storage; storage_quota — the space's attachment storage reached SKEIN_QUOTA_*_STORAGE_BYTES (soft: confirmed while used < quota, even if this file takes it over; reading, downloading and deleting stay allowed; spaceId, storageUsedBytes, storageQuotaBytes only for a participant of the space, an external user gets the code alone — INV-28 (3))"
// @Failure 503 {object} map[string]any "Attachments subsystem disabled (S3 not configured)"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/attachments/{id}/confirm [post]
func (h *AttachmentsHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.attachmentsAvailable(w) {
		return
	}
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}

	// Load + visibility check in one read-only tx (prevents TOCTOU
	// against a concurrent project_members revoke).
	att, _, s3Key, err := h.store.GetWithS3AndVisibility(ctx, id, c.UserID, c.Role)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "attachment not found", nil)
			return
		}
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "confirm: load attachment failed",
			"attachmentId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if att.UploadedBy != c.UserID {
		// Hide existence to mirror the visibility-leak rule used
		// elsewhere in the codebase (issues, projects).
		writeProblem(w, http.StatusNotFound, "not_found", "attachment not found", nil)
		return
	}

	// S3 existence probe — only when transitioning pending→ready.
	// Confirms against an already-ready row skip the probe (idempotent).
	if att.Status == attachments.StatusPending {
		if err := h.s3Access.StatObject(ctx, s3Key); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "object_not_uploaded",
				"the file has not been uploaded to storage", nil)
			return
		}
	}

	// Confirm re-checks visibility inside its own SERIALIZABLE tx so
	// the membership recheck is atomic with the row flip.
	out, err := h.store.Confirm(ctx, id, c.UserID, c.Role, attachments.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	})
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "attachment not found", nil)
			return
		}
		if mapAccessErr(w, err) {
			return
		}
		var quota *attachments.ErrStorageQuota
		if errors.As(err, &quota) {
			// sk-674.18 T026, INV-15: the soft storage quota of the space;
			// its aggregates to a participant only (INV-28 (3)).
			writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeStorageQuota,
				"the space has used up its file storage; free space by deleting files", quota.Extra())
			return
		}
		slog.ErrorContext(ctx, "confirm: store failed",
			"attachmentId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "attachment confirmed",
		"userId", c.UserID.String(), "attachmentId", id.String(), "issueId", out.IssueID)
	writeJSON(w, http.StatusOK, out)
}

// List handles GET /api/v1/issues/{issueID}/attachments. Returns every
// 'ready' attachment for the issue.
//
// @Summary List attachments for an issue
// @Description Returns every 'ready' attachment for the issue (pending uploads-in-flight are hidden — they're an internal lifecycle state). Visibility filter: caller must see the parent project.
// @Tags Attachments
// @Produce json
// @Security ApiKeyAuth
// @Param issueID path string true "Issue ID (hierarchical, e.g. demo-42)"
// @Success 200 {object} attachments.ListResponse "List of ready attachments"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; project ownership)"
// @Failure 404 {object} map[string]any "Issue not found or not visible"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/issues/{issueID}/attachments [get]
func (h *AttachmentsHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_id", "issueID is required", nil)
		return
	}
	pid, err := attachments.ProjectIDForIssue(ctx, h.pool, issueID)
	if err != nil {
		if errors.Is(err, attachments.ErrIssueNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "issue not found", nil)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	resp, err := h.store.ListByIssue(ctx, issueID)
	if err != nil {
		slog.ErrorContext(ctx, "list attachments failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// DownloadURL handles GET /api/v1/attachments/{id}/download-url.
// Returns a short-lived presigned GET URL the browser can hit directly.
//
// Visibility: caller must see the parent issue.
//
// @Summary Mint a short-lived presigned download URL
// @Description Returns a presigned S3 GET URL the browser opens directly to download the file. The URL embeds a Content-Disposition header so the browser saves under the original filename (RFC 6266 / 5987 — UTF-8 filenames are properly encoded). TTL matches SKEIN_ATTACHMENT_URL_TTL_SECONDS.
// @Tags Attachments
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Attachment UUID"
// @Success 200 {object} attachments.DownloadURLResponse "Presigned URL + expiry"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; project ownership)"
// @Failure 404 {object} map[string]any "Attachment not found or project not visible"
// @Failure 422 {object} map[string]any "not_ready — attachment still in pending state"
// @Failure 503 {object} map[string]any "Attachments subsystem disabled (S3 not configured)"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/attachments/{id}/download-url [get]
func (h *AttachmentsHandler) DownloadURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.attachmentsAvailable(w) {
		return
	}
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	// Load + visibility check in one read-only tx — closes the TOCTOU
	// against a concurrent project_members revoke.
	att, _, s3Key, err := h.store.GetWithS3AndVisibility(ctx, id, c.UserID, c.Role)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "attachment not found", nil)
			return
		}
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "download-url: load attachment failed",
			"attachmentId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if att.Status != attachments.StatusReady {
		writeProblem(w, http.StatusUnprocessableEntity, "not_ready",
			"attachment is still pending upload confirmation", nil)
		return
	}

	url, err := h.s3Access.PresignedGetURL(ctx, s3Key, att.Filename, h.urlTTL)
	if err != nil {
		slog.ErrorContext(ctx, "download-url: presign failed",
			"attachmentId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, attachments.DownloadURLResponse{
		URL:       url,
		ExpiresAt: time.Now().UTC().Add(h.urlTTL),
	})
}

// Delete handles DELETE /api/v1/attachments/{id}. Hard-deletes the row +
// best-effort removes the S3 object. Anyone with project visibility can
// delete (R4.2 plan does not restrict to uploader).
//
// @Summary Delete an attachment
// @Description Hard-deletes the row and best-effort removes the S3 object. Emits an attachment.removed event (audit-only — not in the notifiable set, so no inbox spam). Anyone with project visibility can delete; not restricted to the uploader. If S3 delete fails the row is still gone — the cleanup goroutine reconciles the orphan object on its next pass.
// @Tags Attachments
// @Security ApiKeyAuth
// @Param id path string true "Attachment UUID"
// @Success 204 "Deleted"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; project ownership)"
// @Failure 404 {object} map[string]any "Attachment not found or project not visible"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/attachments/{id} [delete]
func (h *AttachmentsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	// Read-only first to surface 404 when the row is already gone, so
	// the response shape matches the rest of the handlers without a
	// special-case in Delete. The actual visibility check happens
	// inside the SERIALIZABLE tx of store.Delete (see TOCTOU note in
	// the store doc-comment) so a concurrent revoke can't slip in.
	att, err := h.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "attachment not found", nil)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	bucket, s3Key, err := h.store.Delete(ctx, id, c.UserID, c.Role, attachments.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	})
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "attachment not found", nil)
			return
		}
		if mapAccessErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "delete attachment failed",
			"attachmentId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	// Best-effort S3 delete. If S3 is unreachable here we log and
	// move on — the row is already gone from the DB and the cleanup
	// goroutine will reconcile the orphan object on its next pass.
	if h.s3Access != nil {
		if err := h.s3Access.RemoveObject(ctx, s3Key); err != nil {
			slog.WarnContext(ctx, "best-effort S3 delete failed",
				"bucket", bucket, "key", s3Key, "error", err.Error())
		}
	}

	slog.InfoContext(ctx, "attachment deleted",
		"userId", c.UserID.String(), "attachmentId", id.String(), "issueId", att.IssueID)
	w.WriteHeader(http.StatusNoContent)
}

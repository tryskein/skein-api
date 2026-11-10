package publicapi

// attachments.go — the attachment operations of the public API (spec
// sk-878.3.1, "API surface", T026): thin adapters over the operations of
// internal/issueops that the actions of the MCP tool skein_attachments also
// call (INV-4). A file never passes through the API: it is uploaded to and
// downloaded from storage by presigned URLs, and a URL handed out before a
// key was revoked or narrowed works until it expires. Storage not
// configured or unavailable is 503 attachments_unavailable (D-11).

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/attachments"
	"skein-api/internal/errcode"
	"skein-api/internal/issueops"
)

// The operationIds of the attachment operations.
const (
	OperationListAttachments          = "listAttachments"
	OperationCreateAttachmentUpload   = "createAttachmentUpload"
	OperationConfirmAttachment        = "confirmAttachment"
	OperationGetAttachment            = "getAttachment"
	OperationGetAttachmentDownloadURL = "getAttachmentDownloadUrl"
	OperationDeleteAttachment         = "deleteAttachment"
)

// attachmentUploadBody is the body of createAttachmentUpload.
type attachmentUploadBody struct {
	// Filename, kept as Content-Disposition on download. Required.
	Filename *string `json:"filename,omitempty"`
	// ContentType, the MIME type of the file; re-checked when the upload is
	// confirmed. Required.
	ContentType *string `json:"contentType,omitempty"`
	// SizeBytes, the exact size of the file: the upload policy binds it.
	// Required, 1..the instance limit.
	SizeBytes *int64 `json:"sizeBytes,omitempty"`
	// CommentID scopes the attachment to a comment of the issue.
	CommentID *string `json:"commentId,omitempty"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// AttachmentDeleted is the answer of deleteAttachment.
type AttachmentDeleted struct {
	// ID of the attachment.
	ID string `json:"id"`
	// Deleted is always true.
	Deleted bool `json:"deleted"`
}

// pathAttachment is the {attachment} of the path; a value that is not a
// UUID names no attachment (404 attachment_not_found).
func pathAttachment(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(chi.URLParam(r, "attachment"))
	if _, err := uuid.Parse(raw); err != nil {
		WriteProblem(w, Problem{Code: errcode.AttachmentNotFound, Origin: OriginPath,
			Detail: "attachment does not exist or is not visible", Extensions: map[string]any{"attachmentId": raw}})
		return "", false
	}
	return raw, true
}

// listAttachments lists the attachments of an issue.
//
// @Summary List the attachments of an issue
// @Description Returns every confirmed attachment of the issue of the path — the same operation as the MCP tool skein_attachments, action list: filename, content type, size, status, the uploader and the comment it belongs to, if any. Unconfirmed uploads are not listed. The set of an issue is bounded by the per-issue limit, so the list is one page (hasMore false). It reads metadata only and answers with storage unavailable. Read-only.
// @Tags Attachments
// @Produce json
// @Security SkeinApiKey[read]
// @Param issue path string true "The issue's hierarchical id"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[attachments.Attachment] "The attachments"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/attachments [get]
// @ID listAttachments
func (a *issueWrites) listAttachments(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	res, f := issueops.AttachmentList(r.Context(), a.attachments, callerOf(r), issueops.Args{"issueId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	items := res.Items
	if items == nil {
		items = []attachments.Attachment{}
	}
	WriteJSON(w, r, http.StatusOK, List[attachments.Attachment]{Items: items})
}

// createAttachmentUpload creates a pending attachment and its upload policy.
//
// @Summary Start an attachment upload
// @Description Creates a pending attachment of the issue of the path (or of one of its comments) and answers the presigned upload policy for it — the same operation as the MCP tool skein_attachments, action upload_url. Upload the file to url as a multipart POST with every form field of fields plus the file, before expiresAt; then confirmAttachment makes it visible. The policy binds the size and the content type; over the instance limit (maxSizeBytes) is 422 attachment_too_large, an executable file 422 attachment_mime_blocked. The answer always carries the upload URL: Prefer does not shorten it.
// @Description
// @Description The owner of the key must be able to write in the issue's project; an attachment is not guarded by the lock. Side effects: a pending attachment row, no event until it is confirmed.
// @Tags Attachments
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body attachmentUploadBody true "The file to upload"
// @Success 201 {object} attachments.UploadURLResponse "The pending attachment and its upload policy"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "A value is missing or invalid, or the comment is not of this issue (code validation_error); too large (attachment_too_large); an executable type (attachment_mime_blocked); the issue holds the maximum number of attachments (attachment_limit_exceeded)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "Storage is not configured or unavailable (code attachments_unavailable, retryable); the database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/attachments [post]
// @ID createAttachmentUpload
func (a *issueWrites) createAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body attachmentUploadBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["issueId"] = id
	res, f := issueops.AttachmentUploadURL(r.Context(), a.attachments, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	w.Header().Set("Location", PathPrefix+"attachments/"+res.AttachmentID.String())
	WriteJSON(w, r, http.StatusCreated, res)
}

// confirmAttachment confirms an uploaded attachment.
//
// @Summary Confirm an attachment upload
// @Description Marks the attachment of the path ready once its file is in storage — the same operation as the MCP tool skein_attachments, action confirm: only the uploader confirms (anyone else gets 404, as for a missing attachment); the file must have landed (422 attachment_object_missing otherwise); its size is charged to the space's storage (422 storage_quota over the quota). Confirming a ready attachment succeeds again without a new event.
// @Description
// @Description Side effects: attachment.added; notifications of the watchers.
// @Tags Attachments
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param attachment path string true "The attachment's UUID"
// @Param body body kindBody false "Options"
// @Success 200 {object} attachments.Attachment "The attachment, ready"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such attachment within reach (code attachment_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "The file is not in storage (code attachment_object_missing); the space's storage is used up (storage_quota)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "Storage is not configured or unavailable (code attachments_unavailable, retryable); the database is unavailable (code database_unavailable)"
// @Router /api/v1/attachments/{attachment}/confirm [post]
// @ID confirmAttachment
func (a *issueWrites) confirmAttachment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathAttachment(w, r)
	if !ok {
		return
	}
	var body kindBody
	if _, ok := decodeArgs(w, r, &body); !ok || !kindOf(w, body.Kind) {
		return
	}
	res, f := issueops.AttachmentConfirm(r.Context(), a.attachments, callerOf(r), issueops.Args{"attachmentId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, res)
}

// getAttachment reads one attachment.
//
// @Summary Get an attachment
// @Description Returns the metadata of the attachment of the path — the same operation as the MCP tool skein_attachments, action metadata: filename, content type, size, status, the uploader. An attachment of a project out of reach answers as a missing one. Read-only.
// @Tags Attachments
// @Produce json
// @Security SkeinApiKey[read]
// @Param attachment path string true "The attachment's UUID"
// @Param fields query string false "Comma-separated fields to return (plus id)"
// @Success 200 {object} attachments.Attachment "The attachment"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such attachment within reach (code attachment_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/attachments/{attachment} [get]
// @ID getAttachment
func (a *issueWrites) getAttachment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathAttachment(w, r)
	if !ok {
		return
	}
	res, f := issueops.AttachmentMetadata(r.Context(), a.attachments, callerOf(r), issueops.Args{"attachmentId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, res)
}

// getAttachmentDownloadURL mints a download URL of an attachment.
//
// @Summary Get the download URL of an attachment
// @Description Mints a presigned download URL of the ready attachment of the path — the same operation as the MCP tool skein_attachments, action get_url. The URL works until expiresAt, also after the key is revoked or narrowed. A pending attachment is 422 attachment_not_ready. Read-only: nothing is written.
// @Tags Attachments
// @Produce json
// @Security SkeinApiKey[read]
// @Param attachment path string true "The attachment's UUID"
// @Param fields query string false "Comma-separated fields to return"
// @Success 200 {object} attachments.DownloadURLResponse "The download URL"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such attachment within reach (code attachment_not_found)"
// @Failure 422 {object} ProblemDocument "The upload is not confirmed yet (code attachment_not_ready)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "Storage is not configured or unavailable (code attachments_unavailable, retryable); the database is unavailable (code database_unavailable)"
// @Router /api/v1/attachments/{attachment}/download-url [get]
// @ID getAttachmentDownloadUrl
func (a *issueWrites) getAttachmentDownloadURL(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathAttachment(w, r)
	if !ok {
		return
	}
	res, f := issueops.AttachmentGetURL(r.Context(), a.attachments, callerOf(r), issueops.Args{"attachmentId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, res)
}

// deleteAttachment deletes an attachment.
//
// @Summary Delete an attachment
// @Description Hard-deletes the attachment of the path — the same operation as the MCP tool skein_attachments, action delete — and removes its file from storage on a best-effort basis (a file left behind is removed by the cleanup worker). The owner of the key must be able to write in the issue's project; an attachment of a project out of reach answers as a missing one. It works with storage unavailable.
// @Description
// @Description Side effects: attachment.removed; notifications of the watchers; the space's storage is released.
// @Tags Attachments
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param attachment path string true "The attachment's UUID"
// @Param body body kindBody false "Options"
// @Success 200 {object} AttachmentDeleted "The attachment deleted"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such attachment within reach (code attachment_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/attachments/{attachment} [delete]
// @ID deleteAttachment
func (a *issueWrites) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathAttachment(w, r)
	if !ok {
		return
	}
	var body kindBody
	if _, ok := decodeArgs(w, r, &body); !ok || !kindOf(w, body.Kind) {
		return
	}
	res, f := issueops.AttachmentDelete(r.Context(), a.attachments, callerOf(r), issueops.Args{"attachmentId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, AttachmentDeleted{ID: res.String(), Deleted: true})
}

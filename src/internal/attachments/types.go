// Package attachments implements R4.2 — file attachments stored in S3
// (S3-compatible) referenced by metadata rows in the attachments table.
//
// Browser uploads via presigned POST policies — the API never proxies
// file bytes. Lifecycle:
//
//  1. POST /api/v1/issues/{id}/attachments/upload-url
//     → server inserts row with status='pending', returns {policy, fields}.
//  2. Browser POSTs the file directly to S3 using {policy, fields}.
//  3. POST /api/v1/attachments/{id}/confirm
//     → server flips status to 'ready' (after re-checking object exists).
//  4. GET /api/v1/issues/{id}/attachments — list ready attachments.
//  5. GET /api/v1/attachments/{id}/download-url — presigned GET for the
//     browser.
//  6. DELETE /api/v1/attachments/{id} — hard-deletes row + best-effort
//     S3 delete.
//
// Stale 'pending' rows (older than ~1h) are reaped by the cleanup
// goroutine. Orphan S3 objects (object exists but row was removed) are
// reconciled in the same pass.
package attachments

import (
	"time"

	"github.com/google/uuid"
)

// Status values for attachments.status column. Closed set; the DB CHECK
// rejects everything else.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
)

// Attachment is the wire DTO returned by every attachment endpoint. It
// mirrors the attachments table row plus a derived field (uploaderUsername)
// the UI renders next to the row.
type Attachment struct {
	// ID is the attachment's own UUID (uuid.NewV7). Not the S3 key.
	ID uuid.UUID `json:"id"`
	// IssueID is the parent issue (always present).
	IssueID string `json:"issueId"`
	// CommentID is set only when the attachment is scoped to a comment.
	// JSON omits the field when nil so issue-level callers don't see it.
	CommentID *uuid.UUID `json:"commentId,omitempty"`
	// Filename is the user-supplied original name (kept verbatim — used
	// in the Content-Disposition header on download).
	Filename string `json:"filename"`
	// ContentType is the MIME type the browser declared at upload time.
	// Re-validated against the deny-list before issuing the presigned URL.
	ContentType string `json:"contentType"`
	// SizeBytes is the file size in bytes (capped by config — see
	// Config.MaxSizeBytes).
	SizeBytes int64 `json:"sizeBytes"`
	// Status is one of: pending | ready (closed set).
	Status string `json:"status"`
	// UploadedBy is the user UUID (FK users.id, RESTRICT).
	UploadedBy uuid.UUID `json:"uploadedBy"`
	// UploadedByUsername is the joined display name (best-effort —
	// nil if the user row was deleted or absent).
	UploadedByUsername *string `json:"uploadedByUsername,omitempty"`
	// UploadedAt is the row creation timestamp (UTC).
	UploadedAt time.Time `json:"uploadedAt"`
}

// UploadURLRequest is the JSON body for POST .../upload-url.
type UploadURLRequest struct {
	// Filename is required: the original file name, used as the trailing
	// component of the S3 key (filename collisions across attachments are
	// fine because the UUID prefix already disambiguates).
	Filename string `json:"filename"`
	// ContentType is the MIME type the browser will send. Required —
	// re-validated against the deny-list.
	ContentType string `json:"contentType"`
	// SizeBytes is the user-declared file size. Validated against the
	// configured cap.
	SizeBytes int64 `json:"sizeBytes"`
	// CommentID is optional: when set, the attachment is scoped to a
	// specific comment under the issue. R4.2 backend supports this end-to-
	// end; the frontend ships only issue-level upload UI.
	CommentID *uuid.UUID `json:"commentId,omitempty"`
}

// UploadURLResponse is what the API returns to the browser after
// minting a presigned POST. The browser submits a multipart/form-data
// POST to URL with every entry from Fields plus a final "file" entry
// holding the file bytes.
type UploadURLResponse struct {
	// AttachmentID is the row's UUID — pass it back to the confirm
	// endpoint after a successful upload.
	AttachmentID uuid.UUID `json:"attachmentId"`
	// URL is the absolute S3 endpoint the browser should POST to
	// (rendered with S3_PUBLIC_URL when set).
	URL string `json:"url"`
	// Fields is the form-data fields (key, policy, signature, etc.) the
	// browser must include in its multipart POST. Order does not matter.
	Fields map[string]string `json:"fields"`
	// ExpiresAt is the absolute time the presigned URL stops working.
	// The browser should restart from upload-url if it fires later.
	ExpiresAt time.Time `json:"expiresAt"`
	// SizeBytes echoes the request value so the client doesn't have to
	// remember it for the confirm step.
	SizeBytes int64 `json:"sizeBytes"`
	// MaxSizeBytes is the API-configured cap (informational — already
	// baked into the policy). UIs use it to decide whether to even
	// attempt the upload.
	MaxSizeBytes int64 `json:"maxSizeBytes"`
}

// DownloadURLResponse mirrors UploadURLResponse for the GET path.
type DownloadURLResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ListResponse wraps GET /issues/{id}/attachments. Cursor is reserved
// for future paging; R4.2 returns the entire attachment set per issue
// (capped by per-issue limit at the store level).
type ListResponse struct {
	Items []Attachment `json:"items"`
	Total int          `json:"total"`
}

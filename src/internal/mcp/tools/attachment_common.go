package tools

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/attachments"
	"skein-api/internal/issueops"
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

// ops is the bundle the attachment operations take.
func (d attachmentDeps) ops() issueops.AttachmentDeps {
	return issueops.AttachmentDeps{
		Pool: d.pool, Store: d.store, S3: d.s3,
		Bucket: d.bucket, MaxSize: d.maxSize, URLTTL: d.urlTTL,
	}
}

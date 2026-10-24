-- 000014_attachments — R4.2 file attachments via MinIO presigned URLs.
--
-- Schema is verbatim from the database schema document (R4.2 — table 2.16).
--
-- Lifecycle:
--   1. Client calls POST /issues/{id}/attachments/upload-url → row inserted
--      with status='pending', server returns presigned POST policy.
--   2. Browser uploads directly to MinIO using the policy.
--   3. Client calls POST /attachments/{id}/confirm → status flips to 'ready'.
--   4. The R4.2e cleanup goroutine periodically deletes stale 'pending' rows
--      (older than 1h — presigned URLs expire after 10 min) and reconciles
--      orphan objects in MinIO whose row was hard-deleted.
--
-- Hard delete: rows are removed via DELETE (no deleted_at column). The S3
-- object is best-effort deleted in the same handler; orphans are swept by
-- the cleanup goroutine.
--
-- Naming notes:
--   - issue_id is varchar(1000) to match issues.id (hierarchical text key).
--   - comment_id is nullable: NULL = attachment scoped to the issue itself;
--     non-NULL = attachment scoped to a specific comment.
--   - uploaded_by is RESTRICT (audit integrity) — admin must reassign or
--     hard-delete attachments before they can deactivate the user.
--   - UNIQUE (s3_bucket, s3_key) blocks accidental double-write into the
--     same key (defence against retried client confirms with a colliding key).

CREATE TABLE attachments (
    id              uuid PRIMARY KEY,
    issue_id        varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    comment_id      uuid REFERENCES comments(id) ON DELETE CASCADE,
    filename        text NOT NULL,
    content_type    text NOT NULL,
    size_bytes      bigint NOT NULL CHECK (size_bytes >= 0),
    s3_bucket       text NOT NULL,
    s3_key          text NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','ready')),
    uploaded_by     uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    uploaded_at     timestamptz NOT NULL DEFAULT now(),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (s3_bucket, s3_key)
);

CREATE INDEX idx_attachments_issue ON attachments (issue_id);
CREATE INDEX idx_attachments_comment ON attachments (comment_id) WHERE comment_id IS NOT NULL;
-- Pending index drives the cleanup goroutine: presigned URLs expire after
-- 10 min, so any row in 'pending' state older than ~1h is abandoned and
-- can be hard-deleted along with its S3 object (if it ever made it there).
CREATE INDEX idx_attachments_pending ON attachments (uploaded_at) WHERE status = 'pending';
-- FK index for users.uploaded_by RESTRICT scan — without it, deactivating
-- a user with attachments triggers a seq-scan of the attachments table.
CREATE INDEX idx_attachments_uploaded_by ON attachments (uploaded_by);

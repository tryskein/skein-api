package attachments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
)

// pgSerializationFailure is SQLSTATE 40001 — a SERIALIZABLE transaction
// loser. Aligned with internal/issues/store.go and notifications/store.go.
const pgSerializationFailure = "40001"

// pgDeadlockDetected is SQLSTATE 40P01.
const pgDeadlockDetected = "40P01"

// isSerializationFailure reports whether the error wraps a Postgres
// 40001 / 40P01 SQLSTATE — both safe to retry without re-validating
// inputs.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
}

// withSerializableRetry runs fn inside a SERIALIZABLE transaction and
// retries up to 3 times on SQLSTATE 40001/40P01 with a small jittered
// backoff. The body is idempotent in our usage: every caller re-reads
// the row at the top of fn, so a retry sees fresh state.
//
// Project convention so far has been to map 40001 to a domain error
// (issues→ErrVersionConflict, users→ErrLastAdmin), but attachments has
// no such useful mapping — concurrent uploaders should not see a 500.
// 3 attempts × ~25ms jitter is enough to absorb the realistic
// contention surface (per-issue cap counter on parallel upload-url
// mints) without burning a request budget.
func withSerializableRetry(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	const maxAttempts = 3
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = signals.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if err == nil {
			return nil
		}
		if !isSerializationFailure(err) {
			return err
		}
		// Last attempt — surface the failure verbatim.
		if attempt == maxAttempts-1 {
			break
		}
		// Jittered backoff: 0..25ms × (attempt+1). Tiny so retries don't
		// stack into a noticeable user-visible latency.
		base := 25 * time.Millisecond * time.Duration(attempt+1)
		jitter := time.Duration(rand.Int64N(int64(25 * time.Millisecond)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base + jitter):
		}
	}
	return err
}

// ErrNotFound is returned when an attachment row does not exist.
var ErrNotFound = errors.New("attachment not found")

// ErrIssueNotFound is returned by upload-url path when the parent issue
// row is missing or not visible to the caller.
var ErrIssueNotFound = errors.New("issue not found")

// ErrCommentNotForIssue is returned when an upload references a
// commentId whose parent issue does not match the attachment's issueId.
var ErrCommentNotForIssue = errors.New("comment does not belong to this issue")

// ErrLimitExceeded surfaces the per-issue cap (50 soft / 100 hard).
// The spec uses the same code ("attachment_limit_exceeded") for both
// — soft mode is reserved for future UI nags.
type ErrLimitExceeded struct{ Cap int }

func (e *ErrLimitExceeded) Error() string {
	return fmt.Sprintf("attachment limit exceeded (cap=%d)", e.Cap)
}

// Per-issue caps. Hard cap aborts the upload; soft cap is informational
// (R4.2 plan G-6). The caller may surface the soft hint in the UI.
const (
	SoftPerIssueCap = 50
	HardPerIssueCap = 100
)

// Store wraps the pool for attachments operations. Mirrors
// internal/issues/store.go shape so callers learn one pattern.
type Store struct {
	pool *pgxpool.Pool
	// quotaPersonalBytes / quotaSpaceBytes are
	// SKEIN_QUOTA_PERSONAL_STORAGE_BYTES / SKEIN_QUOTA_SPACE_STORAGE_BYTES
	// (nil = no quota), set by SetStorageQuotas (sk-674.18 T026).
	quotaPersonalBytes *int64
	quotaSpaceBytes    *int64
}

// NewStore returns a Store backed by the given pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetStorageQuotas sets the attachment storage quotas of the personal space
// and of every other space (the common one included); nil = no quota.
func (s *Store) SetStorageQuotas(personal, space *int64) {
	s.quotaPersonalBytes, s.quotaSpaceBytes = personal, space
}

// ErrStorageQuota — sk-674.18 T026, INV-15, US-14: the attachment storage
// of the space reached its quota. The quota is soft (plan 4.5, E-11): an
// upload is confirmed while the space's used bytes are strictly below the
// quota — even if this file takes it over — and refused after; reading,
// downloading and deleting are never refused. Answered with
// spaces.CodeStorageQuota, the same in REST and MCP; the numbers and the
// space only to a participant of the space (Disclosable).
type ErrStorageQuota struct {
	SpaceID           string // short id
	StorageUsedBytes  int64
	StorageQuotaBytes int64
	// Disclosable — the caller is a participant of the space, to whom
	// INV-28 (3) gives its aggregates. An external user, a member of one of
	// its projects only, is not: the used bytes would tell
	// him the volume of projects he does not see.
	Disclosable bool
}

// Extra is the extra fields of the refusal for its caller: the space and
// its aggregates to a participant, nothing to anyone else.
func (e *ErrStorageQuota) Extra() map[string]any {
	if !e.Disclosable {
		return nil
	}
	return map[string]any{"spaceId": e.SpaceID, "storageUsedBytes": e.StorageUsedBytes,
		"storageQuotaBytes": e.StorageQuotaBytes}
}

func (e *ErrStorageQuota) Error() string {
	return fmt.Sprintf("storage quota of space %s reached: %d of %d bytes", e.SpaceID, e.StorageUsedBytes, e.StorageQuotaBytes)
}

// adjustStorage moves the K-8 counter of the space of project by delta
// bytes, inside tx: the counter row is created on the first confirmed
// upload and never goes below zero.
func adjustStorage(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, delta int64) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO space_storage_usage (space_id, bytes, updated_at)
SELECT p.space_id, GREATEST($2::bigint, 0), now() FROM projects p WHERE p.id = $1
ON CONFLICT (space_id) DO UPDATE
   SET bytes = GREATEST(space_storage_usage.bytes + $2::bigint, 0), updated_at = now()`,
		projectID, delta); err != nil {
		return fmt.Errorf("updating the space storage counter: %w", err)
	}
	return nil
}

// CreatePending inserts a new row in 'pending' state. The S3 key and
// bucket are computed by the caller and stored verbatim — the same
// values must be passed to the presigned-URL minting step so the
// browser uploads to the same path we wrote down.
//
// Returns the attachment id (caller-generated v7 uuid). Returns
// ErrIssueNotFound when the issue row is missing, ErrCommentNotForIssue
// when commentID is set but doesn't belong to issueID, and
// *ErrLimitExceeded when the issue already has HardPerIssueCap
// 'ready' rows. The cap counts ready-only so flaky clients flooding
// pending uploads can't lock honest users out — abandoned pending
// rows are reaped by the cleanup goroutine. ListByIssue surfaces the
// same ready-only set, so the UI's "X/Y" hint stays consistent with
// what the cap actually enforces.
func (s *Store) CreatePending(
	ctx context.Context,
	id uuid.UUID,
	issueID string,
	commentID *uuid.UUID,
	filename string,
	contentType string,
	sizeBytes int64,
	bucket string,
	s3Key string,
	uploadedBy uuid.UUID,
) (*Attachment, error) {
	var att Attachment
	err := withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// Verify the issue exists. We don't need any column from the
		// issues row here — Confirm/Delete do their own project_id
		// look-up when emitting events. SELECT 1 keeps the round-trip
		// off the hot path of every upload-url mint.
		var exists bool
		err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM issues WHERE id = $1)`, issueID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("loading issue for attachment: %w", err)
		}
		if !exists {
			return ErrIssueNotFound
		}

		// Verify commentId, when set, belongs to this issue. Without
		// this check a malicious agent could attach a file to issue X
		// while pretending it's under comment Y in some other issue.
		if commentID != nil {
			var commentIssue string
			err := tx.QueryRow(ctx,
				`SELECT issue_id FROM comments WHERE id = $1`,
				*commentID).Scan(&commentIssue)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCommentNotForIssue
			}
			if err != nil {
				return fmt.Errorf("loading comment for attachment: %w", err)
			}
			if commentIssue != issueID {
				return ErrCommentNotForIssue
			}
		}

		// Per-issue cap. Counts ready-only so flooding pending rows
		// (flaky client retries) can't lock honest users out — pending
		// is reaped by the cleanup goroutine. ListByIssue surfaces the
		// same ready-only set so UI counters stay consistent.
		var existing int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM attachments WHERE issue_id = $1 AND status = 'ready'`,
			issueID).Scan(&existing); err != nil {
			return fmt.Errorf("counting issue attachments: %w", err)
		}
		if existing >= HardPerIssueCap {
			return &ErrLimitExceeded{Cap: HardPerIssueCap}
		}

		_, err = tx.Exec(ctx, `
INSERT INTO attachments (
    id, issue_id, comment_id, filename, content_type, size_bytes,
    s3_bucket, s3_key, status, uploaded_by, uploaded_at, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, 'pending', $9, now(), '{}'::jsonb
)`,
			id, issueID, commentID, filename, contentType, sizeBytes,
			bucket, s3Key, uploadedBy)
		if err != nil {
			return fmt.Errorf("inserting attachment row: %w", err)
		}

		// Fill out the DTO from the inputs we already have. We avoid a
		// SELECT round-trip: every column we'd return is known
		// client-side already, and uploaded_at is `now()` to within
		// transaction granularity.
		att = Attachment{
			ID:          id,
			IssueID:     issueID,
			CommentID:   commentID,
			Filename:    filename,
			ContentType: contentType,
			SizeBytes:   sizeBytes,
			Status:      StatusPending,
			UploadedBy:  uploadedBy,
			UploadedAt:  time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &att, nil
}

// Confirm flips an attachment from 'pending' to 'ready' and writes the
// attachment.added event + fan-out. Idempotent: a second call against
// an already-ready row is a no-op (returns the existing DTO).
//
// Visibility is checked inside the same SERIALIZABLE transaction as the
// status flip — this closes the TOCTOU window where membership could be
// revoked between handler-side check and the write. callerRole is the
// users.role string ("admin" bypasses project_members).
//
// Returns ErrNotFound when the row is missing or owned by a different
// uploader, or projects.ErrProjectNotVisible when the caller can no
// longer see the project.
func (s *Store) Confirm(
	ctx context.Context,
	attachmentID uuid.UUID,
	caller uuid.UUID,
	callerRole string,
	actor Actor,
) (*Attachment, error) {
	var att Attachment
	err := withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// Pull the row + the parent issue's project_id (for event row
		// project_id denorm).
		var (
			projectID      uuid.UUID
			uploadedByName *string
			currentStatus  string
			commentID      *uuid.UUID
			issueID        string
			filename       string
			contentType    string
			sizeBytes      int64
			uploadedBy     uuid.UUID
			uploadedAt     time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT a.issue_id, a.comment_id, a.filename, a.content_type, a.size_bytes,
       a.status, a.uploaded_by, a.uploaded_at, i.project_id,
       u.username
  FROM attachments a
  JOIN issues i ON i.id = a.issue_id
  LEFT JOIN users u ON u.id = a.uploaded_by
 WHERE a.id = $1
   AND a.uploaded_by = $2`,
			attachmentID, caller).Scan(
			&issueID, &commentID, &filename, &contentType, &sizeBytes,
			&currentStatus, &uploadedBy, &uploadedAt, &projectID,
			&uploadedByName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("loading attachment for confirm: %w", err)
		}

		// The write right (US-6 ActionWrite, sk-674.18 T066) inside the
		// same tx — closes the TOCTOU window between the handler-side
		// gate and the status flip below.
		if _, err := projects.EnsureProjectAction(ctx, tx, projectID, caller, authz.ActionWrite); err != nil {
			return err
		}

		att = Attachment{
			ID:                 attachmentID,
			IssueID:            issueID,
			CommentID:          commentID,
			Filename:           filename,
			ContentType:        contentType,
			SizeBytes:          sizeBytes,
			Status:             currentStatus,
			UploadedBy:         uploadedBy,
			UploadedByUsername: uploadedByName,
			UploadedAt:         uploadedAt,
		}

		if currentStatus == StatusReady {
			// Already confirmed — idempotent return. No event re-emit.
			return nil
		}

		// INV-15, K-8: the soft storage quota of the project's space —
		// confirmed while the used bytes are strictly below it, read in O(1)
		// from the counter; pending uploads are not counted, so parallel
		// uploads may go past it (E-11).
		var (
			spaceShort  string
			personal    bool
			used        int64
			participant bool
		)
		if err := tx.QueryRow(ctx, `
SELECT s.short_id, s.kind = 'personal',
       COALESCE((SELECT u.bytes FROM space_storage_usage u WHERE u.space_id = s.id), 0),
       EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = s.id AND sm.user_id = $2)
  FROM projects p JOIN spaces s ON s.id = p.space_id
 WHERE p.id = $1`, projectID, caller).Scan(&spaceShort, &personal, &used, &participant); err != nil {
			return fmt.Errorf("reading the space storage: %w", err)
		}
		quota := s.quotaSpaceBytes
		if personal {
			quota = s.quotaPersonalBytes
		}
		if quota != nil && used >= *quota {
			return &ErrStorageQuota{SpaceID: spaceShort, StorageUsedBytes: used, StorageQuotaBytes: *quota,
				Disclosable: participant}
		}

		// Flip status. Use WHERE status='pending' to keep the update
		// atomic against a concurrent confirm attempt; the second
		// caller sees zero rows affected and reflects the winner's
		// state in the returned DTO.
		tag, err := tx.Exec(ctx,
			`UPDATE attachments SET status = 'ready' WHERE id = $1 AND status = 'pending'`,
			attachmentID)
		if err != nil {
			return fmt.Errorf("flipping attachment status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Race: between SELECT and UPDATE another tx flipped it.
			// Both the WHERE status='pending' guard and the early
			// idempotent return above already filter out the
			// "already-ready" case, so the only remaining path here
			// is "another tx just made it ready". Reflect that in the
			// returned DTO so the caller doesn't see stale 'pending'.
			att.Status = StatusReady
			return nil
		}
		att.Status = StatusReady
		if err := adjustStorage(ctx, tx, projectID, sizeBytes); err != nil {
			return err
		}

		// Emit attachment.added (only on the actual transition).
		payload := map[string]any{
			"id":          attachmentID.String(),
			"issueId":     issueID,
			"filename":    filename,
			"contentType": contentType,
			"sizeBytes":   sizeBytes,
		}
		eventID, err := writeAttachmentEvent(ctx, tx, EventAttachmentAdded,
			attachmentID, projectID, issueID, actor, payload)
		if err != nil {
			return err
		}
		// Fan-out to the issue + project watchers. The event is recorded on
		// the ISSUE entity (see writeAttachmentEvent), so the entity type and
		// the issue id agree here rather than having to be reconciled.
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:     eventID,
			EventType:   EventAttachmentAdded,
			EntityType:  "issue",
			IssueID:     issueID,
			ProjectID:   projectID,
			ActorUserID: actor.UserID,
			ActorType:   actor.Role,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		var quota *ErrStorageQuota
		if errors.As(err, &quota) {
			spaces.ObserveQuotaRefusal(spaces.CodeStorageQuota)
		}
		return nil, err
	}
	return &att, nil
}

// Get loads an attachment by id. Returns ErrNotFound when the row is
// missing. Visibility is the caller's responsibility.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Attachment, error) {
	var (
		att      Attachment
		username *string
	)
	err := s.pool.QueryRow(ctx, `
SELECT a.id, a.issue_id, a.comment_id, a.filename, a.content_type,
       a.size_bytes, a.status, a.uploaded_by, a.uploaded_at, u.username
  FROM attachments a
  LEFT JOIN users u ON u.id = a.uploaded_by
 WHERE a.id = $1`, id).Scan(
		&att.ID, &att.IssueID, &att.CommentID, &att.Filename, &att.ContentType,
		&att.SizeBytes, &att.Status, &att.UploadedBy, &att.UploadedAt, &username)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading attachment: %w", err)
	}
	att.UploadedByUsername = username
	return &att, nil
}

// GetWithS3AndVisibility loads the attachment + bucket/key + checks
// project visibility against the caller — all in a single read-only tx
// so a concurrent membership revocation can't slip in between the load
// and the visibility check. Used by the download-url path. Returns
// ErrNotFound when the row is missing, projects.ErrProjectNotVisible
// when the caller can't see the project.
func (s *Store) GetWithS3AndVisibility(
	ctx context.Context,
	id uuid.UUID,
	caller uuid.UUID,
	callerRole string,
) (*Attachment, string, string, error) {
	var (
		att      Attachment
		username *string
		bucket   string
		s3Key    string
	)
	err := pgx.BeginTxFunc(ctx, s.pool,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var projectID uuid.UUID
			err := tx.QueryRow(ctx, `
SELECT a.id, a.issue_id, a.comment_id, a.filename, a.content_type,
       a.size_bytes, a.status, a.uploaded_by, a.uploaded_at, u.username,
       a.s3_bucket, a.s3_key, i.project_id
  FROM attachments a
  JOIN issues i ON i.id = a.issue_id
  LEFT JOIN users u ON u.id = a.uploaded_by
 WHERE a.id = $1`, id).Scan(
				&att.ID, &att.IssueID, &att.CommentID, &att.Filename, &att.ContentType,
				&att.SizeBytes, &att.Status, &att.UploadedBy, &att.UploadedAt, &username,
				&bucket, &s3Key, &projectID)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("loading attachment: %w", err)
			}
			att.UploadedByUsername = username
			if _, err := projects.EnsureProjectVisible(ctx, tx, projectID, caller); err != nil {
				return err
			}
			return nil
		})
	if err != nil {
		return nil, "", "", err
	}
	return &att, bucket, s3Key, nil
}

// GetWithS3 is Get + the bucket/key (used by download/delete to mint
// presigned URLs and remove objects).
func (s *Store) GetWithS3(ctx context.Context, id uuid.UUID) (*Attachment, string, string, error) {
	var (
		att      Attachment
		username *string
		bucket   string
		s3Key    string
	)
	err := s.pool.QueryRow(ctx, `
SELECT a.id, a.issue_id, a.comment_id, a.filename, a.content_type,
       a.size_bytes, a.status, a.uploaded_by, a.uploaded_at, u.username,
       a.s3_bucket, a.s3_key
  FROM attachments a
  LEFT JOIN users u ON u.id = a.uploaded_by
 WHERE a.id = $1`, id).Scan(
		&att.ID, &att.IssueID, &att.CommentID, &att.Filename, &att.ContentType,
		&att.SizeBytes, &att.Status, &att.UploadedBy, &att.UploadedAt, &username,
		&bucket, &s3Key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "", ErrNotFound
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("loading attachment: %w", err)
	}
	att.UploadedByUsername = username
	return &att, bucket, s3Key, nil
}

// ListByIssue returns every 'ready' attachment for one issue, plus the
// total count. We deliberately exclude pending rows from the wire
// surface — they are an internal lifecycle state, not user-facing.
//
// Includes attachments scoped to comments under the issue too: the UI
// renders both grouped by comment in a future slice; R4.2 ships
// issue-level only but the data path is already shape-correct.
func (s *Store) ListByIssue(ctx context.Context, issueID string) (*ListResponse, error) {
	rows, err := s.pool.Query(ctx, `
SELECT a.id, a.issue_id, a.comment_id, a.filename, a.content_type,
       a.size_bytes, a.status, a.uploaded_by, a.uploaded_at, u.username
  FROM attachments a
  LEFT JOIN users u ON u.id = a.uploaded_by
 WHERE a.issue_id = $1 AND a.status = 'ready'
 ORDER BY a.uploaded_at ASC, a.id ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("querying attachments: %w", err)
	}
	defer rows.Close()

	out := ListResponse{Items: make([]Attachment, 0, HardPerIssueCap)}
	for rows.Next() {
		var (
			att      Attachment
			username *string
		)
		if err := rows.Scan(
			&att.ID, &att.IssueID, &att.CommentID, &att.Filename, &att.ContentType,
			&att.SizeBytes, &att.Status, &att.UploadedBy, &att.UploadedAt, &username,
		); err != nil {
			return nil, fmt.Errorf("scanning attachment row: %w", err)
		}
		att.UploadedByUsername = username
		out.Items = append(out.Items, att)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating attachments: %w", err)
	}
	out.Total = len(out.Items)
	return &out, nil
}

// Delete hard-deletes a row. Returns the bucket + key so the caller can
// best-effort remove the S3 object. Emits attachment.removed event
// (informational — NOT in the notifiable set, so no fan-out).
//
// The write right (US-6 ActionWrite) is checked inside the same
// SERIALIZABLE transaction as the row delete — this closes the TOCTOU
// window where membership could be revoked between a handler-side check
// and the actual write.
//
// Returns ErrNotFound when the row is missing, projects.ErrProjectNotVisible
// when the caller can't see the project, projects.ErrProjectActionForbidden
// when he sees it but may not write in it.
func (s *Store) Delete(
	ctx context.Context,
	id uuid.UUID,
	caller uuid.UUID,
	callerRole string,
	actor Actor,
) (string, string, error) {
	var (
		bucket    string
		s3Key     string
		issueID   string
		projectID uuid.UUID
		filename  string
		status    string
		size      int64
	)
	err := withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
SELECT a.s3_bucket, a.s3_key, a.issue_id, i.project_id, a.filename, a.status, a.size_bytes
  FROM attachments a
  JOIN issues i ON i.id = a.issue_id
 WHERE a.id = $1`, id).Scan(&bucket, &s3Key, &issueID, &projectID, &filename, &status, &size)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("loading attachment for delete: %w", err)
		}
		// The write right (US-6 ActionWrite, sk-674.18 T066) inside the
		// same tx — closes the TOCTOU window between a handler-side
		// gate and the row delete below.
		if _, err := projects.EnsureProjectAction(ctx, tx, projectID, caller, authz.ActionWrite); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM attachments WHERE id = $1`, id); err != nil {
			return fmt.Errorf("deleting attachment row: %w", err)
		}
		// K-8: a confirmed attachment leaves the space's storage counter.
		if status == StatusReady {
			if err := adjustStorage(ctx, tx, projectID, -size); err != nil {
				return err
			}
		}
		// Audit-only event. NO fan-out (attachment.removed is not in
		// the notifiable set per R4.2 plan G-8).
		_, err = writeAttachmentEvent(ctx, tx, EventAttachmentRemoved,
			id, projectID, issueID, actor, map[string]any{
				"id":       id.String(),
				"issueId":  issueID,
				"filename": filename,
			})
		return err
	})
	if err != nil {
		return "", "", err
	}
	return bucket, s3Key, nil
}

// CountByIssue returns the count of READY attachments for one issue.
// Used by the per-issue soft cap surface (R4.2 plan G-6) so the UI can
// show "you're at 47/50 — slow down". REST layer can call this without
// a write transaction.
func (s *Store) CountByIssue(ctx context.Context, issueID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM attachments WHERE issue_id = $1 AND status = 'ready'`,
		issueID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting attachments: %w", err)
	}
	return n, nil
}

// ListStalePending returns up to `limit` attachment ids in 'pending'
// state older than `olderThan`. Used by the cleanup goroutine.
//
// Returns rows oldest-first (ORDER BY uploaded_at ASC) so a FIFO backlog
// is chewed from the front without re-scanning the same prefix every tick.
func (s *Store) ListStalePending(ctx context.Context, olderThan time.Duration, limit int) ([]Attachment, []string, []string, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	rows, err := s.pool.Query(ctx, `
SELECT id, issue_id, filename, content_type, size_bytes, status,
       uploaded_by, uploaded_at, s3_bucket, s3_key
  FROM attachments
 WHERE status = 'pending' AND uploaded_at < $1
 ORDER BY uploaded_at ASC
 LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing stale pending: %w", err)
	}
	defer rows.Close()

	atts := make([]Attachment, 0, limit)
	buckets := make([]string, 0, limit)
	keys := make([]string, 0, limit)
	for rows.Next() {
		var (
			a      Attachment
			bucket string
			key    string
		)
		if err := rows.Scan(&a.ID, &a.IssueID, &a.Filename, &a.ContentType,
			&a.SizeBytes, &a.Status, &a.UploadedBy, &a.UploadedAt,
			&bucket, &key); err != nil {
			return nil, nil, nil, fmt.Errorf("scanning stale row: %w", err)
		}
		atts = append(atts, a)
		buckets = append(buckets, bucket)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("iterating stale pending: %w", err)
	}
	return atts, buckets, keys, nil
}

// DeleteByID hard-deletes a row without writing an event. Used by the
// cleanup goroutine for stale-pending sweep — those rows never reached
// the user-visible state so a removal event would just be noise in the
// audit log.
//
// The stale-pending sweep never removes a confirmed row, but should a ready
// row ever come through here it leaves the K-8 storage counter with it, in
// the same transaction.
func (s *Store) DeleteByID(ctx context.Context, id uuid.UUID) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var (
			status    string
			size      int64
			projectID uuid.UUID
		)
		err := tx.QueryRow(ctx, `
DELETE FROM attachments a USING issues i
 WHERE a.id = $1 AND i.id = a.issue_id
RETURNING a.status, a.size_bytes, i.project_id`, id).Scan(&status, &size, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("deleting attachment row: %w", err)
		}
		if status == StatusReady {
			return adjustStorage(ctx, tx, projectID, -size)
		}
		return nil
	})
}

// ExistsByID returns true iff an attachment row exists with the given
// id. Used by the orphan-S3 reconciler.
func (s *Store) ExistsByID(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE id = $1)`, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking attachment existence: %w", err)
	}
	return exists, nil
}

// ExistingIDs returns the subset of ids that have an attachments row, as
// a set for O(1) membership tests. Used by the orphan-S3 reconciler to
// classify a whole batch of bucket keys in one round-trip instead of one
// SELECT per key.
//
// An empty input issues no query and returns an empty set. Ids with no
// row are simply absent from the result — a missing id is data, not an
// error.
func (s *Store) ExistingIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]struct{}, error) {
	out := make(map[uuid.UUID]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM attachments WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("checking attachment existence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning attachment id: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating attachment ids: %w", err)
	}
	return out, nil
}

// ProjectIDForIssue resolves an issue's project. Used by the REST
// handler before calling the store to perform visibility checks.
func ProjectIDForIssue(ctx context.Context, pool *pgxpool.Pool, issueID string) (uuid.UUID, error) {
	var pid uuid.UUID
	err := pool.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrIssueNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolving issue project: %w", err)
	}
	return pid, nil
}

// Actor mirrors issues.Actor and notifications.Actor to keep the
// FanOut signatures aligned without an import cycle.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string // 'human' | 'agent' | 'system'
	SessionID string
}

// writeAttachmentEvent records an attachment lifecycle event ON THE ISSUE
// (entity_type='issue', entity_id=<issue id>), with the attachment id carried
// in the payload.
//
// It used to be recorded on the attachment entity, and that is the concrete
// defect INV-13 names: "the feed and the watch counter understand only
// issues and comments: an enabled 'Attachment' reason would give an empty
// card and an uncountable badge". A notification about an attachment is a notification
// about the issue it hangs on — the reading surfaces resolve it, the card
// shows the task and leads to it, and the badge counts it, without a single
// reader learning a third entity type.
//
// Mirrors issues.WriteIssueEvent shape — duplicating ~50 lines beats
// importing the issues package and dragging in its dependencies.
func writeAttachmentEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	attachmentID uuid.UUID,
	projectID uuid.UUID,
	issueID string,
	actor Actor,
	payload map[string]any,
) (int64, error) {
	var (
		actorUserID any
		sessionID   any
		actorType   = actor.Role
	)
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}
	if actor.SessionID != "" {
		sessionID = actor.SessionID
	}
	if actorType == "" {
		actorType = "human"
	}

	payloadBytes := []byte("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("marshalling attachment event payload: %w", err)
		}
		payloadBytes = b
	}

	const sql = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    'issue', $6, $7, NULL, NULL, NULL, NULL, $8::jsonb
)
RETURNING id`
	var eventID int64
	err := tx.QueryRow(ctx, sql,
		eventType, actor.Username, actorUserID, sessionID, actorType,
		issueID, projectID, payloadBytes,
	).Scan(&eventID)
	if err != nil {
		return 0, fmt.Errorf("inserting attachment event: %w", err)
	}
	return eventID, nil
}

package mail

// cleanup.go — the retention sweep of outgoing_emails (sk-674.18, T022, K-9).
// Model — outbox/cleanup.go#CleanupRunner.
//
// A queued email holds a personal address. Once it has its outcome the row
// is kept only as long as an incident review may need it:
//
//	state = 'final' AND finalized_at < now − Retention (30 days)
//	                    — the outcome is a month old;
//	state = 'final' AND the subject user is deleted
//	                    — the account is gone, so is its mail history
//	                      (K-9 "and on account deletion");
//	state = 'final' AND the recipient is the reserved email of a deleted
//	                    account (reserved_identities, sha256(lower(email)),
//	                    K-21) — an invitation email names an invitation, not
//	                    a user, so the step above never reaches it (review
//	                    api/6 #14). The deletion itself (T041) removes the
//	                    emails of the address; this step also takes those
//	                    that reach their outcome after it.
//
// Only final rows are deleted: a pending or leased email is still owed an
// outcome (INV-25). A token whose email is deleted keeps living its own
// short term (auth_tokens.email_id is ON DELETE SET NULL).
//
// Each pass first returns expired leases (runner.go#reclaimExpiredLeases)
// and runs the deadline sweep (runner.go#retireExpired), so an installation
// that switched mail off — the worker does not run then — still ends every
// queued email as expired, including one a stopped worker left leased,
// instead of keeping it without an outcome forever.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Cleanup tuning.
const (
	// Retention — K-9: final emails are deleted 30 days after their outcome.
	Retention = 30 * 24 * time.Hour
	// defaultCleanupInterval — housekeeping, once an hour.
	defaultCleanupInterval = time.Hour
	// defaultCleanupBatchSize bounds one DELETE, so the sweep never holds a
	// long lock on the queue the worker uses.
	defaultCleanupBatchSize = 500
)

// CleanupOptions configures the sweep. Zero values take the defaults.
type CleanupOptions struct {
	Interval  time.Duration
	BatchSize int
	Retention time.Duration
	// Now is the clock (runner.go, «Time»); nil is time.Now.
	Now func() time.Time
}

// CleanupRunner is the retention sweep.
type CleanupRunner struct {
	pool *pgxpool.Pool
	opts CleanupOptions
}

// NewCleanupRunner builds the sweep.
func NewCleanupRunner(pool *pgxpool.Pool, opts CleanupOptions) *CleanupRunner {
	if opts.Interval <= 0 {
		opts.Interval = defaultCleanupInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultCleanupBatchSize
	}
	if opts.Retention <= 0 {
		opts.Retention = Retention
	}
	return &CleanupRunner{pool: pool, opts: opts}
}

// Run sweeps once at start and then every Interval until ctx is cancelled.
// A failed pass is logged and counted; the next one comes on schedule.
func (c *CleanupRunner) Run(ctx context.Context) {
	for {
		if _, err := c.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			runnerErrorsTotal.Inc()
			slog.ErrorContext(ctx, "mail cleanup pass failed", "error", err.Error())
		}
		if !sleepCtx(ctx, c.opts.Interval) {
			return
		}
	}
}

// RunOnce is one pass: the deadline sweep, then the deletion of eligible
// final emails in batches. It returns how many emails it deleted.
func (c *CleanupRunner) RunOnce(ctx context.Context) (int64, error) {
	now := time.Now()
	if c.opts.Now != nil {
		now = c.opts.Now()
	}
	// Leftover leases first, so the deadline sweep sees them as pending.
	if err := reclaimExpiredLeases(ctx, c.pool, now); err != nil {
		return 0, err
	}
	if err := retireExpired(ctx, c.pool, now); err != nil {
		return 0, err
	}
	var total int64
	for _, step := range []struct {
		what string
		sql  string
		args []any
	}{
		{"old final emails", `
DELETE FROM outgoing_emails
 WHERE id IN (SELECT id FROM outgoing_emails
               WHERE state = 'final' AND finalized_at < $1
               LIMIT $2)`, []any{now.Add(-c.opts.Retention), c.opts.BatchSize}},
		{"final emails of deleted accounts", `
DELETE FROM outgoing_emails
 WHERE id IN (SELECT e.id
                FROM users u
                JOIN outgoing_emails e ON e.user_id = u.id
               WHERE u.deleted_at IS NOT NULL AND e.state = 'final'
               LIMIT $1)`, []any{c.opts.BatchSize}},
		// recipient is lower(btrim(...)) by its CHECK — the form K-21 hashes.
		{"final emails to reserved addresses", `
DELETE FROM outgoing_emails
 WHERE id IN (SELECT e.id
                FROM outgoing_emails e
               WHERE e.state = 'final'
                 AND EXISTS (SELECT 1 FROM reserved_identities r
                              WHERE r.kind = 'email'
                                AND r.value_hash = sha256(convert_to(e.recipient, 'UTF8')))
               LIMIT $1)`, []any{c.opts.BatchSize}},
	} {
		for {
			tag, err := c.pool.Exec(ctx, step.sql, step.args...)
			if err != nil {
				return total, fmt.Errorf("deleting %s: %w", step.what, err)
			}
			total += tag.RowsAffected()
			if tag.RowsAffected() < int64(c.opts.BatchSize) {
				break
			}
		}
	}
	if total > 0 {
		slog.InfoContext(ctx, "mail cleanup deleted final emails", "count", total)
	}
	return total, nil
}

package outbox

// cleanup.go — sk-303.13 / T013: the retention sweep for invisible push-only
// notification rows.
//
// The wave exists because the storage accumulated rows nobody would ever see,
// and the fix — filtering at write time — creates a smaller version of the same
// problem at the other end: a push-only row is invisible BY DESIGN, so once its
// delivery is over it is exactly what the feature set out to remove.
//
// The spec therefore names a number rather than a sentiment: "an invisible
// push-only row after its final outcome lives no longer than 30 days… a
// 'bounded lifetime' without a value cannot be told from storing forever,
// and then the original defect 'we pile up what nobody will see' is
// reproduced by the new contract".
//
// What is eligible is narrow and each condition earns its place:
//
//	feed_channel = false     — a feed row is somebody's history, not exhaust;
//	push_state   = 'final'   — anything pending, leased or awaiting a retry is
//	                           still owed and must survive;
//	push_finalized_at < cut  — the retention window, counted from the outcome
//	                           rather than from creation, so a row that took a
//	                           week of retries still gets its full window;
//	ANY final outcome        — including configuration_no_send and
//	                           prefs_unavailable: they are the outcomes most
//	                           likely to arrive in bulk during an incident, and
//	                           exempting them would rebuild the pile.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Cleanup tuning.
const (
	// PushRetention is the lifetime of an invisible push-only row after its
	// delivery reached a final outcome. Thirty days is the spec's number, and
	// changing it is a decision about how long an undelivered signal may still
	// be useful in an incident review — not an implementation detail.
	PushRetention = 30 * 24 * time.Hour
	// defaultCleanupInterval — the sweep is housekeeping, not a service. Once
	// an hour keeps the table honest without competing for connections.
	defaultCleanupInterval = time.Hour
	// defaultCleanupBatchSize bounds one DELETE. Bounded batches are what keep
	// the sweep from taking a long lock on a table the write path is using.
	defaultCleanupBatchSize = 500
)

// CleanupOptions configures the sweep. Zero values take the defaults above.
type CleanupOptions struct {
	Interval  time.Duration
	BatchSize int
	Retention time.Duration
}

// CleanupRunner deletes invisible push-only rows whose delivery is over.
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
		opts.Retention = PushRetention
	}
	return &CleanupRunner{pool: pool, opts: opts}
}

// Run blocks until ctx is cancelled, sweeping on its own schedule.
//
// It sweeps once at start-up rather than waiting out the first interval: after
// a deployment that was down for a while, the backlog is exactly what wants
// clearing, and an hour of waiting serves nobody.
func (r *CleanupRunner) Run(ctx context.Context) error {
	slog.InfoContext(ctx, "push cleanup runner started",
		"intervalMs", r.opts.Interval.Milliseconds(),
		"batchSize", r.opts.BatchSize,
		"retentionHours", int64(r.opts.Retention/time.Hour),
	)
	defer slog.InfoContext(ctx, "push cleanup runner stopped")

	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := r.Sweep(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Housekeeping failing is not worth backing off exponentially:
			// the next interval is soon enough, and the rows are not going
			// anywhere.
			slog.ErrorContext(ctx, "push cleanup sweep failed", "error", err.Error())
		}
		if !sleepCtx(ctx, r.opts.Interval) {
			return nil
		}
	}
}

// deleteEligibleSQL removes ONE bounded batch.
//
// The sub-select with a LIMIT is what bounds it: a plain DELETE with the same
// predicate would take as many row locks as the predicate matches, which after
// an outage could be the whole backlog at once. Ordering by the finalisation
// stamp drains the oldest first, so a sweep interrupted half-way still made the
// progress that matters.
const deleteEligibleSQL = `
DELETE FROM notifications
 WHERE id IN (
     SELECT id
       FROM notifications
      WHERE NOT feed_channel
        AND push_state = 'final'
        AND push_outcome IS NOT NULL
        AND push_finalized_at IS NOT NULL
        AND push_finalized_at < $1
      ORDER BY push_finalized_at
      LIMIT $2
 )`

// Sweep deletes eligible rows in bounded batches until none are left or the
// context ends. Returns how many rows it removed.
//
// Exported so tests drive it deterministically against a controlled clock
// instead of racing the Run loop.
func (r *CleanupRunner) Sweep(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-r.opts.Retention)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, nil
		}
		tag, err := r.pool.Exec(ctx, deleteEligibleSQL, cutoff, r.opts.BatchSize)
		if err != nil {
			return total, fmt.Errorf("deleting expired push-only notifications: %w", err)
		}
		n := tag.RowsAffected()
		if n == 0 {
			break
		}
		pushCleanupDeletedTotal.Add(float64(n))
		total += n
		if n < int64(r.opts.BatchSize) {
			break
		}
	}
	if total > 0 {
		slog.InfoContext(ctx, "push cleanup removed expired invisible rows",
			"count", total,
			"olderThan", cutoff.Format(time.RFC3339),
		)
	}
	return total, nil
}

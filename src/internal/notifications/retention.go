package notifications

// retention.go — sk-622.21, T019 (US-8, INV-8, K5, K12): the retention sweep
// for notification rows.
//
// Notifications are kept NOTIFICATIONS_RETENTION_DAYS days (90 by default);
// older ones are deleted by this background sweep in bounded batches, at
// least once an hour. What is eligible, and why each condition is there:
//
//	type NOT IN (mentions)     — a mention is never deleted, whatever its age
//	                             (US-8, INV-8): the Mentions badge and feed
//	                             have no window. The literal is the closed set
//	                             of fanout.go#mentionEventTypes, spelled out
//	                             because it must equal the predicate of the
//	                             partial index idx_notif_retention (000059,
//	                             K5) for the planner to use that index;
//	created_at < cutoff        — the retention window, counted from creation;
//	push_state NOT IN          — a row whose push is still owed (queued or
//	  ('pending','leased')       leased by a runner) survives until the
//	                             delivery is over (US-8, K12). Reminders and
//	                             digests never read history rows (K12), so
//	                             deleting them changes neither.
//
// The existing push-only sweep (outbox/cleanup.go#CleanupRunner, 30 days
// after the final outcome) is untouched: it is a stricter policy over a
// subset, and both may delete the same old push-only row — whichever comes
// first.
//
// Replicas. Every replica runs this sweep at the same time, with no advisory
// lock: each batch picks its rows with FOR UPDATE SKIP LOCKED, so two sweeps
// never wait on each other (nor on a writer holding a row) and never delete
// one row twice — a row locked by somebody else is simply left for the next
// batch or the next run.
//
// The life cycle (Run / Sweep, stopping on the context, going on after a failed
// run) is the one of outbox/cleanup.go#CleanupRunner; the scope, SKIP LOCKED,
// the metrics and the cadence counted from start to start (runEvery) are this
// file's own.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention sweep tuning.
const (
	// RetentionInterval is the product cadence of the sweep: US-8 says
	// «not less often than once an hour».
	RetentionInterval = time.Hour
	// defaultRetentionBatchSize bounds one DELETE, so the sweep never takes a
	// long lock on a table the write path is using.
	defaultRetentionBatchSize = 500
)

// RetentionOptions configures the sweep.
type RetentionOptions struct {
	// Retention is how long a notification is kept. Required: the value is
	// NOTIFICATIONS_RETENTION_DAYS from the loaded configuration, and a
	// non-positive one is refused by NewRetentionRunner rather than replaced
	// by a default — a sweep with a made-up window would delete history.
	Retention time.Duration
	// Interval between runs; zero means RetentionInterval.
	Interval time.Duration
	// BatchSize bounds one DELETE; zero means the default.
	BatchSize int
}

// RetentionRunner deletes non-mention notifications older than the retention
// window.
type RetentionRunner struct {
	pool *pgxpool.Pool
	opts RetentionOptions
}

// NewRetentionRunner builds the sweep. It fails on a non-positive Retention.
func NewRetentionRunner(pool *pgxpool.Pool, opts RetentionOptions) (*RetentionRunner, error) {
	if opts.Retention <= 0 {
		return nil, fmt.Errorf("notification retention must be > 0; got %s", opts.Retention)
	}
	if opts.Interval <= 0 {
		opts.Interval = RetentionInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultRetentionBatchSize
	}
	return &RetentionRunner{pool: pool, opts: opts}, nil
}

// Run blocks until ctx is cancelled, sweeping once at start-up and then once
// per interval, counted from the START of one run to the start of the next
// (runEvery) — so «at least once an hour» (US-8) holds whatever a run costs.
// A failed run is logged and the next one comes on schedule: the rows are not
// going anywhere.
func (r *RetentionRunner) Run(ctx context.Context) {
	slog.InfoContext(ctx, "notification retention runner started",
		"intervalMs", r.opts.Interval.Milliseconds(),
		"batchSize", r.opts.BatchSize,
		"retentionHours", int64(r.opts.Retention/time.Hour),
	)
	defer slog.InfoContext(ctx, "notification retention runner stopped")

	runEvery(ctx, r.opts.Interval, func(ctx context.Context) {
		if _, err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "notification retention sweep failed",
				"retentionHours", int64(r.opts.Retention/time.Hour),
				"error", err.Error())
		}
	})
}

// runEvery calls run at once and then on a fixed cadence: the n-th call is
// due interval×n after the first one STARTED, not interval after the previous
// one ENDED (found in review) — otherwise every run's own
// duration would be added to the period. Calls never overlap: a call that
// overruns its slot is followed by the next one at once, without a further
// wait and without a burst of catch-up calls; the cadence then restarts
// from that call. It returns when ctx ends, including while run is going.
func runEvery(ctx context.Context, interval time.Duration, run func(context.Context)) {
	next := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		run(ctx)
		if ctx.Err() != nil {
			return
		}
		next = next.Add(interval)
		wait := time.Until(next)
		if wait < 0 {
			// The run overran its slot: start the next one now and count
			// the cadence from it.
			wait = 0
			next = time.Now()
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// deleteExpiredSQL removes ONE bounded batch. $1 is the cutoff, $2 the batch
// size. The sub-select bounds the batch and skips rows somebody else holds;
// the mention literal is the predicate of idx_notif_retention (see the file
// header).
const deleteExpiredSQL = `
DELETE FROM notifications
 WHERE id IN (
     SELECT id
       FROM notifications
      WHERE type NOT IN ('comment.mention', 'issue.mention')
        AND created_at < $1
        AND push_state NOT IN ('pending', 'leased')
      ORDER BY created_at
      LIMIT $2
        FOR UPDATE SKIP LOCKED
 )`

// Sweep deletes eligible rows in bounded batches until a batch comes back
// short (nothing more is free to take) or the context ends. It returns how
// many rows it removed. The cutoff is taken from the process clock once per
// sweep.
//
// Exported so tests drive one run deterministically instead of racing Run.
func (r *RetentionRunner) Sweep(ctx context.Context) (int64, error) {
	start := time.Now()
	defer func() {
		notificationRetentionSweepDurationSeconds.Observe(time.Since(start).Seconds())
	}()

	cutoff := start.UTC().Add(-r.opts.Retention)
	var total int64
	for {
		if ctx.Err() != nil {
			return total, nil
		}
		tag, err := r.pool.Exec(ctx, deleteExpiredSQL, cutoff, r.opts.BatchSize)
		if err != nil {
			return total, fmt.Errorf("deleting notifications older than %s: %w", cutoff.Format(time.RFC3339), err)
		}
		n := tag.RowsAffected()
		if n > 0 {
			notificationRetentionDeletedTotal.Add(float64(n))
			total += n
		}
		if n < int64(r.opts.BatchSize) {
			break
		}
	}
	if total > 0 {
		slog.InfoContext(ctx, "notification retention removed expired rows",
			"count", total,
			"olderThan", cutoff.Format(time.RFC3339),
		)
	}
	return total, nil
}

// sleepCtx waits d or until ctx ends; it reports whether the wait ran out
// (true) rather than being cancelled (false). A timer, not time.After, so a
// cancelled wait releases it at once.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

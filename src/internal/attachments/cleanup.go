package attachments

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CleanupConfig bounds the cleanup goroutine behaviour. Defaults are set
// in main() from config.Config; this struct lives here so the worker
// stays independent of the config package.
type CleanupConfig struct {
	// Interval between ticks. 0 disables the worker (caller decides).
	Interval time.Duration
	// PendingMaxAge is the threshold past which a 'pending' row is
	// considered abandoned and gets hard-deleted on the next sweep.
	PendingMaxAge time.Duration
	// BatchSize bounds the number of rows / objects processed per
	// tick. Defaults to 100 when zero — enough to drain a small
	// backlog quickly without holding long transactions.
	BatchSize int
	// OrphanScanInterval is how often the reconciler walks every
	// object in the bucket. The DB-side stale sweep runs every tick;
	// the orphan reconciler is more expensive (it lists S3) so we
	// throttle it. Defaults to 1h when zero.
	OrphanScanInterval time.Duration
}

// RunCleanupWorker is the attachments cleanup goroutine. Started by
// main() once at boot; stops when ctx is cancelled. Two responsibilities:
//
//  1. Stale-pending sweep — every tick. Selects rows in 'pending'
//     older than cfg.PendingMaxAge, hard-deletes the row, and best-
//     effort removes the S3 object. Pending rows are the scar tissue
//     of failed/abandoned uploads; cleaning them up keeps the per-
//     issue count caps honest.
//
//  2. Orphan-S3 reconcile — every cfg.OrphanScanInterval. Walks every
//     object in the bucket, derives the attachment id from the key,
//     and deletes objects whose row is gone (best-effort). Covers the
//     window where the request handler's best-effort delete fails
//     because S3 was briefly unreachable.
//
// The worker is single-instance per replica. Multi-replica setups can
// safely run multiple copies because every operation is idempotent and
// last-writer-wins.
//
// All errors are logged + counted; the worker never panics out of the
// goroutine.
func RunCleanupWorker(
	ctx context.Context,
	pool *pgxpool.Pool,
	s3 S3Access,
	cfg CleanupConfig,
) {
	if cfg.Interval <= 0 {
		slog.Warn("attachments cleanup worker disabled: interval <= 0")
		return
	}
	if cfg.PendingMaxAge <= 0 {
		cfg.PendingMaxAge = 1 * time.Hour
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.OrphanScanInterval <= 0 {
		cfg.OrphanScanInterval = 1 * time.Hour
	}

	store := NewStore(pool)
	slog.Info("attachments cleanup worker started",
		"interval", cfg.Interval,
		"pendingMaxAge", cfg.PendingMaxAge,
		"orphanScanInterval", cfg.OrphanScanInterval,
		"batchSize", cfg.BatchSize)

	// Initial tick so a backlog at boot doesn't wait for the first
	// scheduled interval.
	cleanupTick(ctx, pool, store, s3, cfg)

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()

	orphanTick := time.NewTicker(cfg.OrphanScanInterval)
	defer orphanTick.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("attachments cleanup worker stopping")
			return
		case <-tick.C:
			cleanupTick(ctx, pool, store, s3, cfg)
		case <-orphanTick.C:
			reconcileOrphans(ctx, store, s3, cfg.BatchSize)
		}
	}
}

// cleanupTick: one stale-pending sweep. Refreshes the pending-rows
// gauge and processes one batch.
func cleanupTick(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *Store,
	s3 S3Access,
	cfg CleanupConfig,
) {
	// Refresh the gauge regardless of whether the sweep finds work. A
	// failure here is soft: it doesn't block the sweep, but a silent
	// drop would let a persistent DB error leave the gauge stale
	// forever. Log + count so dashboards can surface the regression.
	if n, err := countPending(ctx, pool); err != nil {
		slog.WarnContext(ctx, "attachments cleanup: count pending failed",
			"error", err.Error())
		cleanupRunsTotal.WithLabelValues("db", "pending").Inc()
	} else {
		pendingRowsGauge.Set(float64(n))
	}

	rows, _, keys, err := store.ListStalePending(ctx, cfg.PendingMaxAge, cfg.BatchSize)
	if err != nil {
		slog.WarnContext(ctx, "attachments cleanup: list stale failed",
			"error", err.Error())
		cleanupRunsTotal.WithLabelValues("db", "pending").Inc()
		return
	}

	for i, row := range rows {
		// Best-effort S3 delete (the row may be a phantom whose object
		// never landed). s3 == nil → S3 disabled, skip.
		if s3 != nil {
			if err := s3.RemoveObject(ctx, keys[i]); err != nil {
				slog.WarnContext(ctx, "attachments cleanup: S3 delete failed",
					"key", keys[i], "error", err.Error())
				// Do not bail — we still want to remove the row so it
				// doesn't keep showing up in the next tick.
			}
		}
		if err := store.DeleteByID(ctx, row.ID); err != nil {
			slog.WarnContext(ctx, "attachments cleanup: row delete failed",
				"id", row.ID.String(), "error", err.Error())
			cleanupRunsTotal.WithLabelValues("db", "pending").Inc()
			return
		}
		cleanupRowsDeletedTotal.Inc()
	}
	cleanupRunsTotal.WithLabelValues("ok", "pending").Inc()
}

// reconcileOrphans walks the WHOLE bucket and removes objects whose row
// is gone. Cheap in steady state: ListKeys is O(objects), but the bulk of
// objects always have a matching row.
//
// batchSize bounds memory and the size of one existence query, not the
// reach of the scan. The listing is lexicographic and stateless between
// passes, so a scan that stopped after the first batchSize keys would look
// at the same keys every time and never reach the rest of the bucket.
// Instead the skein-owned keys are collected into batches of batchSize and
// each full batch is classified with a single ExistingIDs query while the
// listing continues.
//
// Failure handling differs per phase, deliberately:
//
//   - a failed existence lookup abandons the pass. There is no way to tell
//     orphan from live without it, and deleting an object whose row *might*
//     exist is unrecoverable data loss — so nothing in that batch is
//     deleted, the failure is counted as a db failure, and the next
//     interval rescans (the scan is stateless, nothing is lost but one
//     interval of latency).
//   - a failed RemoveObject still only costs that key.
//   - a mid-list failure still keeps the work already enumerated: the
//     candidates collected before the error are processed, then the list
//     failure is logged and counted.
func reconcileOrphans(
	ctx context.Context,
	store *Store,
	s3 S3Access,
	batchSize int,
) {
	if s3 == nil {
		return
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	// orphanCandidate is a bucket key that carries a skein attachment id.
	type orphanCandidate struct {
		key string
		id  uuid.UUID
	}
	batch := make([]orphanCandidate, 0, batchSize)
	dbFailed := false

	// flush classifies the collected batch with one existence query and
	// removes the keys without a row. A failed lookup is reported to the
	// caller so the listing stops.
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, 0, len(batch))
		for _, c := range batch {
			ids = append(ids, c.id)
		}
		existing, err := store.ExistingIDs(ctx, ids)
		if err != nil {
			slog.WarnContext(ctx, "orphan reconcile: existence check failed",
				"keys", len(batch), "error", err.Error())
			cleanupRunsTotal.WithLabelValues("db", "orphan").Inc()
			dbFailed = true
			return err
		}
		for _, c := range batch {
			if _, ok := existing[c.id]; ok {
				continue
			}
			if err := s3.RemoveObject(ctx, c.key); err != nil {
				slog.WarnContext(ctx, "orphan reconcile: delete failed",
					"key", c.key, "error", err.Error())
				continue
			}
			cleanupOrphansDeletedTotal.Inc()
		}
		batch = batch[:0]
		return nil
	}

	listErr := s3.ListKeys(ctx, "", func(key string) error {
		id, ok := AttachmentIDFromKey(key)
		if !ok {
			// Non-skein object (manual upload, leftover from migrations).
			// Don't touch — only delete what we know we own.
			return nil
		}
		batch = append(batch, orphanCandidate{key: key, id: id})
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})
	if dbFailed {
		return
	}
	// Keys enumerated before a mid-list failure are still processed.
	if err := flush(); err != nil {
		return
	}

	if listErr != nil {
		slog.WarnContext(ctx, "orphan reconcile: list failed",
			"error", listErr.Error())
		cleanupRunsTotal.WithLabelValues("storage", "orphan").Inc()
		return
	}
	cleanupRunsTotal.WithLabelValues("ok", "orphan").Inc()
}

// countPending returns the current count of pending rows. Used solely
// to refresh the pendingRowsGauge.
func countPending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE status = 'pending'`).Scan(&n)
	return n, err
}

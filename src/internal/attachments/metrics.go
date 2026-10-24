package attachments

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the attachments cleanup goroutine. Names follow
// the `skein_<area>_<measure>` convention used elsewhere in the codebase.
//
// All metrics are package-level singletons registered with promauto in
// the default registerer. The /metrics endpoint exposes them
// automatically. Metricy annotations let go-metricy-extract publish
// them to the registry.
var (
	// cleanupRunsTotal counts cleanup ticks by outcome and phase. The
	// pending-sweep arm and the orphan-reconcile arm increment
	// independently so dashboards can pick them apart — a pending-sweep
	// failure (DB) is a different signal from an orphan-reconcile
	// failure (S3 list).
	//
	// @metric description Number of attachments cleanup-goroutine arm executions, classified by outcome and phase. Steady state on an idle system is mostly reason="ok" with the rate matching the configured cleanup / orphan-scan intervals. A non-zero rate of reason="db" or reason="storage" indicates the cleanup is failing to make progress in that phase.
	// @metric calculation Incremented at the end of each arm body in attachments.RunCleanupWorker. cleanupTick increments phase="pending"; reconcileOrphans increments phase="orphan". Reason is "ok" on success, "db" on Postgres failure, "storage" on S3 failure.
	// @label reason Classification of arm outcome. Values: "ok", "db", "storage"
	// @label phase Cleanup arm. Values: "pending", "orphan"
	cleanupRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_attachments_cleanup_runs_total",
			Help: "Number of attachments cleanup ticks, by outcome and phase",
		},
		[]string{"reason", "phase"},
	)

	// cleanupRowsDeletedTotal counts rows the goroutine hard-deleted
	// during the stale-pending sweep. Useful for capacity planning —
	// a sustained non-zero rate suggests users abandoning uploads.
	//
	// @metric description Total attachment rows hard-deleted by the cleanup goroutine because they sat in 'pending' state past SKEIN_ATTACHMENT_PENDING_MAX_AGE. One increment per row deleted (multi-row batch ticks are summed).
	// @metric calculation Incremented in attachments.RunCleanupWorker after each successful row delete during the stale-pending sweep.
	cleanupRowsDeletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_attachments_cleanup_rows_deleted_total",
			Help: "Stale 'pending' attachment rows hard-deleted by the cleanup goroutine",
		},
	)

	// cleanupOrphansDeletedTotal counts S3 objects the goroutine
	// removed because no DB row referenced them. Operationally a small
	// non-zero rate is healthy (covers the brief window where the row
	// is gone but the object still lingers).
	//
	// @metric description Total S3 objects removed by the orphan reconciler — objects whose attachment row was hard-deleted while the best-effort S3 delete in the request handler failed. One increment per object removed.
	// @metric calculation Incremented in attachments.RunCleanupWorker after each successful RemoveObject during the orphan-reconciliation sweep.
	cleanupOrphansDeletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_attachments_cleanup_orphans_deleted_total",
			Help: "Orphan S3 objects (no row) deleted by the cleanup goroutine",
		},
	)

	// pendingRowsGauge tracks the current count of attachments in
	// 'pending' state. Refreshed at the start of every cleanup tick.
	//
	// @metric description Current count of attachment rows in 'pending' state (upload-url issued but confirm not yet called). Steady state is sub-second; growth indicates clients failing to confirm uploads.
	// @metric calculation Refreshed at the start of every attachments.RunCleanupWorker tick via SELECT count(*) WHERE status = 'pending'.
	pendingRowsGauge = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "skein_attachments_pending_rows",
			Help: "Number of attachment rows currently in 'pending' state",
		},
	)
)

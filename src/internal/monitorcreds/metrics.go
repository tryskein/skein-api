package monitorcreds

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the monitor-credential cleanup goroutine.
// Names follow the `skein_<area>_<measure>` convention.
var (
	// cleanupRunsTotal counts cleanup ticks by outcome.
	//
	// @metric description Number of monitor-credential cleanup-goroutine sweeps, classified by outcome. Steady state is mostly reason="ok" at the configured interval rate.
	// @metric calculation Incremented at the end of each cleanupTick in monitorcreds.RunCleanupWorker. Reason is "ok" on success, "db" on Postgres failure.
	// @label reason Classification of sweep outcome. Values: "ok", "db"
	cleanupRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_monitor_creds_cleanup_runs_total",
			Help: "Number of monitor-credential cleanup sweeps, by outcome",
		},
		[]string{"reason"},
	)

	// cleanupRowsDeletedTotal counts revoked credential rows hard-deleted.
	//
	// @metric description Total monitor-credential rows hard-deleted by the cleanup goroutine after RevokedRetention. One increment per deleted row.
	// @metric calculation Incremented in monitorcreds.cleanupTick by the number of rows Store.Cleanup reports deleted on success.
	cleanupRowsDeletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_monitor_creds_cleanup_rows_deleted_total",
			Help: "Revoked monitor-credential rows hard-deleted by the cleanup goroutine",
		},
	)

	// RootClosedTotal counts monitors that finished because their root issue
	// closed.
	//
	// @metric description Number of agent task monitors whose root-closed state was recorded (sk-579 INV-12): the poll that found the root issue closed and everything the monitor owed delivered. One increment per monitor, on the poll that records it; later polls of an already finished monitor and polls with rootClosed=false do not count.
	// @metric calculation Incremented in handler.AgentPollHandler.Poll when notifications.Store.FinishMonitorPoll reports NewlyClosed (root_closed_at set by that very statement).
	RootClosedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_monitor_root_closed_total",
			Help: "Agent task monitors finished because their root issue closed",
		},
	)
)

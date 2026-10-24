// Package lockreap implements R5.3a — the stale-lock reaper goroutine.
//
// Once per cfg.Interval the reaper looks for issues whose locked_at is
// older than cfg.Threshold and clears their lock. Each released row gets
// an issue.unclaimed event with reason="auto-released-stale-lock" and
// flows through the same notifications.FanOut as a manual unclaim — the
// previous lock-holder gets a ping that their session was reaped.
//
// This package owns its own metrics + a small worker loop. The reaper
// reuses no domain helpers beyond pgx + slog because it must stay
// independently testable: the integration tests directly construct a
// reaper with a *pgxpool.Pool and tick it once.
package lockreap

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// staleLocksReleasedTotal counts issue rows whose lock was cleared by
// the reaper. Each successful row update increments the counter once.
//
// @metric description Total issue lock rows auto-released by the stale-lock reaper. One increment per row whose locked_by/locked_at was cleared because locked_at was older than SKEIN_STALE_LOCK_THRESHOLD. Steady state on a healthy system is zero — a sustained non-zero rate indicates agents crashing without unclaim, or the threshold being too aggressive for active workloads.
// @metric calculation Incremented in lockreap.Reaper.tick after each successful per-row event-write tx commit. Multi-row batch ticks are summed (one increment per released row).
var staleLocksReleasedTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "skein_stale_locks_released_total",
		Help: "Total issue locks auto-released by the stale-lock reaper",
	},
)

// staleLockAgeSeconds is the distribution of "how stale was each lock
// when we released it". Bucket boundaries cover the typical spread:
// agents that crashed minutes ago up to locks that survived a full day.
//
// @metric description Histogram of stale-lock ages at release time (seconds). For each row reaped, the value is now() - locked_at observed in the SELECT just before the UPDATE. Used to spot drift in the SKEIN_STALE_LOCK_THRESHOLD configuration: if the bulk of releases sit at the lowest bucket, the threshold is probably too aggressive.
// @metric calculation Observed in lockreap.Reaper.tick once per released row, using the age computed by EXTRACT(EPOCH FROM (now() - locked_at)) in the same SELECT that drives the UPDATE.
var staleLockAgeSeconds = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "skein_stale_lock_age_seconds",
		Help:    "Age of locks at the moment the reaper released them, in seconds",
		Buckets: []float64{60, 300, 900, 1800, 3600, 7200},
	},
)

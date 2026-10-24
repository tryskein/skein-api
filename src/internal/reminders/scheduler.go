package reminders

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// remindersRungTotal counts reminders that came due and had a bell
// notification materialised for their owner. One increment per reminder
// rung (per claimed row), summed across batch ticks.
//
// @metric description Total reminders rung by the reminder scheduler. One increment per reminder row that came due (remind_at <= now, un-notified for its current remind_at) and had a notification materialised for its owner. Drives operational visibility into the alarm subsystem — a flatline while reminders exist in the future is normal; the rate spikes as armed reminders cross their remind_at.
// @metric calculation Incremented in scheduler.Scheduler.tick by the count returned from Store.ClaimAndNotifyDue after each successful per-tick SERIALIZABLE tx commit.
var remindersRungTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "skein_reminders_rung_total",
		Help: "Total reminders rung (due + owner notified) by the scheduler",
	},
)

// reminderRingLagSeconds is the distribution of "how late was the ring":
// now() at ring time minus remind_at. A healthy scheduler keeps this under
// one tick interval; a sustained tail indicates the scheduler is falling
// behind (backlog larger than BatchSize per tick, or the tick interval is
// too coarse for the alarm precision users expect).
//
// @metric description Histogram of reminder ring lag in seconds (scheduler-observed now() minus remind_at, at ring time). Bounded below by 0 and by the tick interval on a healthy system; a growing tail means the scheduler is behind. Used to tune SKEIN_REMINDER_INTERVAL / batch size.
// @metric calculation Observed in scheduler.Scheduler.tick once per rung reminder is not feasible (the batch returns only a count); instead the gauge-style lag is sampled per tick from the oldest claimed remind_at. See tick().
var reminderRingLagSeconds = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "skein_reminder_ring_lag_seconds",
		Help:    "Lag between a reminder's remind_at and when the scheduler rang it, in seconds",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 900},
	},
)

// DefaultInterval is the scheduler tick period when Config.Interval is
// zero. 30s matches the sk-62.2 spec — fine-grained enough that a reminder
// rings within half a minute of coming due, coarse enough that the claim
// query is cheap.
const DefaultInterval = 30 * time.Second

// DefaultBatchSize bounds how many due reminders one tick claims+rings.
const DefaultBatchSize = 200

// MaxRowsPerTick caps the total reminders one tick processes across its
// internal drain loop, so a pathological backlog (a batch of reminders all
// armed for the same instant) drains over several ticks instead of one
// runaway sweep holding the goroutine.
const MaxRowsPerTick = 2000

// Config bounds the scheduler goroutine. Populated in main() from
// config.Config; kept here so the worker stays config-package-independent
// (same pattern as lockreap.Config).
type Config struct {
	// Interval between ticks. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// BatchSize is the per-statement claim cap. <= 0 → DefaultBatchSize.
	BatchSize int
}

// Scheduler is the reminder ring worker. Construction is cheap; call Run
// to start the loop (blocks until ctx is cancelled).
type Scheduler struct {
	store *Store
	cfg   Config
	actor Actor
}

// NewScheduler builds a Scheduler over the shared Store. The actor is the
// synthetic 'system' identity stamped on every reminder.rang event (so the
// audit trail attributes the ring to the scheduler, not a user).
func NewScheduler(store *Store, cfg Config) *Scheduler {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	return &Scheduler{
		store: store,
		cfg:   cfg,
		actor: Actor{
			UserID:   uuid.Nil,
			Username: "system",
			Role:     "system",
		},
	}
}

// Run starts the scheduler loop. The first tick fires immediately at boot
// (same pattern as lockreap.Reaper.Run / attachments cleanup) so a backlog
// of already-due reminders at startup doesn't wait a full interval. The
// loop never panics out of the goroutine — a tick error is logged and the
// next tick retries.
func (sc *Scheduler) Run(ctx context.Context) {
	slog.Info("reminder scheduler started",
		"interval", sc.cfg.Interval,
		"batchSize", sc.cfg.BatchSize)

	sc.tick(ctx)

	t := time.NewTicker(sc.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("reminder scheduler stopping")
			return
		case <-t.C:
			sc.tick(ctx)
		}
	}
}

// tick drains due reminders in batches until a batch comes back smaller
// than the cap (backlog cleared), the per-tick budget is hit, or ctx is
// done. Each batch is its own SERIALIZABLE tx inside the store, so we never
// hold one long transaction.
func (sc *Scheduler) tick(ctx context.Context) {
	rowsThisTick := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if rowsThisTick >= MaxRowsPerTick {
			slog.WarnContext(ctx, "reminder scheduler: per-tick budget reached, deferring rest to next tick",
				"rung", rowsThisTick)
			return
		}
		// Sample lag just before the claim so the histogram reflects the
		// oldest currently-due reminder. Best-effort — a query error here
		// is non-fatal (the claim below will surface real failures).
		sc.observeLag(ctx)

		n, err := sc.store.ClaimAndNotifyDue(ctx, sc.cfg.BatchSize, sc.actor)
		if err != nil {
			slog.WarnContext(ctx, "reminder scheduler: ring batch failed",
				"error", err.Error())
			return
		}
		if n == 0 {
			return
		}
		remindersRungTotal.Add(float64(n))
		rowsThisTick += n
		slog.InfoContext(ctx, "reminders rung", "count", n)
		if n < sc.cfg.BatchSize {
			return
		}
	}
}

// observeLag samples the lag of the oldest due-and-un-notified reminder and
// records it on the histogram. A separate cheap read (not part of the claim
// tx) so the metric is populated even when the batch is empty-but-armed.
func (sc *Scheduler) observeLag(ctx context.Context) {
	var lagSeconds *float64
	err := sc.store.pool.QueryRow(ctx, `
SELECT EXTRACT(EPOCH FROM (now() - MIN(remind_at)))
  FROM reminders
 WHERE remind_at <= now()
   AND (notified_at IS NULL OR notified_at < remind_at)`).Scan(&lagSeconds)
	if err != nil {
		slog.DebugContext(ctx, "reminder scheduler: lag sample failed", "error", err.Error())
		return
	}
	if lagSeconds != nil && *lagSeconds >= 0 {
		reminderRingLagSeconds.Observe(*lagSeconds)
	}
}

// Tick runs one scheduler sweep against the DB. Exposed for tests so they
// can drive the scheduler deterministically without the ticker.
func (sc *Scheduler) Tick(ctx context.Context) {
	sc.tick(ctx)
}

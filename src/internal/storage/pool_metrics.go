package storage

import (
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Connection-pool metrics (sk-671). pgx keeps the pool's counters in
// pool.Stat() but exports nothing to Prometheus on its own; without these
// gauges a request waiting for a free connection is indistinguishable from
// a slow query — the root of the "upstream_timeout" investigation (sk-609).
//
// Every metric is a *Func metric: it reads pool.Stat() at scrape time, so
// there is no ticker and no extra work between scrapes. The pool is handed
// over once by ObservePool; before that (and in processes that never call
// it) every metric reads 0. Package-level vars with literal opts keep the
// declarations visible to go-metricy-extract.

// observedPool is the pool the metrics read; nil until ObservePool.
var observedPool atomic.Pointer[pgxpool.Pool]

// ObservePool makes the pool metrics read p. Call it once, right after the
// process' pool is created (cmd/api/main.go). A later call replaces the
// observed pool; nil detaches it (metrics read 0).
func ObservePool(p *pgxpool.Pool) {
	observedPool.Store(p)
}

// poolStat returns the observed pool's snapshot, or nil when none is set.
func poolStat() *pgxpool.Stat {
	p := observedPool.Load()
	if p == nil {
		return nil
	}
	return p.Stat()
}

func statGauge(f func(*pgxpool.Stat) float64) func() float64 {
	return func() float64 {
		s := poolStat()
		if s == nil {
			return 0
		}
		return f(s)
	}
}

var (
	// poolAcquiredConns is the number of connections currently checked out.
	//
	// @metric description Connections of the skein-api Postgres pool currently acquired by callers (in use by a query or transaction), per process. Equal to skein_db_pool_max_conns means the pool is exhausted and new acquires wait.
	// @metric calculation Read from pgxpool Stat().AcquiredConns() at scrape time (GaugeFunc in internal/storage/pool_metrics.go); 0 before storage.ObservePool is called.
	poolAcquiredConns = promauto.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "skein_db_pool_acquired_conns",
			Help: "Postgres pool connections currently acquired",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }),
	)

	// poolIdleConns is the number of open connections nobody holds.
	//
	// @metric description Open connections of the skein-api Postgres pool that are idle (established, not acquired), per process.
	// @metric calculation Read from pgxpool Stat().IdleConns() at scrape time (GaugeFunc in internal/storage/pool_metrics.go); 0 before storage.ObservePool is called.
	poolIdleConns = promauto.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "skein_db_pool_idle_conns",
			Help: "Postgres pool connections currently idle",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) }),
	)

	// poolTotalConns is acquired + idle + being established.
	//
	// @metric description All connections of the skein-api Postgres pool, per process: acquired + idle + under construction. This is what the process holds against the server's max_connections.
	// @metric calculation Read from pgxpool Stat().TotalConns() at scrape time (GaugeFunc in internal/storage/pool_metrics.go); 0 before storage.ObservePool is called.
	poolTotalConns = promauto.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "skein_db_pool_total_conns",
			Help: "Postgres pool connections in total (acquired, idle, constructing)",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.TotalConns()) }),
	)

	// poolConstructingConns is the number of connections being established.
	//
	// @metric description Connections of the skein-api Postgres pool currently being established (dial + auth in progress), per process. Sustained non-zero values mean slow or failing connects.
	// @metric calculation Read from pgxpool Stat().ConstructingConns() at scrape time (GaugeFunc in internal/storage/pool_metrics.go); 0 before storage.ObservePool is called.
	poolConstructingConns = promauto.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "skein_db_pool_constructing_conns",
			Help: "Postgres pool connections currently being established",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.ConstructingConns()) }),
	)

	// poolMaxConns is the configured pool ceiling.
	//
	// @metric description Configured maximum size of the skein-api Postgres pool, per process (DATABASE_MAX_CONNS, clamped up to the pool minimum). Compare with skein_db_pool_acquired_conns to see exhaustion.
	// @metric calculation Read from pgxpool Stat().MaxConns() at scrape time (GaugeFunc in internal/storage/pool_metrics.go); 0 before storage.ObservePool is called.
	poolMaxConns = promauto.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "skein_db_pool_max_conns",
			Help: "Postgres pool maximum size",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }),
	)

	// poolAcquireTotal counts successful acquires.
	//
	// @metric description Cumulative successful connection acquires from the skein-api Postgres pool since process start, per process. Rate = connections handed out per second.
	// @metric calculation Read from pgxpool Stat().AcquireCount() at scrape time (CounterFunc in internal/storage/pool_metrics.go); resets with the process; 0 before storage.ObservePool is called.
	poolAcquireTotal = promauto.NewCounterFunc(
		prometheus.CounterOpts{
			Name: "skein_db_pool_acquire_total",
			Help: "Successful Postgres pool acquires",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.AcquireCount()) }),
	)

	// poolEmptyAcquireTotal counts acquires that had to wait.
	//
	// @metric description Cumulative successful acquires from the skein-api Postgres pool that found no idle connection and had to wait for one to be released or established, per process. Its share of skein_db_pool_acquire_total is the pool-pressure signal.
	// @metric calculation Read from pgxpool Stat().EmptyAcquireCount() at scrape time (CounterFunc in internal/storage/pool_metrics.go); resets with the process; 0 before storage.ObservePool is called.
	poolEmptyAcquireTotal = promauto.NewCounterFunc(
		prometheus.CounterOpts{
			Name: "skein_db_pool_empty_acquire_total",
			Help: "Postgres pool acquires that had to wait for a connection",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.EmptyAcquireCount()) }),
	)

	// poolCanceledAcquireTotal counts acquires abandoned by their caller.
	//
	// @metric description Cumulative connection acquires from the skein-api Postgres pool cancelled by the caller's context before a connection was obtained (request timeout or client gone while waiting), per process.
	// @metric calculation Read from pgxpool Stat().CanceledAcquireCount() at scrape time (CounterFunc in internal/storage/pool_metrics.go); resets with the process; 0 before storage.ObservePool is called.
	poolCanceledAcquireTotal = promauto.NewCounterFunc(
		prometheus.CounterOpts{
			Name: "skein_db_pool_canceled_acquire_total",
			Help: "Postgres pool acquires cancelled before a connection was obtained",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return float64(s.CanceledAcquireCount()) }),
	)

	// poolAcquireSecondsTotal is the total time spent in successful acquires.
	//
	// @metric description Cumulative time spent in successful connection acquires from the skein-api Postgres pool, in seconds, per process. rate(this)/rate(skein_db_pool_acquire_total) is the mean acquire latency.
	// @metric calculation Read from pgxpool Stat().AcquireDuration() at scrape time (CounterFunc in internal/storage/pool_metrics.go); resets with the process; 0 before storage.ObservePool is called.
	poolAcquireSecondsTotal = promauto.NewCounterFunc(
		prometheus.CounterOpts{
			Name: "skein_db_pool_acquire_seconds_total",
			Help: "Total seconds spent acquiring Postgres pool connections",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return s.AcquireDuration().Seconds() }),
	)

	// poolEmptyAcquireWaitSecondsTotal is the total time acquires waited.
	//
	// @metric description Cumulative time successful acquires from the skein-api Postgres pool spent waiting because no idle connection was available, in seconds, per process. Growth here is time requests queued for the database before running.
	// @metric calculation Read from pgxpool Stat().EmptyAcquireWaitTime() at scrape time (CounterFunc in internal/storage/pool_metrics.go); resets with the process; 0 before storage.ObservePool is called.
	poolEmptyAcquireWaitSecondsTotal = promauto.NewCounterFunc(
		prometheus.CounterOpts{
			Name: "skein_db_pool_empty_acquire_wait_seconds_total",
			Help: "Total seconds Postgres pool acquires waited for a connection",
		},
		statGauge(func(s *pgxpool.Stat) float64 { return s.EmptyAcquireWaitTime().Seconds() }),
	)
)

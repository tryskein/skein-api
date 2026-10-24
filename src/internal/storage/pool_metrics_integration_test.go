package storage_test

// Integration coverage for the connection-pool metrics (sk-671): the
// skein_db_pool_* series exported on the default registry must reflect the
// observed pool's real state — connections held, acquires that had to wait,
// acquires abandoned by their caller. The expected values come from what the
// test itself does to the pool, not from pool.Stat().

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/storage"
	"skein-api/internal/testsupport"
)

// gatherValue returns the current value of a single-series metric from the
// default registry; it fails the test when the series is absent.
func gatherValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("%s: %d series, want 1", name, len(mf.GetMetric()))
		}
		m := mf.GetMetric()[0]
		if g := m.GetGauge(); g != nil {
			return g.GetValue()
		}
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
		t.Fatalf("%s: neither gauge nor counter", name)
	}
	t.Fatalf("%s: not exported", name)
	return 0
}

func TestPoolMetrics_ReflectTheObservedPool(t *testing.T) {
	url := os.Getenv(testsupport.TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testsupport.TestDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const size int32 = 3
	pool, err := storage.NewPool(ctx, url, size)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	storage.ObservePool(pool)
	t.Cleanup(func() { storage.ObservePool(nil) })

	if got := gatherValue(t, "skein_db_pool_max_conns"); got != float64(size) {
		t.Errorf("max_conns = %v, want %d", got, size)
	}

	// Hold every connection of the pool.
	held := make([]*pgxpool.Conn, 0, size)
	for range size {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		held = append(held, c)
	}
	if got := gatherValue(t, "skein_db_pool_acquired_conns"); got != float64(size) {
		t.Errorf("acquired_conns with the pool exhausted = %v, want %d", got, size)
	}
	if got := gatherValue(t, "skein_db_pool_idle_conns"); got != 0 {
		t.Errorf("idle_conns with the pool exhausted = %v, want 0", got)
	}

	// An acquire on the exhausted pool whose caller gives up is a cancelled
	// acquire.
	canceledBefore := gatherValue(t, "skein_db_pool_canceled_acquire_total")
	shortCtx, cancelShort := context.WithTimeout(ctx, 100*time.Millisecond)
	if _, err := pool.Acquire(shortCtx); err == nil {
		t.Fatal("acquire on an exhausted pool succeeded, want a timeout")
	}
	cancelShort()
	if got := gatherValue(t, "skein_db_pool_canceled_acquire_total"); got != canceledBefore+1 {
		t.Errorf("canceled_acquire_total = %v, want %v", got, canceledBefore+1)
	}

	// An acquire that waits for a release is an empty acquire, and its wait
	// shows up in the wait-time counter.
	emptyBefore := gatherValue(t, "skein_db_pool_empty_acquire_total")
	waitBefore := gatherValue(t, "skein_db_pool_empty_acquire_wait_seconds_total")
	acquired := make(chan *pgxpool.Conn, 1)
	go func() {
		c, err := pool.Acquire(ctx)
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- c
	}()
	// Ordering only, not a time bound: the acquire has to be parked on the
	// exhausted pool before the release, or it would not be an empty acquire
	// (a too-short pause makes the test red, never falsely green).
	time.Sleep(200 * time.Millisecond)
	held[0].Release()
	c := <-acquired
	if c == nil {
		t.Fatal("waiting acquire failed")
	}
	held[0] = c
	if got := gatherValue(t, "skein_db_pool_empty_acquire_total"); got != emptyBefore+1 {
		t.Errorf("empty_acquire_total = %v, want %v", got, emptyBefore+1)
	}
	if got := gatherValue(t, "skein_db_pool_empty_acquire_wait_seconds_total"); got <= waitBefore {
		t.Errorf("empty_acquire_wait_seconds_total = %v, did not grow from %v after a waiting acquire", got, waitBefore)
	}

	for _, h := range held {
		h.Release()
	}
	if got := gatherValue(t, "skein_db_pool_acquired_conns"); got != 0 {
		t.Errorf("acquired_conns after releasing everything = %v, want 0", got)
	}
}

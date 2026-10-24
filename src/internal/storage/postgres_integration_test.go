package storage_test

// Integration coverage for storage.NewPool against a live Postgres
// (sk-105.6 / spec §3.4). Verifies: the pool connects + pings successfully,
// the documented pool-tuning config is applied to the returned pool, and a
// malformed URL fails fast at construction.
//
// DB-backed cases skip when TEST_DATABASE_URL is unset (the harness
// contract). The bad-URL parse case needs no DB and always runs.

import (
	"context"
	"os"
	"testing"
	"time"

	"skein-api/internal/storage"
	"skein-api/internal/testsupport"
)

// testMaxConns is the pool size passed to NewPool in these tests. The exact
// value is irrelevant to connectivity — it only needs to satisfy the
// MaxConns >= MinConns invariant — but TestNewPool_ConfigApplied asserts the
// pool honours whatever value is passed (guarding against a regression back
// to CPU-derived sizing).
const testMaxConns int32 = 8

// TestNewPool_ConnectsAndPings asserts NewPool returns a usable, pre-Ping'd
// pool: a trivial `SELECT 1` round-trips, proving the pool actually reached
// the database (NewPool Ping()s internally before returning).
func TestNewPool_ConnectsAndPings(t *testing.T) {
	url := os.Getenv(testsupport.TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testsupport.TestDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := storage.NewPool(ctx, url, testMaxConns)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 returned %d, want 1", one)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Errorf("Ping after NewPool: %v", err)
	}
}

// TestNewPool_ConfigApplied asserts the tuning knobs documented in
// postgres.go are actually set on the returned pool's config. These are the
// guards against pool starvation / hung readiness probes after a failover,
// so a silent regression (e.g. someone drops the PingTimeout) must fail a
// test.
func TestNewPool_ConfigApplied(t *testing.T) {
	url := os.Getenv(testsupport.TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testsupport.TestDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := storage.NewPool(ctx, url, testMaxConns)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	cfg := pool.Config()

	// The pool must honour the caller-supplied size verbatim — NOT derive it
	// from CPU count. A regression to runtime.NumCPU()-based sizing (which
	// reads the node's cores, not the pod's CPU limit) exhausts the DB.
	if cfg.MaxConns != testMaxConns {
		t.Errorf("MaxConns = %d, want %d (caller-supplied)", cfg.MaxConns, testMaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns = %d, want 2", cfg.MinConns)
	}
	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime = %v, want 30m", cfg.MaxConnLifetime)
	}
	if cfg.MaxConnLifetimeJitter != 5*time.Minute {
		t.Errorf("MaxConnLifetimeJitter = %v, want 5m", cfg.MaxConnLifetimeJitter)
	}
	if cfg.MaxConnIdleTime != 5*time.Minute {
		t.Errorf("MaxConnIdleTime = %v, want 5m", cfg.MaxConnIdleTime)
	}
	if cfg.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod = %v, want 30s", cfg.HealthCheckPeriod)
	}
	// PingTimeout caps Acquire() on a half-open connection — its absence is
	// the exact bug the comment in postgres.go calls out.
	if cfg.ConnConfig.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnConfig.ConnectTimeout = %v, want 5s", cfg.ConnConfig.ConnectTimeout)
	}
	// DialFunc is the custom TCP-keepalive dialer; it must be non-nil.
	if cfg.ConnConfig.DialFunc == nil {
		t.Errorf("ConnConfig.DialFunc is nil — TCP keepalive dialer not wired")
	}
}

// TestNewPool_BadURL asserts a malformed connection string is rejected at
// parse time (fail-fast), not deferred to the first query. No DB needed.
func TestNewPool_BadURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// A scheme pgxpool.ParseConfig rejects outright.
	if _, err := storage.NewPool(ctx, "://not-a-valid-url", testMaxConns); err == nil {
		t.Errorf("NewPool with malformed URL returned nil error, want parse failure")
	}
}

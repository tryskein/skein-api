// Package storage owns Postgres connectivity for skein-api.
//
// The package exposes a single constructor, NewPool, returning a configured
// pgxpool.Pool. Repository types added in later slices (R1.x) will accept
// *pgxpool.Pool and execute queries through it; sqlc-generated query code
// lives under storage/generated.
package storage

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool tuning — derived from the project's deployment shape (Kubernetes,
// CloudNativePG Postgres, NAT'd networking).
//
//   - keepAlive (TCP-level) catches dead peers fast: pgx itself has no
//     application-level keepalive (unlike Npgsql's Keepalive=30), so without
//     this the pool can hand out broken connections after a NAT timeout or
//     pod-level Postgres failover. 10s matches the value used by other
//     services in the org.
//   - maxConnLifetime + jitter spreads pool churn across replicas instead of
//     reconnecting all at once on the same minute boundary.
//   - pingTimeout caps Acquire() on a half-open connection. The pgx default
//     is 0 (no timeout), which can hang readiness probes indefinitely.
const (
	dialKeepAlive         = 10 * time.Second
	dialTimeout           = 5 * time.Second
	minPoolConns          = 2
	maxConnLifetime       = 30 * time.Minute
	maxConnLifetimeJitter = 5 * time.Minute
	maxConnIdleTime       = 5 * time.Minute
	healthCheckPeriod     = 30 * time.Second
	pingTimeout           = 3 * time.Second
)

// NewPool builds and verifies a Postgres connection pool from a libpq URL
// (e.g. "postgres://user:pass@host:5432/db?sslmode=disable"). The returned
// pool has been Ping()'d; the caller must defer pool.Close().
//
// maxConns caps the pool size PER PROCESS and MUST be sized against the
// Postgres server's max_connections (shared across every replica), NOT the
// pod's CPU count — see config.DatabaseMaxConns for the full rationale. A
// value below minPoolConns is clamped up to minPoolConns so the pool always
// satisfies pgx's MaxConns >= MinConns invariant.
func NewPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}

	// Custom DialFunc enables TCP keepalive on every new connection. Without
	// it, half-open connections (NAT timeout, broken Postgres pod) only
	// surface on the next query, which can take minutes.
	cfg.ConnConfig.DialFunc = (&net.Dialer{
		KeepAlive: dialKeepAlive,
		Timeout:   dialTimeout,
	}).DialContext

	if maxConns < minPoolConns {
		maxConns = minPoolConns
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minPoolConns
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnLifetimeJitter = maxConnLifetimeJitter
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.HealthCheckPeriod = healthCheckPeriod
	// PingTimeout caps the in-pool health-check ping; without it Acquire()
	// can block indefinitely on a half-open backend.
	cfg.PingTimeout = pingTimeout
	cfg.ConnConfig.ConnectTimeout = dialTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	// Ping() proves the pool can actually reach the database. Without this a
	// misconfigured DATABASE_URL would only fail later, on the first request.
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return pool, nil
}

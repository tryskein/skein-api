package monitorcreds

import (
	"context"
	"log/slog"
	"time"
)

// CleanupConfig bounds the cleanup goroutine. Populated in main() from
// config.Config; kept here so the worker stays independent of the config
// package (same pattern as attachments.CleanupConfig / lockreap.Config).
type CleanupConfig struct {
	// Interval between sweeps. Defensive guard for direct callers/tests: <= 0
	// makes RunCleanupWorker no-op. In production config.Load rejects a
	// non-positive SKEIN_MONITOR_CREDENTIAL_CLEANUP_INTERVAL, so this branch
	// is never hit via normal startup.
	Interval time.Duration
	// RevokedRetention is how long a revoked row is kept before the sweep
	// hard-deletes it. Passed straight to Store.Cleanup. >= 0.
	RevokedRetention time.Duration
}

// RunCleanupWorker runs the monitor-credential cleanup loop until ctx is
// cancelled. Mirrors the outbox dispatcher / attachments cleanup shape: one
// goroutine per replica, a tick fires immediately at boot so a backlog
// doesn't wait for the first interval, and the loop never panics out of the
// goroutine (a failed sweep is logged and the next tick retries).
//
// The sweep is idempotent and last-writer-wins, so multiple replicas can
// safely run their own copy.
func RunCleanupWorker(ctx context.Context, store *Store, cfg CleanupConfig) {
	if cfg.Interval <= 0 {
		slog.Warn("monitor credential cleanup worker disabled: interval <= 0")
		return
	}

	slog.Info("monitor credential cleanup worker started",
		"interval", cfg.Interval,
		"revokedRetention", cfg.RevokedRetention)
	defer slog.Info("monitor credential cleanup worker stopped")

	cleanupTick(ctx, store, cfg.RevokedRetention)

	t := time.NewTicker(cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cleanupTick(ctx, store, cfg.RevokedRetention)
		}
	}
}

// cleanupTick runs one sweep. Errors are logged and swallowed — a transient
// DB failure must not kill the goroutine; the next tick takes another shot.
func cleanupTick(ctx context.Context, store *Store, revokedRetention time.Duration) {
	n, err := store.Cleanup(ctx, revokedRetention)
	if err != nil {
		cleanupRunsTotal.WithLabelValues("db").Inc()
		slog.WarnContext(ctx, "monitor credential cleanup: sweep failed",
			"error", err.Error())
		return
	}
	cleanupRunsTotal.WithLabelValues("ok").Inc()
	if n > 0 {
		cleanupRowsDeletedTotal.Add(float64(n))
		slog.InfoContext(ctx, "monitor credential cleanup", "deleted", n)
	}
}

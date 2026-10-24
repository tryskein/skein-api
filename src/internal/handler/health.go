// Package handler contains HTTP handlers for skein-api.
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// readyPingTimeout caps the readiness probe's database round-trip. Kept
// short so a stuck Postgres turns the pod "not ready" within K8s probe
// budget instead of blocking the goroutine for the whole probe interval.
const readyPingTimeout = 2 * time.Second

// Health returns a liveness probe handler.
//
// The handler always responds 200 with {"status":"ok"} as long as the process
// can serve HTTP. It MUST NOT check downstream dependencies (DB, NATS, etc.) —
// that is the role of /ready. A failing liveness probe causes Kubernetes to
// restart the pod; failing it on transient DB outages causes restart storms
// (the project guidelines, "Health and Readiness").
func Health() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// S3HealthChecker is the narrow interface Ready needs for R4.2 — a
// single HealthCheck(ctx) error call. Decoupling lets the handler remain
// agnostic of the storage package and survives nil when S3 is
// disabled (S3_URL unset).
type S3HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// Ready returns a readiness probe handler bound to the supplied Postgres
// pool. It pings the database with a 2s timeout and returns 503 on failure
// so Kubernetes removes the pod from load-balancing while keeping it alive
// (so it can recover and rejoin without a full restart).
//
// s3Client is optional: when nil (no S3_URL configured), the
// readiness check skips the S3 ping. When non-nil, a failed ping flips
// the pod to "not ready" so K8s removes it from the service load balancer
// — the API process keeps running and the cleanup goroutine continues to
// reconcile state.
func Ready(pool *pgxpool.Pool, s3Client S3HealthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyPingTimeout)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			slog.WarnContext(ctx, "readiness check failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not ready",
				"error":  "database unavailable",
			})
			return
		}

		if s3Client != nil {
			if err := s3Client.HealthCheck(ctx); err != nil {
				slog.WarnContext(ctx, "readiness check failed", "error", err.Error())
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "not ready",
					"error":  "object storage unavailable",
				})
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

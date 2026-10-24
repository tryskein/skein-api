package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/config"
	"skein-api/internal/notifications"
)

// startNotificationWorkers starts the background notification workers of
// sk-622.21 R1 and returns a channel closed once every one of them has
// returned after ctx is cancelled:
//
//   - the retention sweep (T019, US-8): non-mention notifications older than
//     cfg.NotificationsRetentionDays are deleted, once at start-up and then
//     every notifications.RetentionInterval (an hour).
//   - the assignee re-resolution (T025, INV-15): issues whose
//     assignee_user_id differs from the resolution of their assignee text
//     by the current users directory are re-resolved — the one race the
//     000060 triggers cannot see; once at start-up and then every
//     notifications.AssigneeResyncInterval, one replica at a time. (R2
//     removed the rest of the R1 reconciliation, whose steps existed for
//     the old version's rows.)
//
// It is THE wiring of these workers: main calls it, and the wiring test
// (workers_wiring_test.go, decision R16) calls the same function, so a worker
// that is built but never started, or started with a value that did not come
// from the loaded configuration, is visible to a test. The cadence is the
// product's and lives inside the workers; callers pass none.
//
// The goroutines are started in the caller's goroutine context, so a test
// running under testing/synctest gets them inside its bubble.
func startNotificationWorkers(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) (<-chan struct{}, error) {
	retention, err := notifications.NewRetentionRunner(pool, notifications.RetentionOptions{
		Retention: time.Duration(cfg.NotificationsRetentionDays) * 24 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("building notification retention runner: %w", err)
	}

	resync := notifications.NewAssigneeResyncRunner(pool, notifications.AssigneeResyncOptions{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		retention.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		resync.Run(ctx)
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done, nil
}

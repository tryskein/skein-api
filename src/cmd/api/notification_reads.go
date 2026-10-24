package main

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/config"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
)

// newNotificationReads composes the readers of the caller's notification
// surfaces from the loaded configuration (sk-622.21, decision R16): the
// shared notifications store and the /me handler, both reading the Watching
// badge and column with the window cfg.WatchingFeedWindowDays (US-5).
//
// It is THE wiring of that window: main calls it, and the wiring test
// (watching_window_wiring_test.go) calls the same function, so a reader built
// with a literal window, or one that never received the configured value,
// is visible to a test through the production router.
func newNotificationReads(pool *pgxpool.Pool, cfg *config.Config, issuesStore *issues.Store) (*notifications.Store, *handler.MeHandler) {
	notifStore := notifications.NewStore(pool,
		notifications.WithWatchingWindow(time.Duration(cfg.WatchingFeedWindowDays)*24*time.Hour))
	return notifStore, handler.NewMeHandler(pool, issuesStore, notifStore)
}

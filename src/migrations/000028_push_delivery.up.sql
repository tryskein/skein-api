-- 000028_push_delivery — wire Web Push into the notification fan-out.
--
-- The push foundation (000027) stored browser subscriptions. This migration
-- adds the two columns that turn a bell notification into a web-push delivery:
--
--   notifications.push_category — set by the PRODUCER (PatchIssue / CreateIssue
--       / AcceptIssue / reminder scheduler) on the SPECIFIC recipient row that
--       should also be pushed. NULL = bell-only (the default for every existing
--       and most new rows). We cannot gate push by the raw event `type`:
--       "assignment" arrives as issue.updated, which also carries priority /
--       title / status changes destined for every watcher. Stamping a category
--       on exactly the new-assignee's notification row is the only way to push
--       to the right person for the right reason. Known values live in the Go
--       category registry (internal/push/category.go): assigned | reminder |
--       accepted.
--
--   user_notification_prefs.push_disabled_categories — per-user opt-out list.
--       The dispatcher skips a push when its category is present here. Empty
--       array (the default) = every category is pushed. This is push-ONLY: it
--       does not affect the bell/NATS notification, which the user still sees
--       in-app regardless.
--
-- Both columns are additive and nullable / defaulted, so existing rows need no
-- backfill and the bell/NATS path is untouched.

ALTER TABLE notifications ADD COLUMN push_category text;

ALTER TABLE user_notification_prefs
    ADD COLUMN push_disabled_categories text[] NOT NULL DEFAULT '{}';

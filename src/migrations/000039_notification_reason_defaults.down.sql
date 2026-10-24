-- Rollback of 000039_notification_reason_defaults.
--
-- Drops exactly what the up migration added: reason × channel prefs, the
-- feed-channel flag, the push lifecycle, and the counters journal.
-- Notification rows are not touched — only migration 3 deletes those, and
-- its rollback is not replaced by a backup.

DROP INDEX IF EXISTS idx_notif_push_final_invisible;
DROP INDEX IF EXISTS idx_notif_push_leased;
DROP INDEX IF EXISTS idx_notif_push_pending;
DROP INDEX IF EXISTS idx_notif_feed_user;
DROP INDEX IF EXISTS idx_notif_feed_unread;

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_push_attempts_chk,
    DROP CONSTRAINT IF EXISTS notifications_push_final_outcome_chk,
    DROP CONSTRAINT IF EXISTS notifications_push_outcome_chk,
    DROP CONSTRAINT IF EXISTS notifications_push_state_chk;

ALTER TABLE notifications
    DROP COLUMN IF EXISTS push_finalized_at,
    DROP COLUMN IF EXISTS push_deadline_at,
    DROP COLUMN IF EXISTS push_next_attempt_at,
    DROP COLUMN IF EXISTS push_leased_until,
    DROP COLUMN IF EXISTS push_attempts,
    DROP COLUMN IF EXISTS push_outcome,
    DROP COLUMN IF EXISTS push_state,
    DROP COLUMN IF EXISTS feed_channel,
    DROP COLUMN IF EXISTS reason;

DROP TABLE IF EXISTS user_notification_reason_prefs;

DROP INDEX IF EXISTS idx_migration_counters_migration;
DROP TABLE IF EXISTS migration_counters;

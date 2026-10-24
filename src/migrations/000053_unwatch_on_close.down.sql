-- Rollback of 000053_unwatch_on_close.
--
-- ⚠ DATA LOSS: dropping the column erases the choice of people who turned the
-- setting on. Subscriptions already removed on close are not restored by the rollback — they were
-- removed by the application layer, and they can be restored only by reopening the issue
-- or by a manual subscription.
ALTER TABLE user_notification_prefs
    DROP COLUMN IF EXISTS unwatch_on_close;

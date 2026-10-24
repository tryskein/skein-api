-- Reverse of 000008_watchers_notifications.up.sql. Order matters: drop
-- notifications first because it references events (out of this slice
-- but still through the FK).
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS user_notification_prefs;
DROP TABLE IF EXISTS custom_watchers;
DROP TABLE IF EXISTS project_watchers;
DROP TABLE IF EXISTS issue_watchers;

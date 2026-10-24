-- 000013_watchers_extended — R3.3 rollback
--
-- Reverses the three additions in the up migration. Rollback is safe:
-- dropping notifications.published_at also drops the partial index that
-- depends on it; project_watchers.event_types drops cleanly because no
-- foreign keys point to it.

DROP INDEX IF EXISTS notifications_unpublished;
ALTER TABLE notifications DROP COLUMN IF EXISTS published_at;
ALTER TABLE project_watchers DROP COLUMN IF EXISTS event_types;

-- Reverse 000012_events_outbox: drop the partial index first (depends on
-- the column), then the column itself.

DROP INDEX IF EXISTS events_unpublished;
ALTER TABLE events DROP COLUMN IF EXISTS published_at;

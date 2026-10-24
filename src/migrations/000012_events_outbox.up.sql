-- 000012_events_outbox — R3.2a outbox foundation
--
-- Adds a published_at marker to the events table so a single dispatcher
-- goroutine can use `events` as the transactional outbox for the live-feed
-- NATS topology (R3.2). Existing rows pre-date any subscriber, so backfill
-- them as already-published BEFORE the partial index is created — otherwise
-- the dispatcher's first tick would replay R0–R3.1 audit history into NATS.

ALTER TABLE events ADD COLUMN published_at timestamptz NULL;

-- Backfill historical rows as published. Order matters: this MUST run before
-- the partial index is created so the index does not start huge and shrink.
UPDATE events SET published_at = now() WHERE published_at IS NULL;

-- Partial index over only unpublished rows. The dispatcher polls
-- `WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 100`
-- so this is the index it walks every tick.
CREATE INDEX events_unpublished ON events (id) WHERE published_at IS NULL;

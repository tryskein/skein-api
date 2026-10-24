-- 000013_watchers_extended — R3.3
--
-- Three additions to wire the R3.3 plan:
--
--   1. project_watchers.event_types — per-row whitelist of event types
--      the watcher cares about. NULL preserves R1.6 semantics ("all
--      notifiable types"); a non-NULL array is a strict whitelist applied
--      at FanOut time.
--
--   2. notifications.published_at — extends the R3.2a outbox dispatcher to
--      also publish materialized notifications onto the
--      `skein.user.{uid}.notification` subject. published_at IS NULL
--      identifies rows still pending push; the dispatcher marks rows
--      published in the same SKIP LOCKED transaction. Existing rows are
--      back-filled to now() before the partial index lands so the very
--      first dispatcher tick after deploy doesn't replay history.
--
--   3. notifications_unpublished — partial index used by the dispatcher's
--      SELECT … WHERE published_at IS NULL ORDER BY id pattern. Bounded
--      to the active backlog (all-time historical rows are excluded), so
--      query plan stays cheap regardless of total notification volume.

ALTER TABLE project_watchers ADD COLUMN event_types text[];

ALTER TABLE notifications ADD COLUMN published_at timestamptz;
UPDATE notifications SET published_at = now() WHERE published_at IS NULL;
CREATE INDEX notifications_unpublished ON notifications (id) WHERE published_at IS NULL;

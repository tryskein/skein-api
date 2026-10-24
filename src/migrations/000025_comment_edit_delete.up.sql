-- 000025 — Comment edit + delete (sk-57).
--
-- Three new columns on comments to support author-only edits and
-- author/owner soft-deletes, mirroring the optimistic-concurrency +
-- tombstone patterns already used elsewhere in the schema:
--
--   version    — optimistic-concurrency counter, bumped on every edit
--                (parallels issues.version). Edits pass expectedVersion;
--                a mismatch surfaces as version_conflict so the client
--                rebases against the live value.
--   edited_at  — NULL until the first edit; set to now() on each edit so
--                the UI can render an "edited" badge. The original
--                created_at is never touched.
--   deleted_at — NULL until soft-delete; set to now() on delete. The body
--                text stays in the row (audit trail) but ListComments
--                blanks it on read so deleted text never reaches clients.
--                The row remains in the thread as a tombstone slot.
--
-- All three default to a value that leaves existing rows in the
-- pre-feature state (version=1, never edited, never deleted), so the
-- migration is a pure additive — no backfill needed.
ALTER TABLE comments ADD COLUMN version int NOT NULL DEFAULT 1;
ALTER TABLE comments ADD COLUMN edited_at timestamptz NULL;
ALTER TABLE comments ADD COLUMN deleted_at timestamptz NULL;

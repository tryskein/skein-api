-- 000016 down — restore agents.label column.
--
-- The column comes back as nullable text (matching the original
-- 000010 declaration). Existing rows get NULL — there is no source
-- of truth to backfill from once the column is dropped, and the
-- application code that wrote to it has been removed in the same
-- slice.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS label text;

-- 000025 down — drop the comment edit/delete columns (reverse order).
ALTER TABLE comments DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE comments DROP COLUMN IF EXISTS edited_at;
ALTER TABLE comments DROP COLUMN IF EXISTS version;

-- sk-10.1 down: drop the user-side columns first (the FK on
-- custom_theme_id would otherwise block DROP TABLE themes) and then the
-- themes table itself. Each column is dropped explicitly so the down
-- migration is a faithful reverse of the up; ON DELETE SET NULL on
-- custom_theme_id means we don't have to scrub user rows beforehand.

ALTER TABLE users DROP COLUMN IF EXISTS custom_theme_id;
ALTER TABLE users DROP COLUMN IF EXISTS builtin_theme;
ALTER TABLE users DROP COLUMN IF EXISTS theme_mode;

DROP INDEX IF EXISTS themes_updated_at_idx;
DROP TABLE IF EXISTS themes;

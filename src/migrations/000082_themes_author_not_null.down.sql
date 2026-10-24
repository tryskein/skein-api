-- 000082_themes_author_not_null (down). Drops only NOT NULL: authors and
-- personal copies stay - they are correct for pre-T035 code too (see up).

ALTER TABLE themes ALTER COLUMN author_id DROP NOT NULL;

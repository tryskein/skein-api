-- 000006_search_indexes — DOWN
--
-- Reverse order of UP. Drop indexes first, then the generated columns —
-- Postgres refuses to drop a column an index is sitting on, so the order
-- matters even when DROP INDEX IF EXISTS would tolerate the missing object.

DROP INDEX IF EXISTS idx_components_tsv_en;
DROP INDEX IF EXISTS idx_components_tsv_ru;
ALTER TABLE components DROP COLUMN IF EXISTS tsv_en;
ALTER TABLE components DROP COLUMN IF EXISTS tsv_ru;

DROP INDEX IF EXISTS idx_projects_tsv_en;
DROP INDEX IF EXISTS idx_projects_tsv_ru;
ALTER TABLE projects DROP COLUMN IF EXISTS tsv_en;
ALTER TABLE projects DROP COLUMN IF EXISTS tsv_ru;

DROP INDEX IF EXISTS idx_comments_tsv_en;
DROP INDEX IF EXISTS idx_comments_tsv_ru;
ALTER TABLE comments DROP COLUMN IF EXISTS tsv_en;
ALTER TABLE comments DROP COLUMN IF EXISTS tsv_ru;

DROP INDEX IF EXISTS idx_issues_tsv_en;
DROP INDEX IF EXISTS idx_issues_tsv_ru;
ALTER TABLE issues DROP COLUMN IF EXISTS tsv_en;
ALTER TABLE issues DROP COLUMN IF EXISTS tsv_ru;

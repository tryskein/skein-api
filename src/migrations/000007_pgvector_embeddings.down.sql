-- 000007_pgvector_embeddings — DOWN
--
-- Drop indexes first, then columns. Pending partial indexes are dropped
-- before the HNSW indexes (no functional reason — keeps the order
-- symmetric with UP).

DROP INDEX IF EXISTS idx_components_embedding_pending;
DROP INDEX IF EXISTS idx_projects_embedding_pending;
DROP INDEX IF EXISTS idx_comments_embedding_pending;
DROP INDEX IF EXISTS idx_issues_embedding_pending;

DROP INDEX IF EXISTS idx_components_embedding;
DROP INDEX IF EXISTS idx_projects_embedding;
DROP INDEX IF EXISTS idx_comments_embedding;
DROP INDEX IF EXISTS idx_issues_embedding;

ALTER TABLE components DROP COLUMN IF EXISTS embedding;
ALTER TABLE projects   DROP COLUMN IF EXISTS embedding;
ALTER TABLE comments   DROP COLUMN IF EXISTS embedding;
ALTER TABLE issues     DROP COLUMN IF EXISTS embedding;

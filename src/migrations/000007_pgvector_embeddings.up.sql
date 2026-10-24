-- 000007_pgvector_embeddings — R1.5
--
-- Adds qwen3-embedding:0.6b (1024-dim float32) embedding columns to the
-- four searchable entities, plus HNSW indexes for cosine similarity and
-- partial pending indexes for the backfill worker.
--
-- Why halfvec?
--   pgvector's halfvec stores each component as float16 (2 bytes) instead of
--   float32 (4 bytes). For semantic search the recall loss is negligible
--   (<0.5% on standard benchmarks) and the index footprint halves. With
--   1024 dimensions a single vector is 2 KiB on disk — twice that with
--   float32 — and the HNSW graph traversal cost scales with vector size.
--
-- Why HNSW (not IVF)?
--   IVF requires a "training" build over a representative set of vectors.
--   On an empty table or one that grows incrementally that doesn't work.
--   HNSW builds incrementally: every INSERT/UPDATE updates the graph,
--   readers see the new vector immediately. m=16 / ef_construction=128 are
--   the pgvector defaults that hit the sweet spot for <10M-row tables.
--
-- Partial pending indexes:
--   `WHERE embedding IS NULL` — every row the worker hasn't processed yet.
--   `ORDER BY id` keyset traversal so the worker can pull a deterministic
--   batch with `FOR UPDATE SKIP LOCKED`. Once the row is filled in, the
--   index entry vanishes. In steady state the pending index is effectively
--   empty and costs nothing to maintain.
--
-- pgvector ≥ 0.7.0 is required for halfvec. The cluster image is
-- pgvector/pgvector:pg18 which ships 0.8.x; the application user only needs
-- USAGE on the extension (CREATE EXTENSION runs at cluster bootstrap as
-- superuser; see infra/postgres/init-extensions.sql).

ALTER TABLE issues     ADD COLUMN embedding halfvec(1024);
ALTER TABLE comments   ADD COLUMN embedding halfvec(1024);
ALTER TABLE projects   ADD COLUMN embedding halfvec(1024);
ALTER TABLE components ADD COLUMN embedding halfvec(1024);

CREATE INDEX idx_issues_embedding
    ON issues USING hnsw (embedding halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 128);

CREATE INDEX idx_comments_embedding
    ON comments USING hnsw (embedding halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 128);

CREATE INDEX idx_projects_embedding
    ON projects USING hnsw (embedding halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 128);

CREATE INDEX idx_components_embedding
    ON components USING hnsw (embedding halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 128);

-- Pending-row indexes for the embedding worker. SKIP LOCKED scan plays
-- nicely with multiple worker replicas — each picks up a disjoint batch.
CREATE INDEX idx_issues_embedding_pending
    ON issues (id) WHERE embedding IS NULL;

CREATE INDEX idx_comments_embedding_pending
    ON comments (id) WHERE embedding IS NULL;

CREATE INDEX idx_projects_embedding_pending
    ON projects (id) WHERE embedding IS NULL;

CREATE INDEX idx_components_embedding_pending
    ON components (id) WHERE embedding IS NULL;

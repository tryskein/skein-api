-- 000006_search_indexes — R1.5
--
-- Dual-language full-text search (ru + en) on every searchable entity:
-- issues, comments, projects, components. Each table gains two STORED
-- generated tsvector columns plus a GIN index per language.
--
-- Why two languages?  The org runs Russian + English content side by side.
-- A single 'simple' / 'pg_catalog.simple' tsvector misses morphology in
-- both dialects; running both 'russian' and 'english' configurations and
-- summing ts_rank captures stems no matter which side the document is on.
--
-- Column weights:
--   A = title / name           — most discriminative, wins ts_rank ties
--   B = description            — secondary anchor
--   C = body / acceptance / design / notes  — long-form context
--
-- Generated STORED columns mean writes pay the tsvector cost once at INSERT/
-- UPDATE; reads use the GIN index directly. The reset-on-text-change
-- behaviour for embeddings (R1.5 worker) is independent of these columns —
-- tsvector regenerates automatically on any text-column UPDATE.

-- ---------- issues ----------
ALTER TABLE issues
    ADD COLUMN tsv_ru tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('russian', coalesce(title, '')), 'A') ||
            setweight(to_tsvector('russian', coalesce(description, '')), 'B') ||
            setweight(to_tsvector('russian', coalesce(acceptance_criteria, '')), 'C') ||
            setweight(to_tsvector('russian', coalesce(design, '')), 'C') ||
            setweight(to_tsvector('russian', coalesce(notes, '')), 'C')
        ) STORED,
    ADD COLUMN tsv_en tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
            setweight(to_tsvector('english', coalesce(description, '')), 'B') ||
            setweight(to_tsvector('english', coalesce(acceptance_criteria, '')), 'C') ||
            setweight(to_tsvector('english', coalesce(design, '')), 'C') ||
            setweight(to_tsvector('english', coalesce(notes, '')), 'C')
        ) STORED;

CREATE INDEX idx_issues_tsv_ru ON issues USING gin (tsv_ru);
CREATE INDEX idx_issues_tsv_en ON issues USING gin (tsv_en);

-- ---------- comments ----------
ALTER TABLE comments
    ADD COLUMN tsv_ru tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('russian', coalesce(body, '')), 'A')
        ) STORED,
    ADD COLUMN tsv_en tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('english', coalesce(body, '')), 'A')
        ) STORED;

CREATE INDEX idx_comments_tsv_ru ON comments USING gin (tsv_ru);
CREATE INDEX idx_comments_tsv_en ON comments USING gin (tsv_en);

-- ---------- projects ----------
ALTER TABLE projects
    ADD COLUMN tsv_ru tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('russian', coalesce(name, '')), 'A') ||
            setweight(to_tsvector('russian', coalesce(description, '')), 'B')
        ) STORED,
    ADD COLUMN tsv_en tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('english', coalesce(name, '')), 'A') ||
            setweight(to_tsvector('english', coalesce(description, '')), 'B')
        ) STORED;

CREATE INDEX idx_projects_tsv_ru ON projects USING gin (tsv_ru);
CREATE INDEX idx_projects_tsv_en ON projects USING gin (tsv_en);

-- ---------- components ----------
ALTER TABLE components
    ADD COLUMN tsv_ru tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('russian', coalesce(name, '')), 'A') ||
            setweight(to_tsvector('russian', coalesce(description, '')), 'B')
        ) STORED,
    ADD COLUMN tsv_en tsvector
        GENERATED ALWAYS AS (
            setweight(to_tsvector('english', coalesce(name, '')), 'A') ||
            setweight(to_tsvector('english', coalesce(description, '')), 'B')
        ) STORED;

CREATE INDEX idx_components_tsv_ru ON components USING gin (tsv_ru);
CREATE INDEX idx_components_tsv_en ON components USING gin (tsv_en);

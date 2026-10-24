-- 000038_users_directory_prefix — sk-285.5.5 review F5
--
-- GET /api/v1/users/directory searches with a leading-wildcard ILIKE and
-- leans on the pg_trgm GIN indexes from 000037. Those cannot serve a term
-- shorter than three characters: pg_trgm extracts no COMPLETE trigram from
-- such a pattern, so there is nothing to look up and the planner falls back
-- to a full scan of `users`. That is not a rare shape — 1-2 characters is
-- what a typeahead sends on the first keystrokes of EVERY search, so the
-- unservable case was the common one.
--
-- users.Store.listDirectory now matches short terms by PREFIX instead
-- (`LIKE 'ad%'`), which a btree can serve. A btree built with the DEFAULT
-- operator class cannot: this database is en_US.utf8, and under a non-C
-- collation `idx_users_active_username` (000037) is ordered by collation
-- rules that do not match LIKE's byte-prefix semantics, so the planner will
-- not use it for a LIKE prefix. text_pattern_ops indexes the same columns in
-- plain byte order, which is exactly what LIKE needs.
--
-- Measured on a test dataset (39 782 active users, PG18,
-- EXPLAIN (ANALYZE, BUFFERS), LIMIT 51):
--
--   q='ad'  before  ILIKE '%ad%'  Seq Scan, shared hit=2910,      23.18 ms
--   q='ad'  after   LIKE  'ad%'   BitmapOr, shared hit=7 read=6,   0.12 ms
--   q='u'   after   LIKE  'u%'    BitmapOr, shared hit=369 read=8, 1.03 ms
--           (573 matching rows — the broadest single letter in the fixture)
--
-- Both rows are the FULL statement the store emits, ranking CASE and all,
-- not a stripped-down predicate.
--
-- 1) idx_users_active_username_pattern — username is CHECK-constrained to
--    lowercase (000001_init) and the search term is lowercased in the store,
--    so the username half is a case-SENSITIVE LIKE and indexes the bare
--    column. Partial on `deactivated_at IS NULL`, matching the endpoint's
--    invariant WHERE clause.
--
-- 2) idx_users_active_name_lower_pattern — `name` is free-form, so the case
--    fold has to happen somewhere; it happens in the predicate as
--    `lower(name) LIKE …` and the index is built on the same expression.
--
-- Both are prefix-only helpers. The 000037 trigram indexes stay: they serve
-- every term of three characters or more, which keeps full substring
-- matching for real searches.

CREATE INDEX idx_users_active_username_pattern
    ON users (username text_pattern_ops)
    WHERE deactivated_at IS NULL;

CREATE INDEX idx_users_active_name_lower_pattern
    ON users (lower(name) text_pattern_ops)
    WHERE deactivated_at IS NULL;

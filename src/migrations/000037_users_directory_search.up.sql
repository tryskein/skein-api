-- 000037_users_directory_search — sk-285.5.5
--
-- GET /api/v1/users/directory became a bounded, searchable listing
-- (users.Store.listDirectory). Neither of its two access shapes had a
-- usable index; both degraded to a full seq scan of `users` on every
-- assignee-picker interaction. Measured on a test dataset (39 782 active
-- users, PG18, EXPLAIN (ANALYZE, BUFFERS)):
--
--   no search, ORDER BY username LIMIT 50
--     before: Seq Scan + top-N heapsort, shared hit=2904, 8.9 ms
--     after : Index Scan (idx_users_active_username), hit=50 read=3, 0.08 ms
--
--   search, (username ILIKE '%adm%' OR name ILIKE '%adm%') LIMIT 50
--     before: Seq Scan, shared hit=2904, 19.4 ms
--     after : BitmapOr over the two trigram indexes, shared hit=7, 0.10 ms
--
-- 1) idx_users_active_username — partial btree matching the endpoint's
--    default (unsearched) shape exactly: the WHERE is `deactivated_at IS
--    NULL` and the ORDER BY is bare `username`, so the LIMIT is served
--    straight out of the index with no sort node. The pre-existing
--    idx_users_username_lower cannot do this: it is an index on the
--    EXPRESSION lower(username), which the planner will not match to an
--    `ORDER BY username`.
--
-- 2/3) trigram GIN over username and name — a leading-wildcard
--    `ILIKE '%q%'` is unindexable by btree by construction. pg_trgm is
--    already a cluster-level extension here (used by
--    idx_projects_name_trgm, idx_issues_title_trgm), and gin_trgm_ops
--    serves ILIKE directly, which is why the store searches with ILIKE on
--    the bare column instead of `lower(col) LIKE` (the latter would need
--    the index built on the same expression). Separate per-column indexes,
--    not one composite: the predicate is an OR, and the planner combines
--    them with BitmapOr — a composite would not help either branch.
--    Terms shorter than 3 characters carry no full trigram and still fall
--    back to a scan; that is inherent to pg_trgm, and the LIMIT bounds it.

CREATE INDEX idx_users_active_username
    ON users (username)
    WHERE deactivated_at IS NULL;

CREATE INDEX idx_users_username_trgm ON users USING gin (username gin_trgm_ops);
CREATE INDEX idx_users_name_trgm     ON users USING gin (name gin_trgm_ops);

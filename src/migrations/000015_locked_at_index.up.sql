-- Partial index supporting the stale-lock reaper's sweep query
-- (internal/issues/lockreap/reaper.go::reapOnce):
--
--     SELECT id, project_id, locked_by, locked_at, ...
--       FROM issues
--      WHERE locked_by IS NOT NULL
--        AND locked_at < now() - $1::interval
--      LIMIT $2
--
-- Without this index the reaper scans the full issues table every
-- tick. The CHECK constraint issues_locked_consistency
-- (000004_issues.up.sql) enforces locked_by IS NULL ↔ locked_at IS
-- NULL, so the partial-index predicate is exact: every row with a
-- non-NULL locked_at also has a non-NULL locked_by, and vice versa.
--
-- Indexing locked_at (not locked_by) lets the reaper's threshold
-- range-scan the index directly. Cardinality of the predicate set is
-- bounded by the number of currently-locked issues (typically a few
-- per fleet, near zero on a healthy system).
CREATE INDEX idx_issues_locked_at_partial
    ON issues (locked_at)
    WHERE locked_by IS NOT NULL;

-- Rollback of 000047 — sk-303.14, task T017.
--
-- The index is pure performance: dropping it loses no rows and does not
-- change query answers, only their cost. So rollback is unconditional and
-- complete, with no caveats about data preservation.

DROP INDEX IF EXISTS idx_issues_unaccepted_assignee;

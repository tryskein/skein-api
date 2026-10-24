-- Rollback of 000063_issue_watchers_no_tombstone_validate — intentionally empty.
--
-- PostgreSQL has no operation that returns a validated constraint to
-- NOT VALID (except editing pg_constraint, which is forbidden). The state
-- "constraint validated" is harmless for version 000062: the constraint is the same,
-- it checks new rows identically. The constraint is dropped by down 000062,
-- which follows on a rollback R2 → R1 (DROP CONSTRAINT IF EXISTS — regardless of
-- whether it is validated).
SELECT 1;

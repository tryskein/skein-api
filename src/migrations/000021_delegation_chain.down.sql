-- 000021 down — drop the delegation chain columns + indexes.
--
-- Per the sk-31 convention (see 000019_drop_blocked_status.down.sql), we
-- accept the data loss on rollback: any previous_assignees / dismissed_by
-- history accumulated since the up-migration is irrecoverable once these
-- columns are dropped. Operators planning a rollback should pg_dump first
-- if the audit trail matters.
--
-- IF EXISTS keeps the down idempotent so a re-run after a partial failure
-- doesn't error. Reverse order: drop indexes first (they depend on the
-- columns), then the columns.
DROP INDEX IF EXISTS idx_issues_previous_assignees;
DROP INDEX IF EXISTS idx_issues_dismissed_by;
ALTER TABLE issues DROP COLUMN IF EXISTS previous_assignees;
ALTER TABLE issues DROP COLUMN IF EXISTS dismissed_by;

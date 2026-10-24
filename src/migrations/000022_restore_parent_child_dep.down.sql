-- sk-35 down: forward-compat revert ONLY. Pure revert is INCOMPLETE if a
-- `parent-child` dep row was added post-migration through the normal app
-- code path (CreateIssue inlines the INSERT directly, the MCP dep tools
-- accept the type, etc). We only delete the rows we backfilled — anyone
-- else added stays, mirroring the sk-33 pattern (000020 was itself
-- non-revertable per its down.sql header). Tightening the CHECK after
-- such rows exist would fail; the operator has to clean them up first.

DELETE FROM dependencies
 WHERE type = 'parent-child'
   AND metadata->>'migration_note' = 'backfilled from issues.parent_id by 000022';

ALTER TABLE dependencies DROP CONSTRAINT dependencies_type_check;
ALTER TABLE dependencies ADD CONSTRAINT dependencies_type_check
  CHECK (type IN ('blocks','waits-for','related','discovered-from',
                  'supersedes','duplicates'));

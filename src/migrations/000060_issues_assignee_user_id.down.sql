-- Rollback of 000060_issues_assignee_user_id.
--
-- Fully reversible for the schema: triggers, functions, indexes and the column
-- are dropped, issues.assignee (the assignee text) is not touched — the previous
-- code version resolves the assignee from it, as before the migration. The
-- assignee_user_id values are lost; they are derivable from the text and on a repeated up
-- are restored by the backfill (T020).

DROP INDEX IF EXISTS idx_issues_assignee_lower;
DROP INDEX IF EXISTS idx_issues_assignee_user_active;

DROP TRIGGER IF EXISTS users_resync_assignee_user_id_upd ON users;
DROP TRIGGER IF EXISTS users_resync_assignee_user_id_ins ON users;
DROP TRIGGER IF EXISTS issues_resolve_assignee_user_id ON issues;

DROP FUNCTION IF EXISTS skein_users_resync_assignee_user_id();
DROP FUNCTION IF EXISTS skein_issues_resolve_assignee_user_id();
DROP FUNCTION IF EXISTS resolve_assignee_user_id(text);

ALTER TABLE issues DROP COLUMN IF EXISTS assignee_user_id;

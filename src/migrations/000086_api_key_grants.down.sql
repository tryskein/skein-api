-- 000086_api_key_grants (down) - drops the tables api_key_scope_projects and
-- api_key_scope_spaces with their rows, and the api_keys columns level,
-- is_admin, scope_mode, expires_at, reissued_from_id with their constraints
-- and index. The keys themselves stay; after the rollback every key works as
-- before 000086, with its owner's rights and no expiry.

DROP TABLE IF EXISTS api_key_scope_spaces;
DROP TABLE IF EXISTS api_key_scope_projects;

DROP INDEX IF EXISTS idx_api_keys_reissued_from;

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_admin_scope_all,
    DROP CONSTRAINT IF EXISTS api_keys_has_permission,
    DROP CONSTRAINT IF EXISTS api_keys_scope_mode_check,
    DROP CONSTRAINT IF EXISTS api_keys_level_check,
    DROP COLUMN IF EXISTS reissued_from_id,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS scope_mode,
    DROP COLUMN IF EXISTS is_admin,
    DROP COLUMN IF EXISTS level;

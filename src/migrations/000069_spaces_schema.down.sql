-- 000069_spaces_schema (down) — reverse order of up.

ALTER TABLE themes DROP COLUMN IF EXISTS author_id;

DROP TABLE IF EXISTS space_storage_usage;
DROP TABLE IF EXISTS reserved_prefixes;
DROP TABLE IF EXISTS reserved_identities;

DROP INDEX IF EXISTS idx_users_pseudonym;
ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_deleted_has_pseudonym,
    DROP COLUMN IF EXISTS session_epoch,
    DROP COLUMN IF EXISTS pseudonym,
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS terms_accepted_at,
    DROP COLUMN IF EXISTS email_confirmed_at;

ALTER TABLE project_members
    DROP CONSTRAINT IF EXISTS project_members_has_source,
    DROP COLUMN IF EXISTS via_space,
    DROP COLUMN IF EXISTS via_manual;

DROP INDEX IF EXISTS idx_projects_space;
ALTER TABLE projects
    DROP COLUMN IF EXISTS visibility,
    DROP COLUMN IF EXISTS space_id;

DROP TABLE IF EXISTS space_invitations;
DROP TABLE IF EXISTS space_members;
DROP TABLE IF EXISTS spaces;

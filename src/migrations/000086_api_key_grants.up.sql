-- 000086_api_key_grants - permissions, scope and lifetime of an API key.
--
-- A key gets:
--   * level      - read | write | manage, where a higher level includes the
--                  lower ones; NULL only on a key that carries just the admin
--                  flag;
--   * is_admin   - the admin permission, independent of the level;
--   * scope_mode - projects | spaces | all; the members of a projects or
--                  spaces scope live in api_key_scope_projects and
--                  api_key_scope_spaces;
--   * expires_at - NULL only on keys created before this migration;
--   * reissued_from_id - the key this one was reissued from.
--
-- The new columns have no DEFAULT on purpose: a writer that does not name the
-- grant fails instead of creating a key with permissions it never chose.
--
-- Backfill: every existing key keeps what it could do before - level manage,
-- the admin flag only when the owner is an instance admin, scope all, no
-- expiry. Secret hash, prefix, label and last_used_at are not touched.
--
-- Table constraints hold the grant rules that do not depend on the clock:
-- at least one permission, and the admin flag only with scope all.

ALTER TABLE api_keys
    ADD COLUMN level            text,
    ADD COLUMN is_admin         boolean,
    ADD COLUMN scope_mode       text,
    ADD COLUMN expires_at       timestamptz,
    ADD COLUMN reissued_from_id uuid REFERENCES api_keys(id) ON DELETE SET NULL;

UPDATE api_keys k
   SET level      = 'manage',
       is_admin   = (u.role = 'admin'),
       scope_mode = 'all'
  FROM users u
 WHERE u.id = k.user_id;

ALTER TABLE api_keys
    ALTER COLUMN is_admin   SET NOT NULL,
    ALTER COLUMN scope_mode SET NOT NULL,
    ADD CONSTRAINT api_keys_level_check
        CHECK (level IN ('read', 'write', 'manage')),
    ADD CONSTRAINT api_keys_scope_mode_check
        CHECK (scope_mode IN ('projects', 'spaces', 'all')),
    ADD CONSTRAINT api_keys_has_permission
        CHECK (level IS NOT NULL OR is_admin),
    ADD CONSTRAINT api_keys_admin_scope_all
        CHECK (NOT is_admin OR scope_mode = 'all');

CREATE INDEX idx_api_keys_reissued_from ON api_keys (reissued_from_id)
    WHERE reissued_from_id IS NOT NULL;

COMMENT ON COLUMN api_keys.level IS
    'Permission level: read, write or manage (a higher level includes the lower ones). NULL only when the key carries just the admin flag.';
COMMENT ON COLUMN api_keys.is_admin IS
    'Admin permission, independent of the level; allowed only with scope_mode all. Effective only while the owner is an instance admin.';
COMMENT ON COLUMN api_keys.scope_mode IS
    'Scope of the key: projects or spaces (members in api_key_scope_projects / api_key_scope_spaces) or all.';
COMMENT ON COLUMN api_keys.expires_at IS
    'End of the key lifetime. NULL only on keys created before 000086.';
COMMENT ON COLUMN api_keys.reissued_from_id IS
    'The key this one was reissued from; NULL for a key created directly.';

CREATE TABLE api_key_scope_projects (
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (api_key_id, project_id)
);

CREATE INDEX idx_api_key_scope_projects_project ON api_key_scope_projects (project_id);

CREATE TABLE api_key_scope_spaces (
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    PRIMARY KEY (api_key_id, space_id)
);

CREATE INDEX idx_api_key_scope_spaces_space ON api_key_scope_spaces (space_id);

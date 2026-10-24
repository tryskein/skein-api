-- 000072_spaces_constraints (down). Global uniqueness of the project name
-- and the previous users_auth_method are restored; the rollback fails if the data already
-- has same-named projects in different spaces or deleted accounts without
-- a sign-in method - resolve manually.

DROP INDEX IF EXISTS idx_projects_space_name_lower;
CREATE UNIQUE INDEX idx_projects_name_lower ON projects (lower(name));

ALTER TABLE users
    DROP CONSTRAINT users_auth_method,
    ADD CONSTRAINT users_auth_method
        CHECK (password_hash IS NOT NULL OR keycloak_sub IS NOT NULL);

-- 000072_spaces_constraints - sk-674.18, T016 (K-23 step 3, D-4, K-21).
--
-- WHY. Tightening after the 000071 transition.
--   * The project name is unique within a space (D-4), not globally:
--     idx_projects_name_lower is replaced by (space_id, lower(name)). The name of
--     the new index contains "name_lower" - handler/projects.go#
--     mapConflict uses it to return name_taken, as before. NULLS NOT DISTINCT:
--     projects created before T024 by a write path without space_id share one
--     namespace, as before.
--   * users_auth_method allows a deleted account without a sign-in method
--     (K-21: anonymization erases the password and keycloak_sub).
--
-- NOT HERE (lead's decision, sk-674.18): projects.space_id NOT NULL - in
-- T024, themes.author_id NOT NULL - in T035, each as a migration in the commit of
-- its write path. Theme name uniqueness per author - in 000071 (it is
-- required by the personal theme copies).

DROP INDEX idx_projects_name_lower;
CREATE UNIQUE INDEX idx_projects_space_name_lower
    ON projects (space_id, lower(name)) NULLS NOT DISTINCT;

ALTER TABLE users
    DROP CONSTRAINT users_auth_method,
    ADD CONSTRAINT users_auth_method
        CHECK (deleted_at IS NOT NULL OR password_hash IS NOT NULL OR keycloak_sub IS NOT NULL);

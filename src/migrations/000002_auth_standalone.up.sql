-- 000002_auth_standalone — R1.1
--
-- Adds standalone-auth columns to users (F245 password hash, F246 last login)
-- and the partial unique index on Keycloak subject (F242, F252) so a single
-- user row can carry both auth methods (hybrid mode) without a NULL conflict.
--
-- The users_auth_method CHECK enforces "every row has at least one auth path":
-- standalone (password_hash NOT NULL), Keycloak (keycloak_sub NOT NULL), or
-- both. This catches a user-creation bug at the DB layer instead of letting a
-- ghost row exist that nobody can ever log in as.

ALTER TABLE users
    ADD COLUMN password_hash text,
    ADD COLUMN keycloak_sub  text,
    ADD COLUMN last_login_at timestamptz;

CREATE UNIQUE INDEX idx_users_keycloak_sub
    ON users (keycloak_sub)
    WHERE keycloak_sub IS NOT NULL;

ALTER TABLE users
    ADD CONSTRAINT users_auth_method
    CHECK (password_hash IS NOT NULL OR keycloak_sub IS NOT NULL);

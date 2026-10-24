-- 000002_auth_standalone — R1.1 (down)
--
-- Reverses the auth-standalone migration: drop the auth-method CHECK, the
-- partial unique index on keycloak_sub, then the three columns. Order
-- matters: the CHECK references password_hash and keycloak_sub, so it must
-- be dropped before the columns.

ALTER TABLE users
    DROP CONSTRAINT users_auth_method;

DROP INDEX idx_users_keycloak_sub;

ALTER TABLE users
    DROP COLUMN last_login_at,
    DROP COLUMN keycloak_sub,
    DROP COLUMN password_hash;

-- 000033_external_users — sk-163 S1
--
-- Adds the `is_external` flag (external collaborator marker) to users. The
-- flag is orthogonal to `role`: an external user is role='user' +
-- is_external=true. It is the visibility ceiling — later slices (S2/S3/S6)
-- derive `IsAdmin()` as `role='admin' AND NOT is_external` and scope
-- cross-project people surfaces by it.
--
-- DEFAULT false → every existing row is internal, so this migration is a
-- behaviour no-op until later slices enforce the flag.
--
-- The partial index covers the admin-directory / surface-gate queries that
-- select only the (rare) external rows; internal rows (the overwhelming
-- majority) carry no index entry.
--
-- users_external_not_admin is the same-table half of the DB backstop
-- (spec §5): is_external=true AND role='admin' can never coexist. The
-- cross-table owner backstop is a trigger added in a later slice (S3).

ALTER TABLE users
    ADD COLUMN is_external boolean NOT NULL DEFAULT false;

CREATE INDEX idx_users_is_external
    ON users (is_external)
    WHERE is_external;

ALTER TABLE users
    ADD CONSTRAINT users_external_not_admin
    CHECK (NOT (is_external AND role = 'admin'));

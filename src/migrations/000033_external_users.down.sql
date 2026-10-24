-- Revert 000033_external_users.
--
-- Drop in reverse order of creation: constraint, then partial index, then
-- the column. IF EXISTS keeps the down idempotent if a partial apply left
-- only some objects behind.

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_external_not_admin;
DROP INDEX IF EXISTS idx_users_is_external;
ALTER TABLE users DROP COLUMN IF EXISTS is_external;

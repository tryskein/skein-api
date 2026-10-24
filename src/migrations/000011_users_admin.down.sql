-- Revert 000011_users_admin.

DROP INDEX IF EXISTS idx_users_active;
ALTER TABLE users DROP COLUMN IF EXISTS deactivated_at;

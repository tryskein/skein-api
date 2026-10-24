-- Revert 000034_external_owner_trigger.
--
-- Drop the triggers first (they depend on the function), then the function.
-- IF EXISTS keeps the down idempotent if a partial apply left only some
-- objects behind.

DROP TRIGGER IF EXISTS users_external_not_owner ON users;
DROP TRIGGER IF EXISTS project_members_external_not_owner ON project_members;
DROP FUNCTION IF EXISTS skein_enforce_external_not_owner();

-- 000010_api_keys_agents — R2.1a (down)
--
-- Reverse the up migration. Drop in opposite order so any stray FK from
-- elsewhere doesn't catch us, although neither table is currently
-- referenced from outside.

DROP INDEX IF EXISTS idx_agents_last_seen;
DROP TABLE IF EXISTS agents;

DROP INDEX IF EXISTS idx_api_keys_prefix_active;
DROP INDEX IF EXISTS idx_api_keys_active;
DROP INDEX IF EXISTS idx_api_keys_user;
DROP TABLE IF EXISTS api_keys;

-- 000088_agents_rest_holder - the REST executor in the agents registry.
--
-- On the public REST API the lock holder is not an MCP session but an
-- executor of an API key: the key itself, or the key plus the client-chosen
-- X-Skein-Worker-Id. Its issues.locked_by text lives in the namespace
-- api:<keyId> or api:<keyId>:<workerId>, so it never equals an MCP session
-- id. Its liveness is the same agents row with last_seen_at, so the stale
-- lock reaper keeps one threshold for both kinds of holder.
--
--   * api_key_id - the key of a REST executor row; NULL on an MCP session row.
--                  Deleting the key deletes its executor rows.
--   * worker_id  - X-Skein-Worker-Id of the executor; NULL when the key itself
--                  holds the lock. Never set without api_key_id.

ALTER TABLE agents
    ADD COLUMN api_key_id uuid REFERENCES api_keys(id) ON DELETE CASCADE,
    ADD COLUMN worker_id  text,
    ADD CONSTRAINT agents_worker_needs_key
        CHECK (worker_id IS NULL OR api_key_id IS NOT NULL);

CREATE INDEX idx_agents_api_key ON agents (api_key_id) WHERE api_key_id IS NOT NULL;

COMMENT ON COLUMN agents.api_key_id IS
    'API key of a REST executor row (session_id api:<keyId>[:<workerId>]); NULL on an MCP session row.';
COMMENT ON COLUMN agents.worker_id IS
    'X-Skein-Worker-Id of a REST executor; NULL when the key itself is the executor.';

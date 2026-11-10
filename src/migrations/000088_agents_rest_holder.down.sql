-- 000088_agents_rest_holder (down) - deletes the REST executor rows of agents
-- (api_key_id IS NOT NULL), then drops the agents columns api_key_id and
-- worker_id with their constraint and index. Without the columns such rows
-- would read as MCP sessions. MCP session rows are kept.

DELETE FROM agents WHERE api_key_id IS NOT NULL;

DROP INDEX IF EXISTS idx_agents_api_key;

ALTER TABLE agents
    DROP CONSTRAINT IF EXISTS agents_worker_needs_key,
    DROP COLUMN IF EXISTS worker_id,
    DROP COLUMN IF EXISTS api_key_id;

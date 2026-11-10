-- 000090_agents_key_on_mcp_sessions (down) - clears api_key_id on MCP
-- session rows (session_id not in the api: namespace) and restores the
-- 000088 comment, so 000088 down still reads api_key_id IS NOT NULL as a
-- REST executor row. REST executor rows are kept.

UPDATE agents SET api_key_id = NULL WHERE session_id NOT LIKE 'api:%';

COMMENT ON COLUMN agents.api_key_id IS
    'API key of a REST executor row (session_id api:<keyId>[:<workerId>]); NULL on an MCP session row.';

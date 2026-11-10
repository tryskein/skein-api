-- 000090_agents_key_on_mcp_sessions - agents.api_key_id names the key of
-- every holder that called with one.
--
-- An MCP session called with an API key records that key too, not only a
-- REST executor: the stale lock reaper keeps a holder alive only while its
-- key is active, has the write right and covers the issue, whatever the
-- channel. A REST executor row is told apart by its session_id
-- (api:<keyId>[:<workerId>]), not by api_key_id. Comment change only; no
-- data is rewritten - existing MCP rows get the key on their next request.

COMMENT ON COLUMN agents.api_key_id IS
    'API key the holder called with: a REST executor (session_id api:<keyId>[:<workerId>]) or an MCP session that used a key; NULL for a session without a key.';

-- 000010_api_keys_agents — R2.1a
--
-- Long-lived API keys for service accounts and dev-container agents
-- (vision §3 Auth API Keys, F243, F255), plus the lazy agent registry that
-- maps MCP session ids to users (F226, F227).
--
-- API-key lookup design. db-schema.md §2.2 originally allowed an O(N)
-- iterating bcrypt compare across every active key for a user. That breaks
-- on the MCP path: each /mcp request arrives with X-Skein-Api-Key alone, so
-- we don't yet know the user_id and can't even narrow the search before the
-- bcrypt loop. We add a non-secret `prefix` column (first 12 chars of the
-- plain key, captured at create time) so the middleware can run a single
-- WHERE prefix=$1 lookup → one bcrypt verify → identified user. Twelve
-- chars × 64 base64-url alphabet ≈ 4.7e21 combinations, far more than any
-- realistic key-set, so the partial UNIQUE INDEX below catches the
-- effectively-impossible collision at INSERT time rather than letting two
-- keys share a prefix and then hashing each one.
--
-- The agents table is a lazy registry — every authenticated MCP request
-- UPSERTs (user_id, session_id) and bumps last_seen_at, so we get a
-- best-effort directory of who is connected without any explicit
-- registration step. Composite PK: there's no surrogate uuid because no
-- other table FK's into agents (issues.locked_by stores session_id as
-- text — see db-schema.md §2.3).

CREATE TABLE api_keys (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_hash        text NOT NULL,
    prefix          text NOT NULL,
    label           text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz,
    revoked_at      timestamptz
);

CREATE INDEX idx_api_keys_user ON api_keys (user_id);
CREATE INDEX idx_api_keys_active ON api_keys (user_id) WHERE revoked_at IS NULL;

-- Partial UNIQUE on prefix scoped to active keys: revoked keys keep their
-- prefix for audit but no longer participate in lookup. Two distinct
-- generations colliding on 12-char prefix is astronomically unlikely; if
-- it ever happens we surface it at INSERT time (handler retries) instead
-- of letting both keys silently coexist and produce ambiguous lookups.
CREATE UNIQUE INDEX idx_api_keys_prefix_active
    ON api_keys (prefix)
    WHERE revoked_at IS NULL;

CREATE TABLE agents (
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id      text NOT NULL,
    label           text,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, session_id)
);

-- Composite PK already covers (user_id, session_id) lookups. last_seen_at
-- index supports stale-lock auto-release queries (F230) — added now so the
-- R5.3 cleanup job (TODO: cleanup goroutine) doesn't need a follow-up
-- migration. No partial WHERE clause: NOW() is not IMMUTABLE so it can't
-- live in an index predicate, and there's no other selective filter that
-- would help here.
CREATE INDEX idx_agents_last_seen ON agents (last_seen_at);

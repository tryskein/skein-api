-- 000057_monitor_liveness — sk-579 (T004).
--
-- WHY. An issue monitor must not die silently (sk-579). The server needs to
-- know about every monitor issued after the rollout well enough to
-- (a) record a successful poll, (b) record completion on closing of
-- the root issue (INV-12), (c) find a stalled monitor and remind about it
-- in an MCP tool response (US-5, INV-5), no more than once per 5 minutes per
-- recipient session (INV-7). The migration is additive, no backfill: the old
-- binary works on the new schema, the existing monitor_credentials operators
-- do not change.
--
-- ⚠ issued_session_id — NULL and '' are deliberately different.
--   NULL  — the credential was issued BEFORE the rollout: the issuing session is unknown, such a
--           monitor does not get into the hint (INV-5) and lives out its ≤ 12 h
--           under the old protocol;
--   ''    — issued after the rollout by a client without Mcp-Session-Id.
--   Therefore there is neither NOT NULL DEFAULT '' nor a backfill: both would erase
--   the difference.
--
-- token_file_path — the token file path, given by the server as a literal (sk-239:
--   client input does not get here). NULL — issued before the rollout.
--
-- last_poll_succeeded_at — time of the last successful poll (a 200 response with
--   a body, INV-5). NULL — there have been no successful polls yet: the "stalled" countdown runs
--   from the issue time.
--
-- root_closed_at — the moment when a poll recorded completion on closing of
--   the root (INV-12). NULL — not recorded. The "root_closed" flag = NOT NULL.
--   A time column, not a boolean NOT NULL DEFAULT false: for a credential issued
--   before the rollout all new columns stay NULL, and the saved time
--   will be useful for analysis. Reopening the root does not clear the flag.
ALTER TABLE monitor_credentials
    ADD COLUMN issued_session_id      text,
    ADD COLUMN token_file_path        text,
    ADD COLUMN last_poll_succeeded_at timestamptz,
    ADD COLUMN root_closed_at         timestamptz;

COMMENT ON COLUMN monitor_credentials.issued_session_id IS
    'Mcp-Session-Id of the session that issued the monitor. NULL — issued before sk-579 '
    '(session unknown, monitor is outside the monitorStalled hint); '''' — client '
    'without Mcp-Session-Id.';
COMMENT ON COLUMN monitor_credentials.token_file_path IS
    'Token file path given by the server at issue time (a server literal). '
    'NULL — issued before sk-579.';
COMMENT ON COLUMN monitor_credentials.last_poll_succeeded_at IS
    'Time of the last successful poll of /agent/v1/notifications/poll '
    '(200 with a body). NULL — there were no successful polls.';
COMMENT ON COLUMN monitor_credentials.root_closed_at IS
    'When a poll recorded completion of the monitor on closing of the root '
    'issue (INV-12 sk-579). NULL — not recorded.';

-- A partial index of live candidates for "stalled" by issuing key.
-- The monitorStalled hint is addressed to the key that issued the token (INV-8),
-- so the selection goes by minted_by_api_key_id and does not scan the table (NFR).
-- The index condition is the constant part of the stalled-selection predicate (T014):
-- not revoked, not completed by root closing, issuing session known.
-- Lifetime (expires_at, absolute_deadline) is not part of the condition: now()
-- is not allowed in a partial index predicate.
CREATE INDEX monitor_credentials_stall_candidates_idx
    ON monitor_credentials (minted_by_api_key_id)
    WHERE revoked_at IS NULL
      AND root_closed_at IS NULL
      AND issued_session_id IS NOT NULL;

-- Marks of showing the monitorStalled hint: (monitor × recipient session) →
-- time of the last show. Shared by all API replicas: strict throttling
-- "no more than once per 5 minutes" is held by a conditional upsert on the primary key
-- (row lock in PG, INV-7), not by replica memory. The recipient session
-- may be '' (a call without Mcp-Session-Id) — that is also a separate recipient.
-- They are deleted together with the monitor (ON DELETE CASCADE), including cleanup
-- of expired and revoked credentials.
CREATE TABLE monitor_stall_notices (
    monitor_id           uuid        NOT NULL REFERENCES monitor_credentials(id) ON DELETE CASCADE,
    recipient_session_id text        NOT NULL,
    shown_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (monitor_id, recipient_session_id)
);

COMMENT ON TABLE monitor_stall_notices IS
    'Last show of the monitorStalled hint about the monitor to the recipient session '
    '(sk-579, INV-7). PK on the pair; deleted by cascade with the monitor.';

-- Time of the last MCP tool call without Mcp-Session-Id with this key.
-- The only source of the rule "an empty-session issuer went quiet" (US-5): for
-- a non-empty session the same fact is given by agents.last_seen_at, and there are no agents rows
-- with an empty session. Written no more than once per 60 s per key (the granularity of
-- agents). NULL — there were no calls without a session.
ALTER TABLE api_keys
    ADD COLUMN sessionless_seen_at timestamptz;

COMMENT ON COLUMN api_keys.sessionless_seen_at IS
    'Last MCP tool call without Mcp-Session-Id with this key '
    '(granularity 60 s, sk-579). NULL — there were no such calls.';

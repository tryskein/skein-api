-- 000030_monitor_credentials — sk-142.1 (T1)
--
-- Short-lived, scoped credential for the agent task-monitor (inter-agent
-- comms over comments). A monitor credential is the `skm_`-prefixed opaque
-- token a headless agent's poll runner authenticates with on the separate
-- /agent/v1 api-key door — NEVER a real ska_ api key..
--
-- Auth design highlights baked into this schema:
--
--   token_digest — SHA-256 (NOT bcrypt) of the plaintext `skm_…` token. The
--     token is a random 256-bit secret, so a slow password hash buys nothing
--     and would burn CPU on the ~2s poll loop (spec D4). The column is UNIQUE:
--     the digest is the lookup key on every /agent/v1 request, and the unique
--     constraint backs that lookup with an index (no separate index needed).
--
--   minted_by_api_key_id — FK → api_keys ON DELETE CASCADE. This is the
--     cascade revocation hook (spec R11): when the minting ska_ key is hard-
--     deleted the credential goes with it. The atomic validate-and-renew SQL
--     additionally joins api_keys (revoked_at IS NULL) + users (deactivated_at
--     IS NULL) so a SOFT-revoked key or a deactivated user also kills the
--     token without a row delete. user_id/project_id are plain RESTRICT FKs —
--     users are soft-deactivated (never hard-deleted on this path) and a
--     project with live monitors should not vanish underneath them.
--
--   scope_issue_id text NOT NULL — v1 monitors always target an issue (and
--     optionally its subtree via scope_subtree). project-wide scope is
--     deferred: a NULL here would never match the poll predicate, so we keep
--     it NOT NULL rather than ship a column that can only mean "matches
--     nothing" (spec §5 / S1).
--
--   event_types text[] NOT NULL — the notification types this monitor
--     delivers, constrained at mint time (T7) to the set the poll endpoint
--     can normalise (comment.created + issue.*). NOT NULL so the poll filter
--     never has to special-case a missing array.
--
--   absolute_deadline / expires_at — lease-style (spec D5): expires_at
--     slides forward (created + 10m, renewed ≤10m on each use) but is capped
--     at absolute_deadline (created + 12h, immutable) so a forgotten monitor
--     cannot live forever. Both stamped from the DB clock to avoid app/DB
--     skew.

CREATE TABLE monitor_credentials (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_digest         bytea NOT NULL UNIQUE,
    user_id              uuid NOT NULL REFERENCES users(id),
    minted_by_api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    audience             text NOT NULL,
    capability           text NOT NULL,
    project_id           uuid NOT NULL REFERENCES projects(id),
    scope_issue_id       text NOT NULL,
    scope_subtree        boolean NOT NULL,
    event_types          text[] NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    absolute_deadline    timestamptz NOT NULL,
    expires_at           timestamptz NOT NULL,
    last_used_at         timestamptz,
    revoked_at           timestamptz
);

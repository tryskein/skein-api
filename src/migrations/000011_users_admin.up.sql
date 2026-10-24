-- 000011_users_admin — R3.1
--
-- Adds soft-deactivation column to the users table. When deactivated_at
-- IS NOT NULL the user cannot log in (auth handler rejects with the
-- generic invalid_credentials code so the deactivation is not leaked
-- through the response surface) and existing API keys are soft-revoked
-- in the same transaction by users.Store.Deactivate.
--
-- Per R3.1 plan Q-8: NO BACKFILL. This is a dev environment migration —
-- existing rows get the default NULL (active). No business action is
-- required because deactivation is opt-in via the admin REST/MCP path.

ALTER TABLE users
    ADD COLUMN deactivated_at timestamptz NULL;

-- Partial index covering only active users — the common case for login,
-- list operations, and last-admin protection counts. Deactivated users
-- are queried rarely (admin reactivation flow only) and don't need the
-- index entry.
CREATE INDEX idx_users_active
    ON users (id)
    WHERE deactivated_at IS NULL;

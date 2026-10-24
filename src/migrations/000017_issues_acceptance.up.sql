-- 000017 — Per-assignee acceptance flag on issues.
--
-- Adds two columns that record when (and by whom) the current assignee
-- explicitly accepted the task. R-future Phase 2 will introduce a
-- `skein_accept` MCP tool + `issue.accepted` event for the positive
-- transition; Phase 1 lays the schema + the auto-accept-on-self-assign
-- and reset-on-reassign mechanics in the issues store.
--
-- Semantics:
--   - accepted_at NULL  → assignee has not yet acknowledged the task
--                         (the Inbox surfaces it as "needs acceptance").
--   - accepted_at !NULL → current assignee has accepted; updated_at /
--                         version are NOT bumped on acceptance (no
--                         event-row, no audit churn — Phase 2 adds the
--                         dedicated event).
--
-- accepted_by FK → users(id) ON DELETE SET NULL: when a user is hard-
-- deleted we keep the timestamp (the row was accepted at some point) but
-- drop the dangling reference. ON DELETE SET NULL matches the existing
-- pattern on issues.created_by-equivalents.
--
-- Index: partial on (assignee, accepted_at) WHERE accepted_at IS NULL
-- AND status = 'open'. The Inbox query is "show me open issues
-- assigned to me that I have not yet accepted" — the partial keeps the
-- index tiny (excludes the closed + accepted majority) and supports a
-- range-scan on accepted_at IS NULL by using a NULL-first ordering.
--
-- Lock cost: ADD COLUMN with no default + nullable is metadata-only in
-- PostgreSQL 11+ (no rewrite, sub-second on any size). Index build:
-- write lock for the duration; sub-second on current DB size.
--
-- No backfill: existing issues are treated as "not yet accepted" — the
-- Inbox surfaces them on first session, the assignee acks via the new
-- flow.
ALTER TABLE issues
    ADD COLUMN accepted_at timestamptz,
    ADD COLUMN accepted_by uuid REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX idx_issues_unaccepted ON issues (assignee, accepted_at)
    WHERE accepted_at IS NULL AND status = 'open';

-- sk-118 (F2a): introduce the `standing` issue status as a valid stored
-- value. A standing issue is a long-lived "watch / recurring" task that
-- lives outside the open→in_progress→closed work lifecycle — it is not
-- counted as active work, never gates readiness, and cannot be claimed
-- (those behaviors land in later slices F2b–F2i; this migration ONLY
-- widens the CHECK constraint so the value can be stored).
--
-- The constraint name comes from the issues init migration
-- (000004_issues.up.sql) — Postgres auto-names it `issues_status_check`.
-- Mirror the exact DROP + ADD shape used by 000019_drop_blocked_status.
ALTER TABLE issues
    DROP CONSTRAINT IF EXISTS issues_status_check;

ALTER TABLE issues
    ADD CONSTRAINT issues_status_check
    CHECK (status IN ('open', 'in_progress', 'deferred', 'closed', 'standing'));

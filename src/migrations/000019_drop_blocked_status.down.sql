-- sk-31 down: Pure revert is impossible because the up-migration
-- destroyed the per-issue blocked-state signal (every blocked row was
-- flipped to 'open' and we don't know which "open" rows used to be
-- "blocked" without parsing the events table back to front, which
-- isn't safe for a migration to do).
--
-- The audit context survives as a system comment per blocked issue
-- (see the INSERT in the up migration); ops can manually restore
-- specific issues to 'blocked' if needed, but that's a per-issue
-- decision, not a wholesale reverse migration.
--
-- This file exists only so `migrate down 1` is a valid no-op and the
-- migration history table stays consistent.

-- Restore the CHECK constraint so manual SQL `UPDATE ... status =
-- 'blocked'` would work again. The data-side restore is not provided.
ALTER TABLE issues
    DROP CONSTRAINT IF EXISTS issues_status_check;

ALTER TABLE issues
    ADD CONSTRAINT issues_status_check
    CHECK (status IN ('open', 'in_progress', 'blocked', 'deferred', 'closed'));

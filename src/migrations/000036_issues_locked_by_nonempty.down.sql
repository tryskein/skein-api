-- 000036_issues_locked_by_nonempty (down) — drop the locked_by non-empty
-- invariant. The column reverts to accepting '' (which no code writes).

ALTER TABLE issues
    DROP CONSTRAINT IF EXISTS issues_locked_by_nonempty;

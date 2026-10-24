-- 000017 down — drop accepted_at / accepted_by + index.
--
-- Reverse order matters: drop the partial index first (it depends on
-- accepted_at), then the columns. IF EXISTS keeps the down idempotent
-- so a re-run after a partial failure doesn't error.
DROP INDEX IF EXISTS idx_issues_unaccepted;
ALTER TABLE issues DROP COLUMN IF EXISTS accepted_by;
ALTER TABLE issues DROP COLUMN IF EXISTS accepted_at;

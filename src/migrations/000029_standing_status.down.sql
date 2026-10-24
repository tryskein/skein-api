-- sk-118 (F2a) down: remove `standing` from the valid status set.
--
-- DATA-GUARDED (unlike 000019's near-no-op down): the up-migration made
-- 'standing' storable, so by the time we roll back there may be live
-- issues in status='standing'. Re-adding the 4-value CHECK while such
-- rows exist would abort the migration. We therefore FIRST demote every
-- standing row back to 'open' (the safe lifecycle entry point — a
-- standing task that loses its status becomes an ordinary open task),
-- THEN re-add the constraint without the 'standing' value.
UPDATE issues
   SET status = 'open',
       updated_at = now()
 WHERE status = 'standing';

ALTER TABLE issues
    DROP CONSTRAINT IF EXISTS issues_status_check;

ALTER TABLE issues
    ADD CONSTRAINT issues_status_check
    CHECK (status IN ('open', 'in_progress', 'deferred', 'closed'));

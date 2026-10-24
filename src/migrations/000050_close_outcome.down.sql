-- Rollback of 000050_close_outcome — sk-489.
--
-- ⚠ THE ROLLBACK LOSES OUTCOMES, and this is the only thing the operator must know before
-- running it. DROP COLUMN deletes data silently and with a success status: the outcomes
-- of ALL closed issues go away — both those seeded by the apply and those chosen by people after
-- it. There is nothing to restore them from: the trace of the intent "will not do" / "duplicate" /
-- "superseded" existed only in this column, while the free text of the close
-- reason is not machine-parsed (which is exactly why the column appeared).
--
-- ⚠ WHAT A RE-APPLY WILL DO. It will seed ALL closed issues with the outcome
-- "done" — including those that were closed without work before the rollback. That is,
-- a rollback followed by an apply does not return the state, it REPLACES it with the
-- factory one: an epic that honestly showed "100 %, without work: 1" will, after the cycle,
-- show the old untruth — four of four done. This is a deliberate price
-- of an additive migration, not an oversight: there is nowhere to put what was removed, and holding
-- the rollback for a manual export is the operator's decision, not the migration's.
--
-- To save the outcomes before the rollback, if the decision is already made:
--
--   CREATE TABLE close_outcome_backup AS
--       SELECT id, close_outcome FROM issues WHERE close_outcome IS NOT NULL;
--
-- The rollback does not depend on this and does not create a table: a silently left copy
-- of data is a surprise for the next operator, not a help.
--
-- ⚠ WHAT THE ROLLBACK DOES NOT BREAK. It is unconditional: there is nothing to fail here — only
-- the added objects are removed, no earlier constraint comes back in
-- a narrowed form (unlike 000044, whose rollback fails on the first row with
-- a new outcome). Old code on the rolled-back schema works as it did: before
-- 000050 it knew nothing about the outcome.
--
-- The order is the reverse of the apply: first the dependent (the trigger depends on the function),
-- then the constraint, then the column. IF EXISTS keeps the rollback idempotent
-- if a partial apply left only some of the objects. The CHECK on the value set
-- and the comments go away with the column, no separate DROP is needed for them.

DELETE FROM migration_counters WHERE migration = '000050_close_outcome';

DROP TRIGGER IF EXISTS issues_normalize_close_outcome ON issues;

DROP FUNCTION IF EXISTS skein_normalize_close_outcome();

ALTER TABLE issues
    DROP CONSTRAINT IF EXISTS issues_close_outcome_consistent_chk;

ALTER TABLE issues DROP COLUMN IF EXISTS close_outcome;

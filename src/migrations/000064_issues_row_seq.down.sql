-- Rollback of 000064_issues_row_seq.
--
-- Fully reversible for the schema: the trigger, the function and the column are dropped;
-- the sequence issues_row_seq_seq belongs to the column (OWNED BY) and
-- goes away with it — DROP SEQUENCE IF EXISTS below only insures the case
-- when up was interrupted between creating the sequence and binding it.
-- The row_seq values are lost: a repeated up again gives all existing
-- rows 0 ("order unknown") and starts the sequence anew from 1: an open phase 2 tab that stores values of the previous
-- sequence will consider the new rows stale until reloaded. So
-- down is applied only together with the rollback of phase 2 on the frontend.

DROP TRIGGER IF EXISTS issues_stamp_row_seq ON issues;
DROP FUNCTION IF EXISTS skein_issues_stamp_row_seq();
ALTER TABLE issues DROP COLUMN IF EXISTS row_seq;
DROP SEQUENCE IF EXISTS issues_row_seq_seq;

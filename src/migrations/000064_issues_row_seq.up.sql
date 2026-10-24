-- 000064_issues_row_seq — sk-622.21, T026 (phase 2; US-15, K16).
--
-- WHY. Phase 2 puts the issue's list row into the issue event, and the tab
-- substitutes the row only if it is fresher than the one shown. "Fresher" cannot be
-- judged by version (adding and removing a label, replacing components do not
-- move it — AddLabelNoLoad / RemoveLabelNoLoad), nor by updated_at (it is not
-- unique, two changes within one microsecond are indistinguishable). An order is needed
-- that distinguishes EVERY committed change of the issue row.
--
-- MECHANISM (K16).
--   * issues_row_seq_seq — one global bigint sequence.
--   * issues.row_seq — the value of the sequence after the last
--     committed write of the issue row.
--   * issues_stamp_row_seq — BEFORE INSERT OR UPDATE ON issues FOR EACH ROW:
--     any write of the row takes the next value. The column is written ONLY by the
--     trigger: a value passed by the writer is overwritten.
--
-- WHY THE row_seq ORDER OF ONE ISSUE = THE ORDER OF ITS COMMITS. A row-level
-- BEFORE trigger on UPDATE fires after the executor has taken the
-- row lock (under READ COMMITTED — after waiting for the concurrent writer and
-- re-checking the row), and the lock is held until the end of the transaction. So
-- of two transactions writing one issue, the second takes nextval only after the
-- first commits and gets the larger value. The value is issued on the WRITE,
-- not at the start of the transaction: a SERIALIZABLE transaction that started earlier and
-- lost the race (40001) is retried in full and takes a new value.
-- The order of values of DIFFERENT issues means nothing.
--
-- Writes outside the issue row that change list-row fields must move it
-- by an UPDATE of issues — this is how all label and component paths are built
-- (store.go#AddLabelNoLoad, #RemoveLabelNoLoad, #ReplaceComponentsNoLoad
-- write updated_at). The recomputation of the assignee by the users directory (the
-- 000060 triggers) also writes an issues row and moves the order — assigneeUserId is part of
-- the row. Extra movement (a write that does not change list-row fields, e.g.
-- an embedding) is harmless: the same state will be substituted. The known remainder
-- (R26 item 4): renaming the author changes the row's createdByUsername (JOIN
-- users) without writing issues and does not move the order.
--
-- ROLLOUT WITHOUT REWRITING THE TABLE (decision R26 item 1). The column is added with a
-- CONSTANT default: ADD COLUMN … NOT NULL DEFAULT 0. A non-volatile
-- default PostgreSQL records in metadata (pg_attribute.attmissingval)
-- and gives to existing rows on read — neither the table nor its indexes
-- (including HNSW on embedding, 000007) are rewritten; ACCESS EXCLUSIVE
-- is held only for the duration of the catalog edit. A volatile default
-- (DEFAULT nextval()) would rewrite the table and all indexes under the same lock
-- until COMMIT — and migrations are applied at API startup. The source is
-- PostgreSQL 18, ALTER TABLE, Notes:
-- https://www.postgresql.org/docs/18/sql-altertable.html
-- («a non-volatile DEFAULT … is stored in the table's metadata … a rewrite
-- of the table is not required»; «a volatile DEFAULT … will cause the entire
-- table and its indexes to be rewritten»).
--
-- Right after that the default is dropped (DROP DEFAULT): the value in
-- metadata stays for already existing rows, while a new row gets
-- its order only from the trigger. If the trigger is ever disabled
-- (session_replication_role = replica), an INSERT that does not know the column will be
-- rejected by NOT NULL instead of writing a silent zero to the new row.
--
-- NO SEED. Rows not written after the migration stay with row_seq = 0.
-- The meaning of zero (contract T009, issues.IssueSummary.RowSeq): "the row has not
-- changed since the phase 2 rollout, order unknown". This is a correct minimum:
-- any write of the row after the migration goes through the trigger and gets a
-- positive value, and phase 2 reads the event row from the current
-- state — so an event with a positive rowSeq is always fresher than the
-- shown zero, and an event older than a REST row with zero cannot exist.
-- An event with zero carries the same state as the shown row with
-- zero. The rejected path "nullable + seed in batches + NOT VALID/VALIDATE"
-- (R26 item 1) does not fit into a golang-migrate transactional migration and gives
-- the same correctness at a higher cost.
--
-- NOT NULL — because readers scan the column into int64 without a NULL branch:
-- the absence of order is expressed by zero, not NULL.
--
-- NO INDEX, ON PURPOSE: nobody searches or sorts by row_seq — it is
-- read together with the issue row already found by id or by list.
--
-- search_path of the function is pinned (pg_catalog, public) — the same security default
-- as in the 000044/000050/000060 triggers.
--
-- Rollback — down: trigger, function, column; the sequence belongs to
-- the column (OWNED BY) and goes away with it.

CREATE SEQUENCE issues_row_seq_seq AS bigint;

ALTER TABLE issues
    ADD COLUMN row_seq bigint NOT NULL DEFAULT 0;

ALTER SEQUENCE issues_row_seq_seq OWNED BY issues.row_seq;

ALTER TABLE issues
    ALTER COLUMN row_seq DROP DEFAULT;

COMMENT ON COLUMN issues.row_seq IS
    'Issue row freshness order (sk-622.21, US-15, K16): the value '
    'of the sequence issues_row_seq_seq after the last committed '
    'write of the row. Written only by the trigger issues_stamp_row_seq on every '
    'INSERT and UPDATE; for one issue a larger row_seq is a later '
    'commit. Values of different issues are not comparable. 0 — the row has not changed since the '
    'phase 2 rollout (migration 000064), order unknown. Given out as '
    'rowSeq in REST rows, the card and the phase 2 event row.';

CREATE FUNCTION skein_issues_stamp_row_seq() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    NEW.row_seq := nextval('issues_row_seq_seq');
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION skein_issues_stamp_row_seq() IS
    'Assigns the issue row the next value of issues_row_seq_seq on every '
    'write (sk-622.21, K16). Fires after the row is locked, so '
    'the row_seq order of one issue matches the order of its commits.';

CREATE TRIGGER issues_stamp_row_seq
    BEFORE INSERT OR UPDATE ON issues
    FOR EACH ROW
    EXECUTE FUNCTION skein_issues_stamp_row_seq();

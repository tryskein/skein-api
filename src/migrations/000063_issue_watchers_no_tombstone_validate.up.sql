-- 000063_issue_watchers_no_tombstone_validate — sk-622.21, T025 (rollout R2;
-- INV-1). Found in review.
--
-- WHY A SEPARATE MIGRATION. 000062 adds the constraint
-- issue_watchers_no_tombstone as NOT VALID. Checking the existing rows is
-- a full scan of issue_watchers. Inside the 000062 transaction it would run under the already
-- taken ACCESS EXCLUSIVE (ADD CONSTRAINT), which is held until COMMIT, —
-- and for the whole duration of the scan it would stop both reads and writes of subscriptions
-- (the mailing of every event reads issue_watchers). golang-migrate runs
-- each file as one transaction, so VALIDATE is moved here: in its own
-- transaction VALIDATE CONSTRAINT takes only SHARE UPDATE EXCLUSIVE —
-- reads and insert/update/delete of rows go on in parallel with the scan; only
-- other DDL and VACUUM of this table wait.
--
-- There can be no violators: 000062, in the same transaction as ADD CONSTRAINT,
-- turned the remaining tombstones into opt-outs, and new rows with 'unwatched'
-- are rejected by the constraint since its appearance. The scan confirms this and
-- moves the constraint to validated (pg_constraint.convalidated).
--
-- Rollback — down: no-op (see there).

ALTER TABLE issue_watchers
    VALIDATE CONSTRAINT issue_watchers_no_tombstone;

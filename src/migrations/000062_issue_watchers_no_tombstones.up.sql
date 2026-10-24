-- 000062_issue_watchers_no_tombstones — sk-622.21, T025 (rollout R2; INV-1,
-- D9, K3, K20).
--
-- WHY. On an explicit unsubscribe the previous skein-api version left in
-- issue_watchers a "tombstone" — a row whose reasons contain 'unwatched'.
-- R1 (000059 and the writers of the new version) replaced it with an opt-out
-- (issue_watch_optouts), and the R1 reconciliation turned tombstones written by the old
-- version during the rollout into opt-outs. R2 rolls out only after
-- the reconciliation showed zero divergences (gate T021), and removes both the reconciliation and
-- the compatibility in code: INV-1 "the subscription table has no rows that are not
-- a live subscription" is pinned by a constraint.
--
-- STEPS (one migration transaction):
--   1. The remainder of tombstones, if the gate missed them after all, is turned into what
--      the new version would have recorded: an opt-out (issue_id, user_id), deletion of the row
--      and nulling of the Watching card of this issue for all notifications of
--      the user (K6, INV-6) — the same step (a) of the R1 reconciliation. In steady
--      state there are no rows, and the step does nothing.
--   2. CHECK (NOT 'unwatched' = ANY(reasons)) NOT VALID: the constraint
--      applies to new rows immediately. ADD CONSTRAINT takes ACCESS
--      EXCLUSIVE on issue_watchers until the end of this migration transaction
--      (golang-migrate runs a file as one transaction), but does not scan the existing
--      rows here — the lock is short. Checking the
--      existing rows (VALIDATE CONSTRAINT) is moved to 000063: in its own
--      transaction it takes only SHARE UPDATE EXCLUSIVE and does not stop
--      reads and writes of subscriptions for the duration of the scan (found in review).
--   3. The R1 partial reconciliation indexes (000061) are dropped together with its worker:
--      idx_issue_watchers_tombstone — the predicate is now empty by the constraint;
--      idx_notif_card_unresolved — completing cards was a reconciliation step, the new
--      version sets card_resolved = true on all rows itself.
--
-- Rollback R2 → R1 — down (after down 000063): the constraint is dropped, the 000061 indexes
-- are restored; down does not touch data.
--
-- project_watchers is not affected: there 'unwatched' is the live mechanism
-- of unsubscribing from a project.

WITH dead AS (
    DELETE FROM issue_watchers
     WHERE 'unwatched' = ANY(reasons)
    RETURNING issue_id, user_id
),
opt AS (
    INSERT INTO issue_watch_optouts (issue_id, user_id)
    SELECT issue_id, user_id FROM dead
    ON CONFLICT (issue_id, user_id) DO NOTHING
)
UPDATE notifications n
   SET card_issue_id = NULL
  FROM dead d
 WHERE n.user_id = d.user_id
   AND n.card_issue_id = d.issue_id;

ALTER TABLE issue_watchers
    ADD CONSTRAINT issue_watchers_no_tombstone
    CHECK (NOT 'unwatched' = ANY(reasons)) NOT VALID;

DROP INDEX IF EXISTS idx_issue_watchers_tombstone;
DROP INDEX IF EXISTS idx_notif_card_unresolved;

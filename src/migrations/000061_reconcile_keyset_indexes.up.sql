-- 000061_reconcile_keyset_indexes — sk-622.21, T020 (D9, K20), review of wave
-- skein-api/4, fable MAJOR-2.
--
-- WHY. The R1 reconciliation (internal/notifications/reconcile.go) every hour and at
-- the start of each replica looks for candidates of two steps by predicates
-- for which the schema had no index:
--
--   * step (a) — tombstones of the old version: issue_watchers WHERE 'unwatched' =
--     ANY(reasons);
--   * step (c) — rows with no card decision: notifications WHERE NOT
--     card_resolved.
--
-- Without an index every batch read and sorted the whole table (Seq Scan →
-- Sort → Limit), i.e. the card backfill is quadratic, and a SERIALIZABLE batch
-- held a predicate SIRead lock on the whole relation — against live
-- mailing writers.
--
-- Both indexes are partial on exactly these predicates and ordered by the keyset
-- walk key of the step (the same order as ORDER BY in the queries):
-- notifications (id), issue_watchers (issue_id, user_id). In steady
-- state (after the R1 rollout there is nothing to fix) both are almost empty, and a pass costs
-- a constant; writes to the tables pay for the index only on rows that fall into the
-- predicate (tombstones are written only by the old version, card_resolved = false — only
-- rows from before R1 and the old version).
--
-- Lifetime — R1: in R2 (T025) they are dropped together with the reconciliation worker.
--
-- A plain CREATE INDEX inside the migration transaction, not CONCURRENTLY
-- (project convention, see 000032).

CREATE INDEX idx_notif_card_unresolved
    ON notifications (id)
    WHERE NOT card_resolved;

CREATE INDEX idx_issue_watchers_tombstone
    ON issue_watchers (issue_id, user_id)
    WHERE 'unwatched' = ANY(reasons);

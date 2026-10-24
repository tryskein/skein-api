-- Rollback of 000061_reconcile_keyset_indexes.
--
-- Fully reversible: only the two partial R1 reconciliation indexes are dropped;
-- data is not affected. The reconciliation code works without them too (slower — Seq
-- Scan over the tables, which is what the indexes were introduced for).

DROP INDEX IF EXISTS idx_issue_watchers_tombstone;
DROP INDEX IF EXISTS idx_notif_card_unresolved;

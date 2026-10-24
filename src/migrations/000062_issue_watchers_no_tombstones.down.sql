-- Rollback of 000062_issue_watchers_no_tombstones (R2 → R1).
--
-- Removes the "no tombstones" constraint and restores the R1 partial reconciliation
-- indexes (000061) — what the R1 version and its reconciliation worker need. Does not touch
-- data: tombstones turned into opt-outs by the up migration stay
-- opt-outs — the R1 version would have recorded them that way too.

ALTER TABLE issue_watchers
    DROP CONSTRAINT IF EXISTS issue_watchers_no_tombstone;

CREATE INDEX IF NOT EXISTS idx_notif_card_unresolved
    ON notifications (id)
    WHERE NOT card_resolved;

CREATE INDEX IF NOT EXISTS idx_issue_watchers_tombstone
    ON issue_watchers (issue_id, user_id)
    WHERE 'unwatched' = ANY(reasons);

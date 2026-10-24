-- 000026_reminders — sk-62.2 "Reminders, phase 1".
--
-- A reminder is a per-user alarm hung on an issue. State is DERIVED from
-- remind_at, never stored as a status column:
--
--   armed   — remind_at  > now()   (waiting to ring)
--   ringing — remind_at <= now()   (due; the scheduler has pinged, or
--             is about to ping, the owner)
--
-- The row's mere existence is the "active reminder" signal — dismissing a
-- reminder DELETEs the row, it is not a soft-delete. Multiple reminders
-- per (issue, user) are allowed (no UNIQUE), so a user can stack several
-- nudges on one task.
--
-- notified_at is the scheduler's idempotency stamp: it records the last
-- time the scheduler materialised a bell notification for the CURRENT
-- remind_at. A reminder is "due and un-notified" when
--   remind_at <= now() AND (notified_at IS NULL OR notified_at < remind_at)
-- Snooze resets notified_at to NULL (and pushes remind_at forward), so the
-- next ring re-notifies. This lets the scheduler skip rows it already rang
-- without a second status table.
--
-- issue_id is varchar(1000) to match issues.id (the hierarchical text key,
-- e.g. "sk-62"), NOT a uuid — issues.id is a varchar PK (000004_issues).
-- ON DELETE CASCADE: when an issue is hard-deleted its reminders go too.
-- user_id ON DELETE CASCADE: deactivating/removing a user drops their
-- reminders (they're a personal lens, no audit value once the user is
-- gone).

CREATE TABLE reminders (
    id              uuid PRIMARY KEY,
    issue_id        varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    remind_at       timestamptz NOT NULL,
    notified_at     timestamptz,
    snooze_count    int NOT NULL DEFAULT 0 CHECK (snooze_count >= 0),
    last_reason     text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Drives the per-user lens + badge queries (ListForUser / CountRingingTasks):
--   WHERE user_id = $1 ORDER BY remind_at
CREATE INDEX idx_reminders_user ON reminders (user_id, remind_at);

-- FK index on issue_id so the issues ON DELETE CASCADE doesn't seq-scan
-- reminders, and so the per-issue "active reminder for caller" lookup
-- (snooze/dismiss targeting by issueId) is cheap.
CREATE INDEX idx_reminders_issue ON reminders (issue_id);

-- Partial scheduler index: the claim query is
--   WHERE remind_at <= now()
--     AND (notified_at IS NULL OR notified_at < remind_at)
-- now() is not immutable, so it can't live in the predicate. We instead
-- index remind_at over the un-notified subset (notified_at IS NULL) — the
-- common steady-state case (every freshly-armed/snoozed reminder has
-- notified_at NULL). The notified_at < remind_at re-ring case (a row that
-- rang for an earlier remind_at then got snoozed) is rare and falls back to
-- the user index; the partial index keeps the hot path a tight range scan
-- bounded by the count of currently-pending reminders.
CREATE INDEX idx_reminders_due ON reminders (remind_at) WHERE notified_at IS NULL;

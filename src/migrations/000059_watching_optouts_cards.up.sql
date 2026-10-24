-- 000059_watching_optouts_cards — sk-622.21, T001 (rollout R1).
--
-- WHY. Watching moves to the model "the subscription table holds only live
-- subscriptions" (US-1, INV-1, D2) and to a card computed when the
-- notification is written (US-5, K4–K6). This migration is only an EXPANSION of the R1 schema:
--
--   1. issue_watch_optouts — opt-outs "user U does not want to be automatically
--      subscribed to issue X". Only auto-subscription reads them; it starts
--      empty (tombstones are not carried over into opt-outs — owner decision, K3).
--   2. notifications.card_issue_id — the Watching card the
--      row belongs to (K4), and notifications.card_resolved — "the card was computed by the new
--      version" (K6, for the T020 reconciliation).
--   3. Partial indexes for the badge, the Watching column, the card FK action and
--      retention cleanup (K5).
--   4. user_notification_prefs.unwatch_on_close: default true, all
--      existing rows → true (US-2, sk-622.19).
--   5. A one-time deletion of all tombstones — issue_watchers rows with the reason
--      'unwatched' (K3).
--
-- OLD VERSION ON TOP OF THE NEW SCHEMA (R1 rollout). Nothing that the old code writes
-- or reads is removed or narrowed: the new columns are
-- nullable / with a constant DEFAULT, the new table is invisible to the old code,
-- the reasons column and the 'unwatched' value in it stay valid (the ban
-- is only in R2, in a separate migration). A tombstone written by the old version
-- during the rollout is turned into an opt-out by the T020 reconciliation.
--
-- INDEXES ON notifications — a plain CREATE INDEX, not CONCURRENTLY: migrations
-- are applied at startup inside a transaction, where CONCURRENTLY is not allowed
-- (project convention, see 000032_notifications_poll_index).

-- 1. Opt-outs. PK (issue_id, user_id) — uniqueness of the pair and the entry point of auto-subscription
-- by issue; a separate index (user_id) — for the cascade when a
-- user is deleted.
CREATE TABLE issue_watch_optouts (
    issue_id   varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, user_id)
);
CREATE INDEX idx_issue_watch_optouts_user ON issue_watch_optouts (user_id);

COMMENT ON TABLE issue_watch_optouts IS
    'Opt-out: the user explicitly unsubscribed from an issue they were subscribed to, '
    'and does not want to be automatically subscribed to it again (sk-622.21, US-1, D2). '
    'Read only by auto-subscription (creation, assignment, reopen); the feed, '
    'badge, mailing and card do not read it. A manual subscription deletes the opt-out. '
    'Deleting the issue or the user deletes the opt-out.';
COMMENT ON COLUMN issue_watch_optouts.created_at IS
    'When the opt-out was recorded (UTC).';

-- 2. The Watching card. FK ON DELETE SET NULL (K4): deleting an issue takes
-- the notification out of Watching, but the Inbox/history feed row stays.
ALTER TABLE notifications
    ADD COLUMN card_issue_id varchar(1000) NULL REFERENCES issues(id) ON DELETE SET NULL,
    ADD COLUMN card_resolved boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN notifications.card_issue_id IS
    'The Watching card of this row: the issue for the sake of whose subscription the row '
    'got into Watching (the event issue itself or its ancestor with a subtree '
    'subscription), computed at write time (sk-622.21, US-5, K4). NULL — the row is not '
    'in Watching (a mention, not by subscription, the card was nulled by an unsubscribe or '
    '"dismiss", the card issue was deleted).';
COMMENT ON COLUMN notifications.card_resolved IS
    'true — the card of this row was computed by the new version (mailing, mentions, '
    'reminders, digests set true on every row they write; nulling '
    'the card does not touch the flag). false — the row was written before R1 or by the old '
    'version during the rollout; its card is completed by the T020 reconciliation (K6).';

-- 3. Indexes K5.
-- The Watching badge: unread rows with a card in a created_at window
-- within the user.
CREATE INDEX idx_notif_watching_badge
    ON notifications (user_id, created_at)
    WHERE card_issue_id IS NOT NULL AND read_at IS NULL AND archived_at IS NULL;
-- The Watching column, nulling the user's cards by X, "mark the card
-- as read".
CREATE INDEX idx_notif_watching_card
    ON notifications (user_id, card_issue_id, created_at DESC)
    WHERE card_issue_id IS NOT NULL AND archived_at IS NULL;
-- The card FK action on issue deletion (ON DELETE SET NULL).
CREATE INDEX idx_notif_card_issue
    ON notifications (card_issue_id)
    WHERE card_issue_id IS NOT NULL;
-- Retention cleanup: cleanup never deletes mentions (US-8),
-- so they are outside the index.
CREATE INDEX idx_notif_retention
    ON notifications (created_at)
    WHERE type NOT IN ('comment.mention', 'issue.mention');

-- 4. Auto-unwatch of closed issues — on for everyone (US-2, sk-622.19).
ALTER TABLE user_notification_prefs
    ALTER COLUMN unwatch_on_close SET DEFAULT true;
UPDATE user_notification_prefs
   SET unwatch_on_close = true
 WHERE NOT unwatch_on_close;

COMMENT ON COLUMN user_notification_prefs.unwatch_on_close IS
    'true — on issue close, remove the person''s automatic subscriptions '
    '(reasons creator and assignee) without recording an opt-out; the manual one stays. Not sticky: '
    'on reopen the subscription comes back if there is no opt-out. Default true, '
    'no row = true (sk-622.21, US-2; was false — sk-551.10).';

-- 5. Tombstones (K3): a subscription row with the reason 'unwatched' is not a subscription.
-- All are deleted; they are not carried over into opt-outs (owner decision).
DELETE FROM issue_watchers
 WHERE 'unwatched' = ANY(reasons);

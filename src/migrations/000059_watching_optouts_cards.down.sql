-- Rollback of 000059_watching_optouts_cards.
--
-- The schema is fully reversible: the opt-out table, the card columns, the K5 indexes and
-- the unwatch_on_close default return to the 000058 form.
--
-- ⚠ DATA LOSS — accepted by the owner (sk-622.21, §Data, Known residuals
-- of the plan):
--   * the issue_watchers tombstones ('unwatched') deleted by the up migration are NOT
--     restored — they no longer exist anywhere;
--   * recorded opt-outs and computed cards are deleted together with the table and
--     the columns; the previous version does not read them;
--   * the unwatch_on_close values that up changed from false to true are NOT
--     returned to false — which of them were false was not saved. The rollback
--     changes only the default for new rows.

ALTER TABLE user_notification_prefs
    ALTER COLUMN unwatch_on_close SET DEFAULT false;

COMMENT ON COLUMN user_notification_prefs.unwatch_on_close IS
    'true — on issue close, remove the person''s automatic subscriptions '
    '(reasons creator and assignee); the manual one stays. Not sticky: on reopen '
    'the subscription comes back. Default false (sk-551.10).';

DROP INDEX IF EXISTS idx_notif_retention;
DROP INDEX IF EXISTS idx_notif_card_issue;
DROP INDEX IF EXISTS idx_notif_watching_card;
DROP INDEX IF EXISTS idx_notif_watching_badge;

ALTER TABLE notifications
    DROP COLUMN IF EXISTS card_resolved,
    DROP COLUMN IF EXISTS card_issue_id;

DROP TABLE IF EXISTS issue_watch_optouts;

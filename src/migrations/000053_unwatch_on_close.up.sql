-- 000053_unwatch_on_close — sk-551.10, item 5.
--
-- WHY. A subscription held for the author or assignee accumulates forever: the issue is closed and
-- the person keeps "watching" it, and the "Watching" list fills up with
-- closed issues. None of the products studied removes it on close by default;
-- in YouTrack it is an option. The decision is the same: a PERSONAL setting, OFF by default.
--
-- WHAT THE SETTING DOES (application layer, not schema): when an issue is closed, for
-- a person with the setting on, the automatic subscription reasons
-- `creator` and `assignee` are removed; the manual one (`manual`) stays. This is NOT a sticky unsubscribe:
-- if the issue is reopened, the automatic subscription comes back by the same rule
-- that sets it on creation and assignment.
--
-- ⚠ WHY NOT NULL DEFAULT false, and not NULL = "not set". Rows of
-- user_notification_prefs are created lazily, and for a person without a row
-- the setting is read through COALESCE(..., false) — the same value that
-- DEFAULT gives. The flag has no third state, and adding one would mean
-- inventing an "I don't know" that has nothing to mean.
ALTER TABLE user_notification_prefs
    ADD COLUMN unwatch_on_close boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN user_notification_prefs.unwatch_on_close IS
    'true — on issue close, remove the person''s automatic subscriptions '
    '(reasons creator and assignee); the manual one stays. Not sticky: on reopen '
    'the subscription comes back. Default false (sk-551.10).';

-- Reverts 000009: drop the per-user agent suppression column.
ALTER TABLE user_notification_prefs
    DROP COLUMN suppress_own_agent_actions;

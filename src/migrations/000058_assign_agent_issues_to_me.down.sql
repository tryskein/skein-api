-- Rollback of 000058_assign_agent_issues_to_me.
--
-- Only the column is removed; the user_notification_prefs rows and their other
-- columns are not touched.
--
-- ⚠ DATA LOSS: dropping the column erases the choice of people who turned the
-- setting off. A repeated up will return the factory true to everyone. Issues already
-- assigned by default are not touched by the rollback — the assignee is recorded in them
-- as an ordinary one.
ALTER TABLE user_notification_prefs
    DROP COLUMN IF EXISTS assign_agent_issues_to_me;

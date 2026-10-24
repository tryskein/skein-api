-- 000058_assign_agent_issues_to_me — sk-565 (D3, INV-8).
--
-- WHY. An agent that creates an issue without an assignee leaves it nobody's: it does not
-- get into anyone's Inbox, and closing it "without forgetting claim" is the concern of each
-- agent separately. The owner decided: an issue that an agent creates through
-- MCP without an assignee is assigned to the api_key owner by default. This is a
-- PERSONAL setting, ON by default for everyone, including existing
-- users. Stored in the same place as the personal default "needs review" (the same
-- class — "my default on creation").
--
-- WHAT THE SETTING DOES (application layer, not schema): it is read in the MCP
-- creation transaction (`skein_create`, `skein_epic_create`). REST creation does not
-- read it (INV-4).
--
-- ⚠ WHY NOT NULL DEFAULT true, and not NULL = "not set". Rows of
-- user_notification_prefs are created lazily, and for a person without a row the setting is
-- read as true (INV-8) — the same value that DEFAULT gives a row
-- created by someone else's upsert. Existing rows get true by the same
-- DEFAULT: ADD COLUMN with a constant DEFAULT fills them without a separate
-- UPDATE. The flag has no third state.
ALTER TABLE user_notification_prefs
    ADD COLUMN assign_agent_issues_to_me boolean NOT NULL DEFAULT true;

COMMENT ON COLUMN user_notification_prefs.assign_agent_issues_to_me IS
    'true — an issue that the owner''s agent creates through MCP without an assignee '
    'is assigned to the api_key owner (except standing issues and issues "under someone else''s"). '
    'No row = true (INV-8). REST creation does not read the flag. '
    'Default true (sk-565).';

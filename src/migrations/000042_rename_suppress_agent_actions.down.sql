-- Rollback of 000042_rename_suppress_agent_actions.
--
-- Restores the historical column name. Data is untouched: RENAME COLUMN
-- changes only the name, keeping type, NOT NULL and DEFAULT, so the rollback
-- is complete and the up/down pair is idempotent for the up → down → up
-- smoke.
--
-- Rolling back the NAME does not roll back the MEANING: suppressing any
-- agent lives in the fan-out code, and returning that is a return of the
-- application image, not of the migration. Deploy rollback order is therefore
-- the usual one: image first, then schema.
ALTER TABLE user_notification_prefs
    RENAME COLUMN suppress_agent_actions TO suppress_own_agent_actions;

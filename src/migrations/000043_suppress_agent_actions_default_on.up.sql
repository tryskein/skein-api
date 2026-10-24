-- 000043_suppress_agent_actions_default_on — sk-343.
--
-- The "do not notify about agent actions" setting becomes ON by default —
-- for new accounts and existing ones.
--
-- THIS IS A REQUIREMENT CHANGE, NOT A FIX. The previous default (false,
-- migration 000009) was deliberate: the setting silenced the watcher path
-- wholesale, including events addressed to the recipient PERSONALLY BY
-- ROLE, and turning it on for everyone would have silently hidden from the
-- creator the fate of their delegated issue. The first half of sk-343
-- (the `selectFanOutCandidateSQL` change) removed exactly that: suppression
-- no longer applies to the event's personal addressee. Only after that did
-- "quiet by default" stop being dangerous — so the work order was
-- mandatory, and this migration goes SECOND.
--
-- SIDE EFFECT, NAMED TO THE PRODUCT OWNER AND ACCEPTED BY THEM. The column
-- has no third state: "never touched" and "deliberately turned off" are
-- stored as the same false. So the UPDATE below also rewrites those who
-- turned the setting off themselves. A person who had previously turned on
-- a noisy reason (comment, attachment, new issue, reparent, priority,
-- auto-unclaim) will lose the agent-authored part of that reason. People
-- must be warned at deploy — the same way it was done when migration
-- 000039 rewrote reason defaults.
--
-- WHY `SET DEFAULT` ALONE IS NOT ENOUGH, AND WHY `UPDATE` ALONE IS NOT
-- EITHER. `user_notification_prefs` rows are created lazily, and every
-- upsert that creates them names this column explicitly. So the schema
-- DEFAULT almost never fires in practice, and a fresh account has no row
-- at all. What such an account actually sees is decided in two places in
-- code — `COALESCE(..., true)` in the watcher arms of fan-out and the
-- fallback in `Store.GetPrefs` — and both constants change in the same
-- commit. UPDATE covers the third class: accounts that already have a
-- row. None of the three edits replaces another, so they stand together
-- here and in code.
--
-- COUNTERS. The wave spec requires the migration to leave in the journal
-- the number of affected accounts: without that, "did it apply" is
-- checked only by sampling data. We write three numbers — rewritten rows,
-- rows left in the off state (invariant: zero), and accounts with no
-- prefs row at all (their default is held by code, not this table, and
-- knowing their count is useful when triaging complaints after deploy).
ALTER TABLE user_notification_prefs
    ALTER COLUMN suppress_agent_actions SET DEFAULT true;

DO $$
DECLARE
    v_rewritten bigint;
    v_left      bigint;
    v_no_row    bigint;
BEGIN
    UPDATE user_notification_prefs
       SET suppress_agent_actions = true,
           updated_at             = now()
     WHERE suppress_agent_actions = false;
    GET DIAGNOSTICS v_rewritten = ROW_COUNT;

    SELECT count(*) INTO v_left
      FROM user_notification_prefs
     WHERE NOT suppress_agent_actions;

    IF v_left > 0 THEN
        RAISE EXCEPTION
            'migration 000043: % account(s) left with suppress_agent_actions = false; the new default did not apply',
            v_left;
    END IF;

    SELECT count(*) INTO v_no_row
      FROM users u
     WHERE NOT EXISTS (
               SELECT 1 FROM user_notification_prefs p WHERE p.user_id = u.id
           );

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000043_suppress_agent_actions_default_on', 'accounts_rewritten',         v_rewritten),
        ('000043_suppress_agent_actions_default_on', 'accounts_left_off',          v_left),
        ('000043_suppress_agent_actions_default_on', 'accounts_without_prefs_row', v_no_row);

    RAISE LOG 'migration 000043: accounts_rewritten=% accounts_left_off=% accounts_without_prefs_row=%',
        v_rewritten, v_left, v_no_row;
END
$$;

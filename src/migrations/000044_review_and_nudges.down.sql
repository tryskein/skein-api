-- Rollback of 000044_review_and_nudges.
--
-- The up migration only ADDED state — columns, a table, a trigger, and one
-- value in the closed outcome set — so the rollback mechanics are trivial:
-- drop what was added. But "added" and "empty" are different things, and
-- claiming that rollback loses nothing holds only until the first day of
-- production. After that it loses what is listed below, and the operator
-- must read this list BEFORE rolling back, not discover the loss after.
--
-- ⚠ WHAT ROLLBACK DESTROYS IRREVERSIBLY. DROP COLUMN and DROP TABLE delete
-- data silently, with a successful completion; there is nothing to restore
-- it with:
--
--   • issues.reviewed_at / reviewed_by — review marks: WHO and WHEN looked
--     at the result. The only record of that fact.
--   • issues.review_required — the "review required" flag, set by authors
--     after release. The personal default will restore it only on NEW
--     issues.
--   • issues.assignee_set_at — burst schedule bases observed by the trigger
--     at assignment time. After rollback they have nowhere to come from —
--     exactly the state whose count issues_assigned_without_base reports
--     at deploy.
--   • user_notification_prefs.* — a HUMAN CHOICE: the mechanic kill switch,
--     the personal flag default, and three schedule parameters. Rollback
--     returns everyone to factory, including those who deliberately turned
--     the mechanic off (INV-6).
--   • reminder_rate_limit — rate-limit state in full, including frozen
--     window due times. A re-apply opens the windows again, and the first
--     worker tick is allowed to remind anyone who has a reason.
--
-- This is a conscious cost of rolling back an additive migration, not an
-- oversight: there is nowhere to park the dropped state, and holding
-- rollback for a manual export is an operator decision, not a migration
-- one.
--
-- ⚠ OUTCOME 'superseded'. Restoring the previous CHECK WILL FAIL if, by
-- rollback time, at least one notification has already finished by
-- supersession. That is deliberate: silently rewriting those rows into
-- another outcome would record a fact that never happened, and the new
-- value was introduced precisely so supersession is distinguishable from a
-- configuration refusal. Rollback in that situation must stop and demand
-- a human decision, not choose for them.
--
-- Order is the reverse of apply: dependents first (the trigger depends on
-- the function), then the objects themselves. IF EXISTS keeps rollback
-- idempotent if a partial apply left only some of the objects.

DELETE FROM migration_counters WHERE migration = '000044_review_and_nudges';

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_push_outcome_chk;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_push_outcome_chk
        CHECK (push_outcome IS NULL OR push_outcome IN (
            'success',
            'no_device',
            'access_revoked',
            'prefs_unavailable',
            'configuration_no_send',
            'transport_exhausted'
        ));

DROP TABLE IF EXISTS reminder_rate_limit;

ALTER TABLE user_notification_prefs
    DROP CONSTRAINT IF EXISTS user_prefs_unreviewed_interval_chk,
    DROP CONSTRAINT IF EXISTS user_prefs_unaccepted_interval_chk,
    DROP CONSTRAINT IF EXISTS user_prefs_reminder_burst_count_chk;

ALTER TABLE user_notification_prefs
    DROP COLUMN IF EXISTS unreviewed_interval_minutes,
    DROP COLUMN IF EXISTS unaccepted_interval_minutes,
    DROP COLUMN IF EXISTS reminder_burst_count,
    DROP COLUMN IF EXISTS review_required_default,
    DROP COLUMN IF EXISTS review_mechanic_enabled;

DROP TRIGGER IF EXISTS issues_stamp_assignee_set_at ON issues;
DROP FUNCTION IF EXISTS skein_stamp_assignee_set_at();

ALTER TABLE issues
    DROP COLUMN IF EXISTS assignee_set_at,
    DROP COLUMN IF EXISTS reviewed_by,
    DROP COLUMN IF EXISTS reviewed_at,
    DROP COLUMN IF EXISTS review_required;

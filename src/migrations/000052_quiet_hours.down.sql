-- Rollback of 000052_quiet_hours.
--
-- ⚠ THE OUTCOME 'quiet_hours'. Restoring the previous set of outcomes WILL FAIL if by
-- the time of the rollback at least one push has already ended in silence. This is deliberate and
-- repeats the decision of 000044 about `superseded`: silently rewriting such rows into
-- another outcome would write into the database a fact that did not happen — and the new value was
-- introduced precisely so that silence can be told apart from a path failure. In
-- such a situation the rollback must stop and require a human decision, not
-- make it for them.
--
-- ⚠ DATA LOSS. Dropping the columns erases time zones and configured quiet
-- windows irreversibly: this is a HUMAN CHOICE, not derived state, and
-- it can be restored after the rollback only from a backup.
--
-- The order is the reverse of the apply. IF EXISTS keeps the rollback idempotent if
-- a partial apply left only some of the objects.

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
            'transport_exhausted',
            'superseded'
        ));

ALTER TABLE user_notification_prefs
    DROP CONSTRAINT IF EXISTS user_prefs_quiet_hours_pair_chk,
    DROP CONSTRAINT IF EXISTS user_prefs_quiet_hours_nondegenerate_chk;

ALTER TABLE user_notification_prefs
    DROP COLUMN IF EXISTS quiet_hours_start,
    DROP COLUMN IF EXISTS quiet_hours_end;

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_timezone_chk;

ALTER TABLE users
    DROP COLUMN IF EXISTS timezone,
    DROP COLUMN IF EXISTS timezone_set_manually;

-- 000083_space_invitation_sends (down) — sk-674.18, T040.
--
-- The order is the reverse of the rollout: FIRST the settings rows of the space_invitation reason,
-- THEN narrowing the CHECK (ADD CONSTRAINT validates the table). Explicit
-- user choices for this reason and the sends log are lost - this is a rollback of the whole invitations
-- feature, not routine.

DELETE FROM user_notification_reason_prefs WHERE reason = 'space_invitation';

ALTER TABLE user_notification_reason_prefs
    DROP CONSTRAINT user_notification_reason_prefs_reason_check;

ALTER TABLE user_notification_reason_prefs
    ADD CONSTRAINT user_notification_reason_prefs_reason_check
        CHECK (reason IN (
            'assignment',
            'accepted',
            'delegated_status',
            'reminder',
            'mention',
            'issue_created',
            'comment',
            'attachment',
            'reparent',
            'auto_unclaim',
            'other_edits',
            'unaccepted_reminder',
            'review_reminder',
            'outcome_changed'
        ));

DROP TABLE space_invitation_sends;

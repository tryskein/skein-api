-- 000083_space_invitation_sends - sk-674.18, T040 (US-5, US-14, "Limits";
-- lead's decisions of 2026-10-14: the sends table and the invitation push reason).
--
-- 1. space_invitation_sends - a log of invitation SENDS: one row for
--    each creation and each resend, with the sender.
--
--    WHY. The SKEIN_SPACE_INVITES_PER_DAY limit is a sliding 24 hours per
--    sender (US-14), and a resend counts together with creation (the
--    resend contract rejects with daily_limit). A resend does not create a
--    space_invitations row - same id, new expiry - and the sender of a resend
--    may not be the author of the invitation (an admin). So counting from
--    space_invitations is impossible: the count is kept here. The same log over
--    10 minutes gives the "suspicious rate" threshold (captcha, rows 165/165a).
--
--    LINKS. The sender is ON DELETE CASCADE: a deleted account needs no
--    limit. Space and invitation are ON DELETE SET NULL: revoking or
--    deleting an invitation (or a space) does NOT erase the send, otherwise
--    "revoke -> create again" would bypass the daily limit.
--
--    RETENTION. Rows older than 24 hours take part in no count; they are
--    deleted by the invitation expiry worker in the same tick, bounded by
--    a batch (invitations/expiry.go#Expirer). The table does not grow without bound.
--
-- 2. The space_invitation notification reason in the closed set
--    user_notification_reason_prefs.reason (the registry
--    internal/notifications/reasons.go#ReasonKeySpaceInvitation): push
--    to the invitation recipient and to the inviter on acceptance. Settings rows are not
--    seeded: a missing row is read as the registry default
--    value (store.go#GetReasonStates).

CREATE TABLE space_invitation_sends (
    id            bigserial   PRIMARY KEY,
    sender_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    space_id      uuid        REFERENCES spaces(id) ON DELETE SET NULL,
    invitation_id uuid        REFERENCES space_invitations(id) ON DELETE SET NULL,
    sent_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_space_invitation_sends_sender ON space_invitation_sends (sender_id, sent_at);
-- The worker cleanup goes by row age, not by sender.
CREATE INDEX idx_space_invitation_sends_sent ON space_invitation_sends (sent_at);

COMMENT ON TABLE space_invitation_sends IS
    'Invitation sends (creation and resend) with the sender: sliding 24 h '
    'SKEIN_SPACE_INVITES_PER_DAY and the captcha threshold (sk-674.18 T040). Rows older than '
    '24 h are deleted by the invitation expiry worker.';

-- The list is written out in full: a CHECK in Postgres is replaced, not extended.
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
            'outcome_changed',
            'space_invitation'
        ));

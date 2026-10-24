-- 000079 (down): the previous 000077 rule - without a space only revoked.
-- Finished invitations of deleted spaces with another outcome do not
-- satisfy it and are deleted together with their tokens and emails (as in
-- down 000077) - the history of those invitations is lost on rollback.

UPDATE space_invitations SET current_token_id = NULL
 WHERE space_id IS NULL AND status <> 'revoked';
DELETE FROM auth_tokens WHERE invitation_id IN
    (SELECT id FROM space_invitations WHERE space_id IS NULL AND status <> 'revoked');
DELETE FROM outgoing_emails WHERE invitation_id IN
    (SELECT id FROM space_invitations WHERE space_id IS NULL AND status <> 'revoked');
DELETE FROM space_invitations WHERE space_id IS NULL AND status <> 'revoked';
ALTER TABLE space_invitations
    DROP CONSTRAINT space_invitations_deleted_space_not_pending,
    ADD CONSTRAINT space_invitations_deleted_space_revoked
        CHECK (space_id IS NOT NULL OR status = 'revoked');

-- 000077 (down): invitations of deleted spaces are deleted together with their
-- tokens and emails, space_id is mandatory again and blocks deletion.

UPDATE space_invitations SET current_token_id = NULL WHERE space_id IS NULL;
DELETE FROM auth_tokens WHERE invitation_id IN (SELECT id FROM space_invitations WHERE space_id IS NULL);
DELETE FROM outgoing_emails WHERE invitation_id IN (SELECT id FROM space_invitations WHERE space_id IS NULL);
DELETE FROM space_invitations WHERE space_id IS NULL;
ALTER TABLE space_invitations
    DROP CONSTRAINT space_invitations_deleted_space_revoked,
    DROP CONSTRAINT space_invitations_space_id_fkey,
    ADD CONSTRAINT space_invitations_space_id_fkey
        FOREIGN KEY (space_id) REFERENCES spaces(id) ON DELETE RESTRICT,
    ALTER COLUMN space_id SET NOT NULL;

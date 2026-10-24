-- 000077_space_invitations_survive_space — sk-674.18, T019 (US-4, US-5, E-6).
--
-- Deleting a space (K-22) revokes its pending invitations, but the invitation
-- row stays: the /invite/:token link for such an invitation answers
-- "space deleted" - the first state of the US-5 order (screens 2.5), before
-- "invalid". Therefore the invitation's space_id is nulled together with
-- the space deletion (ON DELETE SET NULL) instead of blocking it. An empty
-- space_id is exactly the "space deleted" marker; an invitation with an empty
-- space_id can only be revoked (CHECK).

ALTER TABLE space_invitations
    ALTER COLUMN space_id DROP NOT NULL,
    DROP CONSTRAINT space_invitations_space_id_fkey,
    ADD CONSTRAINT space_invitations_space_id_fkey
        FOREIGN KEY (space_id) REFERENCES spaces(id) ON DELETE SET NULL,
    ADD CONSTRAINT space_invitations_deleted_space_revoked
        CHECK (space_id IS NOT NULL OR status = 'revoked');

-- 000079_space_invitations_finished_survive - sk-674.18, T019 fix (review
-- api/4 #1; US-4, US-5, E-6; K-22).
--
-- WHY. 000077 allowed an invitation to survive a deleted space only
-- in status revoked (CHECK space_id IS NOT NULL OR status = 'revoked'). But
-- K-22 revokes only pending invitations, while accepted, declined and
-- expired ones keep their outcome. On DELETE FROM spaces the FK action
-- SET NULL runs as an UPDATE and runs into this CHECK: a space with
-- an invitation history cannot be deleted at all (23514, the whole cascade rolls back).
--
-- RULE. Spec: deleting a space revokes PENDING invitations
-- (US-4); the link of any invitation of a deleted space answers
-- "deleted" as the first state of the US-5 order - an empty space_id is exactly this
-- marker. A finished invitation keeps its outcome. Therefore only a pending
-- invitation can exist without a space.

ALTER TABLE space_invitations
    DROP CONSTRAINT space_invitations_deleted_space_revoked,
    ADD CONSTRAINT space_invitations_deleted_space_not_pending
        CHECK (space_id IS NOT NULL OR status <> 'pending');

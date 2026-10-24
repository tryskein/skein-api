-- 000074_space_members_user_cascade — sk-674.18, T017.
--
-- Space membership goes away together with the user, like the membership row
-- in a project (project_members.user_id ON DELETE CASCADE, 000003). In
-- the product a user is never physically deleted - account deletion
-- anonymizes the row (K-21), and membership is removed by the space primitives
-- (T018, T041); the cascade only applies when the users row is physically
-- deleted (test fixtures, manual cleanup). Space ownership
-- (spaces.owner_id) stays RESTRICT: a space without an owner does not
-- exist (INV-10).

ALTER TABLE space_members
    DROP CONSTRAINT space_members_user_id_fkey,
    ADD CONSTRAINT space_members_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

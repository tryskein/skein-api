-- 000074_space_members_user_cascade (down) - back to RESTRICT (000069).

ALTER TABLE space_members
    DROP CONSTRAINT space_members_user_id_fkey,
    ADD CONSTRAINT space_members_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT;

-- 000080_projects_space_not_null - sk-674.18, T024 (lead's decision:
-- NOT NULL projects.space_id moved from T016 to here, into the commit of the write path
-- POST /projects with spaceId).
--
-- From this commit every write path creates a project in a space. Projects
-- created before it by the old path (without space_id) move by the
-- 000071 transition rule (step 2): into the common space (created if absent,
-- the owner is the earliest active non-external admin), the visibility of the moved
-- projects is members. Members: if this migration creates the common space - all
-- non-external users (the 000071 rule); if the common space already exists - its
-- membership stays as is (those removed by an admin and accounts with spaceIds: [] are not
-- brought back), only the non-external members
-- of the moved projects are added: they already had access to them, and a project member
-- of a space is a space member (INV-5).
--
-- The owner of the common space is only an active, non-external, non-deleted admin (one
-- definition with 000071 and the startup check, INV-20). The startup check
-- (config/startup_checks.go) blocks migrations while there are projects without a
-- space and no such admin; a migration run bypassing startup without
-- one fails with an explicit error instead of leaving NULL under NOT NULL.

DO $$
DECLARE
    v_alphabet constant text := 'abcdefghijklmnopqrstuvwxyz234567';
    v_owner    uuid;
    v_common   uuid;
    v_short    text;
    v_cname    text;
    v_created  boolean := false;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM projects WHERE space_id IS NULL) THEN
        RETURN;
    END IF;

    SELECT id INTO v_common FROM spaces WHERE kind = 'common';
    IF v_common IS NULL THEN
        SELECT u.id INTO v_owner
          FROM users u
         WHERE u.role = 'admin' AND u.deactivated_at IS NULL AND u.deleted_at IS NULL AND NOT u.is_external
         ORDER BY u.created_at, u.id
         LIMIT 1;
        IF v_owner IS NULL THEN
            RAISE EXCEPTION 'projects without a space must move into the common space, '
                'but there is no active admin to own it (sk-674.18 T024)';
        END IF;
        LOOP
            SELECT string_agg(substr(v_alphabet, 1 + floor(random() * 32)::int, 1), '')
              INTO v_short FROM generate_series(1, 8);
            BEGIN
                INSERT INTO spaces (short_id, kind, owner_id, name)
                VALUES (v_short, 'common', v_owner, 'Common')
                RETURNING id INTO v_common;
                v_created := true;
                EXIT;
            EXCEPTION WHEN unique_violation THEN
                GET STACKED DIAGNOSTICS v_cname = CONSTRAINT_NAME;
                IF v_cname <> 'idx_spaces_short_id' THEN
                    RAISE;
                END IF;
            END;
        END LOOP;
    END IF;

    IF v_created THEN
        INSERT INTO space_members (space_id, user_id, role)
        SELECT v_common, u.id, CASE WHEN u.id = v_owner THEN 'owner' ELSE 'member' END
          FROM users u
         WHERE NOT u.is_external AND u.deleted_at IS NULL
        ON CONFLICT (space_id, user_id) DO NOTHING;
    ELSE
        INSERT INTO space_members (space_id, user_id, role)
        SELECT DISTINCT v_common, u.id, 'member'
          FROM project_members pm
          JOIN projects p ON p.id = pm.project_id
          JOIN users u ON u.id = pm.user_id
         WHERE p.space_id IS NULL AND NOT u.is_external AND u.deleted_at IS NULL
        ON CONFLICT (space_id, user_id) DO NOTHING;
    END IF;

    UPDATE projects SET space_id = v_common, visibility = 'members' WHERE space_id IS NULL;
END
$$;

ALTER TABLE projects ALTER COLUMN space_id SET NOT NULL;

-- 000071_spaces_transition - sk-674.18, T016 (US-2, US-21, INV-19, INV-22, D-12; K-23 step 2).
--
-- WHY. Move an existing installation to spaces without widening
-- access (INV-19): afterwards every project is in exactly one space,
-- project memberships and roles are unchanged, the only new thing for each user is their own empty
-- "Personal" space.
--
-- WHAT (per K-23).
--   1. "Personal" for every non-external user, deactivated ones too
--      (INV-8): a space of kind=personal, the user is its owner and only
--      member. Externals are outside spaces (E-16).
--   2. The shared "Common" space - only if the installation has projects (a transition
--      with data, INV-22). The owner is the earliest by created_at ACTIVE
--      admin: not deactivated, not deleted, not external - the same definition
--      as config/startup_checks.go#activeAdminExists and
--      accounts.EnsureCommonSpace. Members are all non-external users,
--      deactivated ones too. All projects (archived too) go to the common one with
--      visibility members (D-12: space would make everything visible to everyone).
--      Without projects the common space is not created here: on a fresh installation it is
--      created by api startup per SKEIN_COMMON_SPACE_ENABLED (T020).
--   3. Project membership rows get the source "manual", not "from
--      space" (INV-6); roles are not touched.
--   4. The email of existing users is confirmed (US-5: they were created by an admin
--      or SSO).
--   5. Themes (US-21, K-31): the author of each theme is its creator; the name is
--      unique within an author, case-insensitively (the global
--      case-sensitive name uniqueness is dropped). Themes of one author
--      that differ only by name case (allowed before the transition) are separated
--      before the index is created: in each such group the earliest by
--      (created_at, id) keeps the name, each following one in the same order
--      gets the first free suffix " 2", " 3", ... for that author (the same
--      suffix rule as for the copies below). id, values and theme selections are not
--      changed. The original names of the renamed rows are recorded in
--      spaces_transition_theme_renames - down uses it to restore them: the suffix
--      is checked only within the author and can match the name of another
--      author's theme, while the global uniqueness that down restores does not allow such
--      a match. There is no name length limit in the DB; a renamed name can
--      exceed the validator limit (themes/validate.go#nameMaxLen) by the length of the
--      suffix - like a copy name. Everyone who selected someone else's theme gets a personal copy with the same
--      name and values, the selection is switched to the copy; a name matching
--      one of their own themes (case-insensitively) gets a numeric
--      suffix " 2", " 3", .... The copies are recorded in
--      spaces_transition_theme_copies - down uses it to restore the selection.
--
-- FAILURE. A transition without an owner for the common space is impossible; this is checked by the
-- api pre-start check before migrations (config/startup_checks.go#
-- PreMigration, K-19): a RAISE in the migration was rejected (it would leave the schema dirty).
-- Migrations are run only by api after this check. A migration run
-- bypassing api on a DB with projects and no active admin does not create the common space and
-- leaves projects without a space: api startup refuses while there is no active
-- admin; if one appears later (reactivation), the check will pass, and
-- the projects will stay without a space - such a bypass is not supported and
-- is fixed manually.
--
-- THEME NAME UNIQUENESS - NULLS NOT DISTINCT: themes created before T035
-- by a write path without author_id share one namespace, as before
-- (NOT NULL author_id is set by T035 together with the write path - lead's decision,
-- sk-674.18).

ALTER TABLE themes DROP CONSTRAINT themes_name_key;

UPDATE themes SET author_id = created_by WHERE author_id IS NULL;

-- Themes of one author that differ only by name case: see item 5 of the
-- header. NULL authors share one namespace (like the index below).
CREATE TABLE spaces_transition_theme_renames (
    theme_id      uuid NOT NULL PRIMARY KEY,
    original_name text NOT NULL
);

DO $$
DECLARE
    v_theme record;
    v_n     int;
    v_name  text;
BEGIN
    FOR v_theme IN
        SELECT d.id, d.name, d.author_id
          FROM (SELECT t.id, t.name, t.author_id,
                       row_number() OVER (PARTITION BY t.author_id, lower(t.name)
                                          ORDER BY t.created_at, t.id) AS rn
                  FROM themes t) d
         WHERE d.rn > 1
         ORDER BY d.author_id, lower(d.name), d.rn
    LOOP
        v_n := 2;
        LOOP
            v_name := v_theme.name || ' ' || v_n;
            EXIT WHEN NOT EXISTS (
                SELECT 1 FROM themes
                 WHERE author_id IS NOT DISTINCT FROM v_theme.author_id
                   AND lower(name) = lower(v_name));
            v_n := v_n + 1;
        END LOOP;
        INSERT INTO spaces_transition_theme_renames (theme_id, original_name)
        VALUES (v_theme.id, v_theme.name);
        UPDATE themes SET name = v_name WHERE id = v_theme.id;
    END LOOP;
END $$;

CREATE UNIQUE INDEX idx_themes_author_name_lower
    ON themes (author_id, lower(name)) NULLS NOT DISTINCT;

CREATE TABLE spaces_transition_theme_copies (
    user_id     uuid NOT NULL,
    copy_id     uuid NOT NULL PRIMARY KEY,
    original_id uuid NOT NULL
);

DO $$
DECLARE
    v_alphabet constant text := 'abcdefghijklmnopqrstuvwxyz234567';
    v_user     record;
    v_theme    record;
    v_owner    uuid;
    v_common   uuid;
    v_space    uuid;
    v_short    text;
    v_name     text;
    v_n        int;
    v_copy     uuid;
    v_cname    text;
BEGIN
    -- 1. "Personal" for every non-external user.
    FOR v_user IN
        SELECT u.id FROM users u
         WHERE NOT u.is_external
           AND NOT EXISTS (SELECT 1 FROM spaces s WHERE s.owner_id = u.id AND s.kind = 'personal')
         ORDER BY u.created_at, u.id
    LOOP
        LOOP
            SELECT string_agg(substr(v_alphabet, 1 + floor(random() * 32)::int, 1), '')
              INTO v_short FROM generate_series(1, 8);
            BEGIN
                INSERT INTO spaces (short_id, kind, owner_id, name)
                VALUES (v_short, 'personal', v_user.id, 'Personal')
                RETURNING id INTO v_space;
                EXIT;
            EXCEPTION WHEN unique_violation THEN
                GET STACKED DIAGNOSTICS v_cname = CONSTRAINT_NAME;
                IF v_cname <> 'idx_spaces_short_id' THEN
                    RAISE;
                END IF;
                -- short id collision - try again
            END;
        END LOOP;
        INSERT INTO space_members (space_id, user_id, role) VALUES (v_space, v_user.id, 'owner');
    END LOOP;

    -- 2. Common - only for a transition with data.
    SELECT u.id INTO v_owner
      FROM users u
     WHERE u.role = 'admin' AND u.deactivated_at IS NULL AND u.deleted_at IS NULL AND NOT u.is_external
     ORDER BY u.created_at, u.id
     LIMIT 1;
    IF v_owner IS NOT NULL AND EXISTS (SELECT 1 FROM projects WHERE space_id IS NULL) THEN
        SELECT id INTO v_common FROM spaces WHERE kind = 'common';
        IF v_common IS NULL THEN
            LOOP
                SELECT string_agg(substr(v_alphabet, 1 + floor(random() * 32)::int, 1), '')
                  INTO v_short FROM generate_series(1, 8);
                BEGIN
                    INSERT INTO spaces (short_id, kind, owner_id, name)
                    VALUES (v_short, 'common', v_owner, 'Common')
                    RETURNING id INTO v_common;
                    EXIT;
                EXCEPTION WHEN unique_violation THEN
                    GET STACKED DIAGNOSTICS v_cname = CONSTRAINT_NAME;
                    IF v_cname <> 'idx_spaces_short_id' THEN
                        RAISE;
                    END IF;
                END;
            END LOOP;
        END IF;

        INSERT INTO space_members (space_id, user_id, role)
        SELECT v_common, u.id, CASE WHEN u.id = (SELECT owner_id FROM spaces WHERE id = v_common)
                                    THEN 'owner' ELSE 'member' END
          FROM users u
         WHERE NOT u.is_external
        ON CONFLICT (space_id, user_id) DO NOTHING;

        UPDATE projects SET space_id = v_common, visibility = 'members' WHERE space_id IS NULL;
    END IF;

    -- 5. Personal copies of other people's selected themes.
    FOR v_theme IN
        SELECT u.id AS user_id, t.id AS theme_id, t.name, t.base, t.variables
          FROM users u
          JOIN themes t ON t.id = u.custom_theme_id
         WHERE t.author_id IS DISTINCT FROM u.id
         ORDER BY u.created_at, u.id
    LOOP
        v_name := v_theme.name;
        v_n := 1;
        WHILE EXISTS (SELECT 1 FROM themes
                       WHERE author_id = v_theme.user_id AND lower(name) = lower(v_name)) LOOP
            v_n := v_n + 1;
            v_name := v_theme.name || ' ' || v_n;
        END LOOP;
        INSERT INTO themes (name, base, variables, created_by, updated_by, author_id)
        VALUES (v_name, v_theme.base, v_theme.variables, v_theme.user_id, v_theme.user_id, v_theme.user_id)
        RETURNING id INTO v_copy;
        INSERT INTO spaces_transition_theme_copies (user_id, copy_id, original_id)
        VALUES (v_theme.user_id, v_copy, v_theme.theme_id);
        UPDATE users SET custom_theme_id = v_copy WHERE id = v_theme.user_id;
    END LOOP;
END
$$;

-- 3. Membership sources: everything existing is "manual".
UPDATE project_members SET via_manual = true, via_space = false
 WHERE NOT via_manual OR via_space;

-- 4. Existing users' email is confirmed.
UPDATE users SET email_confirmed_at = now() WHERE email_confirmed_at IS NULL;

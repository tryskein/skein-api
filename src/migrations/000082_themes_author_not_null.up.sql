-- 000082_themes_author_not_null — sk-674.18, T035 (US-21, K-31).
--
-- WHY. Since T035 a theme belongs to an author: the write path
-- (themes/store.go#CreateTheme) writes author_id, all reads and edits
-- are filtered by it. A theme without an author is visible to no one - so
-- author_id becomes NOT NULL (lead's decision: NOT NULL is set by T035
-- together with the write path, 000071 left the column nullable).
--
-- WHAT. Themes created between 000071 and this release by the old write path
-- (without author_id) go through the same transition rules as in 000071 (item 5):
--   1. The author is the creator: author_id = created_by (lead's condition).
--      If the author already has a theme with the same name, case-insensitively,
--      the incoming theme gets the first free suffix " 2",
--      " 3", ... for that author - the same suffix rule as in 000071; id, values and
--      selections are not changed.
--   2. Everyone who selected someone else's theme gets a personal copy with the same name and
--      values, the selection is switched to the copy (the same suffix rule).
--   3. author_id NOT NULL.
-- On an installation where 000071 and this migration are applied in one release,
-- items 1-2 find nothing: there are no themes without an author and no foreign selections.
--
-- ROLLBACK. down drops only NOT NULL: authors and copies stay - they
-- are correct for pre-T035 code too, which does not read the author.

DO $$
DECLARE
    v_theme record;
    v_n     int;
    v_name  text;
    v_copy  uuid;
BEGIN
    -- 1. Authorship of themes without an author, one by one: the suffix is computed against
    --    the author's themes that already have an author.
    FOR v_theme IN
        SELECT t.id, t.name, t.created_by
          FROM themes t
         WHERE t.author_id IS NULL
         ORDER BY t.created_at, t.id
    LOOP
        v_name := v_theme.name;
        v_n := 1;
        WHILE EXISTS (SELECT 1 FROM themes
                       WHERE author_id = v_theme.created_by AND lower(name) = lower(v_name)) LOOP
            v_n := v_n + 1;
            v_name := v_theme.name || ' ' || v_n;
        END LOOP;
        UPDATE themes SET author_id = v_theme.created_by, name = v_name WHERE id = v_theme.id;
    END LOOP;

    -- 2. Personal copies of other people's selected themes.
    FOR v_theme IN
        SELECT u.id AS user_id, t.name, t.base, t.variables
          FROM users u
          JOIN themes t ON t.id = u.custom_theme_id
         WHERE t.author_id <> u.id
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
        UPDATE users SET custom_theme_id = v_copy WHERE id = v_theme.user_id;
    END LOOP;
END
$$;

ALTER TABLE themes ALTER COLUMN author_id SET NOT NULL;

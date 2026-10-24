-- 000056_user_ui_prefs_nav_layout — sk-551.8.
--
-- WHY. On a laptop there is not enough width: on the left stand two navigation
-- columns — "Queues" and "Projects" — and the issue column with the issue panel share
-- the rest. The owner decided (sk-551.8, 2026-09-30 and 2026-10-05): a PERSONAL
-- setting "Left panel: Columns side by side / Sections in one column" in the
-- avatar menu → Appearance, "Columns side by side" by default. The mode follows the
-- person (it applies after a reload and on another machine), so it
-- lives in the profile on the server, in the same appearance table as the issue window
-- (000054) and density (000055). The mode geometry (widths, height share,
-- collapses) depends on the window and is stored on the machine — it does not come here.
--
-- ⚠ WHY text WITH A BAN ON FOREIGN VALUES, and not a boolean "sections".
-- There are two modes today; with a third the text column stays as it is — a new
-- migration only needs to extend the CHECK user_ui_prefs_nav_layout_chk, whereas a boolean
-- would have to change type. Values are written as the frontend knows them:
-- columns, sections. The CHECK holds the dictionary (case is strict): a foreign word
-- cannot be written, NULL is blocked by NOT NULL.
--
-- ⚠ WHY NOT NULL DEFAULT 'columns'. Rows in user_ui_prefs are created
-- lazily; existing rows (of those who turned on the issue window or changed
-- density) and new ones created by a write of another setting must get
-- the factory value — "Columns side by side", today's layout. The mode has no third
-- state ("did not choose"): did not choose — means columns side by side.
ALTER TABLE user_ui_prefs
    ADD COLUMN nav_layout text NOT NULL DEFAULT 'columns',
    ADD CONSTRAINT user_ui_prefs_nav_layout_chk
        CHECK (nav_layout IN ('columns', 'sections'));

COMMENT ON COLUMN user_ui_prefs.nav_layout IS
    'Left panel mode: columns — the "Queues" and "Projects" columns side by side, '
    'sections — sections in one column. Default columns (sk-551.8).';

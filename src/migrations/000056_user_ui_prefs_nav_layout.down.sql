-- Rollback of 000056_user_ui_prefs_nav_layout.
--
-- ⚠ DATA LOSS: the choice of people who turned on the "Sections in one
-- column" mode is erased. After the rollback everyone has "Columns side by side" again — the factory
-- setting, so the interface does not break, but the choice will have to be
-- made again. The other appearance settings (issue_modal_views, density) are
-- not touched.
ALTER TABLE user_ui_prefs
    DROP CONSTRAINT IF EXISTS user_ui_prefs_nav_layout_chk,
    DROP COLUMN IF EXISTS nav_layout;

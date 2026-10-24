-- Rollback of 000055_user_ui_prefs_density.
--
-- ⚠ DATA LOSS: the choice of people who turned on compact density is erased.
-- After the rollback everyone has normal density again — the factory setting, so
-- the interface does not break, but the choice will have to be made again. The other
-- appearance settings (issue_modal_views) are not touched.
ALTER TABLE user_ui_prefs
    DROP CONSTRAINT IF EXISTS user_ui_prefs_density_chk,
    DROP COLUMN IF EXISTS density;

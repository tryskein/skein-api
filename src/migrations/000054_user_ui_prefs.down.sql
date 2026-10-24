-- Rollback of 000054_user_ui_prefs.
--
-- ⚠ DATA LOSS: together with the table, the choice of people who turned on the
-- issue popup window is erased. After the rollback everyone has the panel next to the view again —
-- the factory setting, so the interface does not break, but the choice
-- will have to be made again.
DROP TABLE IF EXISTS user_ui_prefs;

-- 000055_user_ui_prefs_density — sk-551.7.
--
-- WHY. After the redesign the font base grew from 14 to 16 px, and with it
-- control heights, filter chips and card paddings. The owner on a 16" MacBook
-- sees expanded filters eating half the screen; the user tutundu
-- asks for smaller fonts and paddings. The owner decided (sk-551.7, 2026-09-30 and
-- 2026-10-05): a PERSONAL setting "Density: Normal / Compact" next to
-- the theme in the avatar menu, "Normal" by default. The setting follows the
-- person, so it lives in the profile on the server, in the same appearance
-- table as the issue window (000054), not in localStorage.
--
-- ⚠ WHY text WITH A BAN ON FOREIGN VALUES, and not a boolean "compact".
-- There are two levels today, but in known systems density also comes
-- in three steps (comfortable / cozy / compact); with a third level the text
-- column stays as it is — a new migration only needs to extend the CHECK
-- user_ui_prefs_density_chk, whereas a boolean would have to change type. Values are written as the
-- frontend knows them (`data-density` on <html>): normal, compact. The CHECK holds
-- the dictionary: a foreign word cannot be written, NULL is blocked by NOT NULL.
--
-- ⚠ WHY NOT NULL DEFAULT 'normal'. Rows in user_ui_prefs are created
-- lazily; existing rows (of those who turned on the issue window) and new ones
-- created by a write of another setting must get the factory
-- value — "Normal", today's sizes. Density has no third state
-- ("did not choose"): did not choose — means normal.
ALTER TABLE user_ui_prefs
    ADD COLUMN density text NOT NULL DEFAULT 'normal',
    ADD CONSTRAINT user_ui_prefs_density_chk
        CHECK (density IN ('normal', 'compact'));

COMMENT ON COLUMN user_ui_prefs.density IS
    'Interface density: normal — factory sizes, compact — smaller font, '
    'lower controls and chips, tighter paddings. Default normal (sk-551.7).';

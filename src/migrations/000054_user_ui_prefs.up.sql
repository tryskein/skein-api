-- 000054_user_ui_prefs — sk-551.5.
--
-- WHY. An open issue today appears as a panel in a row next to the view
-- (cards, board, tree) and shares its width with it: on a 13–14" laptop the panel
-- comes out narrow and the board cropped. The owner decided (sk-551.5, 2026-10-05):
-- a PERSONAL setting "open an issue in a popup window", separately for
-- each of the three views, OFF by default everywhere. The setting follows the
-- person, so it lives in the profile on the server, not in localStorage — the same
-- as the theme (users.theme_*).
--
-- ⚠ WHY A SEPARATE TABLE, and not a users column or user_notification_prefs.
-- This is the first interface APPEARANCE setting, and the next ones are already named after it
-- (density sk-551.7, left panel mode sk-551.8). user_notification_prefs is
-- about what to notify about and how; mixing the interface view into it would mean
-- making the notification form know about layout. Columns in users — a hot
-- row that every authorization reads — would widen it for the sake of pure
-- cosmetics. A per-user table with a lazy row is the same technique as in
-- user_notification_prefs: no row — all settings are at factory values.
--
-- ⚠ WHY text[] WITH A BAN ON FOREIGN VALUES, and not three booleans. The three checkboxes are
-- independent, and the owner worded the setting as one field — "the set of
-- views with the window enabled". The set is stored as an array of view names in the
-- spelling in which the frontend knows them (`ViewMode`: cards, kanban, tree).
-- The CHECK holds the dictionary: a name the frontend does not know, and a NULL element,
-- cannot be written (`<@` does not treat NULL as equal to anything). Order and
-- uniqueness are the application layer's concern: it writes the set normalized
-- (cards, kanban, tree; no repeats).
--
-- ⚠ WHY NOT NULL DEFAULT '{}'. An empty set and "settings untouched"
-- mean the same thing — the panel next to the view everywhere, as before. The set has no third
-- state.
CREATE TABLE user_ui_prefs (
    user_id           uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    issue_modal_views text[]      NOT NULL DEFAULT '{}',
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_ui_prefs_issue_modal_views_chk
        CHECK (issue_modal_views <@ ARRAY['cards', 'kanban', 'tree']::text[])
);

COMMENT ON TABLE user_ui_prefs IS
    'Personal interface appearance settings, one row per user, '
    'created lazily. No row — all settings are at factory values (sk-551.5).';

COMMENT ON COLUMN user_ui_prefs.issue_modal_views IS
    'Issue views (cards, kanban, tree) in which the selected issue opens '
    'in a popup window in the center instead of a panel in the row. Empty — the panel is '
    'next to the view everywhere, as before. Empty by default (sk-551.5).';

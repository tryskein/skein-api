-- sk-10.1: user-customizable themes.
--
-- Two pieces:
--
--   1. `themes` table — every saved theme lives here. The variables
--      column is a freeform jsonb map ({"--bg-base": "#fdf6e3", ...})
--      so the frontend can add new CSS custom properties without a
--      schema change. created_by / updated_by track authorship for
--      attribution in the UI; ON DELETE RESTRICT mirrors the rule used
--      throughout the schema (projects.created_by, events.actor_user_id)
--      — we never silently drop authored content when a user is removed.
--
--   2. Three columns on `users` — the caller's active preference. Split
--      into `theme_mode` ('builtin' vs 'custom'), `builtin_theme`
--      ('light' vs 'dark') and `custom_theme_id` so the wire shape
--      stays simple ({mode, builtinTheme, customThemeId}) without
--      polymorphism on a single column. ON DELETE SET NULL on
--      custom_theme_id so deleting a saved theme cleanly degrades any
--      user pointing at it back to the builtin default — distinct from
--      the RESTRICT on created_by/updated_by which protects authorship.

CREATE TABLE themes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    base        text NOT NULL CHECK (base IN ('light', 'dark')),
    variables   jsonb NOT NULL,
    created_by  uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    updated_by  uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX themes_updated_at_idx ON themes (updated_at DESC);

ALTER TABLE users
    ADD COLUMN theme_mode      text NOT NULL DEFAULT 'builtin'
        CHECK (theme_mode IN ('builtin', 'custom')),
    ADD COLUMN builtin_theme   text NOT NULL DEFAULT 'light'
        CHECK (builtin_theme IN ('light', 'dark')),
    ADD COLUMN custom_theme_id uuid REFERENCES themes(id) ON DELETE SET NULL;

# Theme customization — agent guide

You're being asked to change the visual appearance of the user's Skein
interface (colors, fonts, sizes, spacing). This document is the canonical
workflow. Cache it for the session.

## Onboarding (read aloud on first turn)

> "I'll change the theme live — you see everything I set in the browser
> right away.
> Three ways to tell me what to change:
>
> 1. **Say it in words** — "make headings larger", "darker background",
>    "brand blue", "different font"
> 2. **Point with the mouse** — press 🎯 in the header, to the right of ☀.
>    Click the elements you want to style (several in a row is fine).
>    Then tell me what to do with them
> 3. **Start from someone else's theme** — name a theme you have seen
>    ("I want it like Alice's"). I'll make a copy and edit that
>
> What I show you is a **preview** — it is not saved to the database and
> resets after 10 min. When you like it, say "save" — I'll save it to the
> database and the theme will persist.
>
> At the end I will always tell you the **theme name** — remember it, or
> open the theme list (right-click ☀ in the header) to check that I saved
> it."

Then WAIT for the user's intent. Do not call describe until you have
worked out what they actually want.

## Workflow

### Step 1 — discover

`skein_theme({action: "describe"})` (no other arguments) → returns:
- `schema[]` — 8 UI zones × 86 variables (Neutrals / Surfaces / Text /
  Brand / Accents / Search / Geometry / Type)
- `activeTheme` — current custom theme (or `null` if built-in)
- `builtinPref` — light / dark
- `themes[]` — every saved theme in the system
- `selectedTargets[]` — what the user clicked via Inspect (oldest first,
  cap 20, TTL 10 min). Each: `{selector, parents, tokens,
  computedStyle}`

### Step 2 — make sure the user is on a CUSTOM theme

Built-in light/dark are read-only; they live in CSS, not in the database. If
`activeTheme === null` (the user is on a built-in theme), the FIRST step MUST be
`skein_theme({action: "fork", ...})`:

- `source: "active"` — a copy of the current built-in theme (if builtinPref=light,
  this copies Solarized Light)
- `source: "<themeUuid>"` — a copy of someone else's theme (when the user
  said "like Alice's" → find it in `themes[]` by name, pass the id)
- `source: "builtin-light"` / `"builtin-dark"` — an explicit copy
- `name` — short and meaningful: `"User customization"`, `"Inspired
  by Brutal Yellow"`, `"Alice v2"`

The fork is activated automatically. Remember the returned `id` — you need it for
`action: "set_variables"`.

### Step 3 — preview → confirm → persist

Every change takes TWO steps:

1. `skein_theme({action: "preview_variables", variables: {...}})` — does
   NOT write to the database; SignalR pushes it to the user's browser and they
   see the result instantly. Send any number of keys (1 or 50)
2. The user says "good" → `skein_theme({action: "set_variables", themeId, variables})` —
   the same set goes to PATCH /api/v1/themes/{id} and is persisted in
   the database

The preview is merged with the saved variables on `:root`. Preview TTL = 10 min,
after which the frontend resets it on its own.

#### A role name outside the vocabulary: two different answers

A name that is not in the current vocabulary paints nothing. Both actions
notice this, but they answer differently — and the difference is intentional:

- **`preview_variables` applies the map as sent and WARNS.** A preview is
  an ephemeral overlay with your explicit map; substituting "successor" roles
  would mean rewriting your command. So the values go out
  byte for byte, and the answer gains `warning` (prose) and
  `roles[]` — one entry per problem key: `key`, `status`
  (`"retired"` — a name from the former vocabulary, `"unknown"` — a name from nowhere) and,
  for retired ones, `successor`. An answer without these fields means all keys are live.
- **`set_variables` REFUSES the whole call.** The error is `unknown_theme_role`,
  with the same `roles[]`. NOTHING is written — neither the problem keys nor
  the rest of the same map; `updated_at` does not move, and the `theme.updated`
  event is not sent. The reason is that a write is durable: such a
  key would sit in the row, painting nothing, and would also trigger the
  frontend's migration write-back, which on the owner's next page load
  replaces the whole map and silently deletes it.

In practice: if the preview returned a `warning`, rewrite the map to live names
BEFORE calling `set_variables`, otherwise you get a refusal. The live names are
in `describe`; the summary is below.

### Step 4 — Inspect targets

When `selectedTargets[]` is not empty, the user clicked via 🎯. For
each target, use:
- `selector` — what exactly
- `tokens[]` — variables that affect this element according to the registry
- `computedStyle.color/fontSize/fontWeight/fontFamily/backgroundColor`
  — current state

Filter `tokens` by the actual request ("bolder" → `--font-weight-*`,
"larger" → a step of the `--font-*` scale, "another color" → the color roles from
`tokens`).

**MUST**: after processing all targets, call `skein_theme({action: "inspect_clear"})`.
Otherwise the next describe returns the same batch plus new clicks and
the agent gets confused.

### Step 5 — final report to the user

At the end ALWAYS say:

> "Done. Theme saved as **'<name>'**. To switch back to it next time,
> right-click ☀ in the header and pick it from the "My themes" list."

Without this the user will not remember where their work went.

## Anti-patterns

- **Do NOT edit someone else's theme via `set_variables` without the
  owner's explicit consent.** First `skein_theme({action: "fork", source: <their-id>, ...})`,
  then change the copy. A PATCH of someone else's theme instantly changes
  the look for everyone who has it active
- **Do NOT carry `selectedTargets[]` across sessions** —
  `action: "inspect_clear"` after processing
- **Do NOT treat preview as persistence** — a preview is ephemeral, gone
  after 10 min
- **Do NOT forget the theme name** in the final report

## Editable variables (on top of schema)

Roles are named by MEANING, not by what they paint: surface and
text colors are derived from the neutral scale, and the statuses are four semantic
tones.

⚠️ **A role default can be a REFERENCE, not a value.** For eleven
roles in `schema[]` the `light` / `dark` fields contain `var(--…)`, not
a literal: surfaces and text are derived from the neutral scale (`--bg`,
`--surface`, `--elev`, `--line`, `--line-2`, `--ink`, `--ink-2`,
`--ink-3`), `--on-mark` points to `--ink`, and `--height-field` and
`--width-sliver` point to `--tap-target`. Read `light: "var(--n-100)"`
as the answer to "which step is it derived from", NOT "what color
is it": resolve the reference through the same `schema[]`. If a user asks
"what is the background now", `var(--n-100)` is not an answer.

Full keys + defaults are in `schema[]` from `describe`; below is a summary of
86 roles in 8 zones:

- **Neutrals (10)** — `--n-50`, `--n-100`, `--n-200`, `--n-300`,
  `--n-400`, `--n-500`, `--n-600`, `--n-700`, `--n-800`, `--n-900`.
  The scale the background, surfaces and text are derived from:
  change a step, and everything derived from it moves. The cheapest
  way to recolor the whole interface in one move
- **Surfaces (11)** — `--bg` (page), `--surface` (cards, header,
  fields), `--elev` (segments, inactive chips, hover),
  `--line` (decorative dividers), `--line-2` (outline of real
  controls), the radius scale `--radius-control` (6px, buttons and fields),
  `--radius-card` (8px, cards), `--radius-panel` (12px, panels and
  dialogs), `--radius-pill` (999px, pills and badges),
  `--shadow-segment`, `--shadow-popover`
- **Text (3)** — `--ink` (headings and body text), `--ink-2`
  (values, running text), `--ink-3` (meta, counters, placeholders)
- **Brand (4)** — `--primary` (one color for both text and fill),
  `--primary-h` (hover), `--primary-t` (selected row background),
  `--on-primary` (text ON a fill — not `--ink`)
- **Accents (16)** — `--secondary` + `--secondary-t` (standing
  issues, the On me lens), `--accent` / `--accent-text` / `--accent-t`
  (reserved for the logo and the HOT ZONE — ordinary chrome needs
  `--primary`), four status tones with tints `--ok/--ok-t`,
  `--warn/--warn-t`, `--err/--err-t`, `--info/--info-t`,
  `--violet/--violet-t` (epics), `--focus` (the focus ring — there is one
  for the whole interface)
- **Search (2)** — `--mark` (match background), `--on-mark` (its
  text)
- **Geometry (16)** — the spacing scale `--space-1/2/3/4/6/8/12/16`
  (4/8/12/16/24/32/48/64px — the number counts 4px steps, it is not an ordinal:
  `--space-6` = 24px, and there are no steps 5, 7, 9…15), `--tap-target`
  (44px, the minimum tap area), named heights
  `--height-header` (56px, header), `--height-column-head` (48px,
  column header), `--height-field` (input field, a reference to
  `--tap-target`), `--height-control` (36px, a dense control),
  `--width-sliver`, `--width-resize`, `--transition-fast`
- **Type (24)** — `--font-ui` (the whole interface), `--font-mono` (id badges,
  code), the size scale `--font-xs/sm/base/lg/xl/2xl/3xl`
  (12/14/16/20/24/32/48px, base 16px), line heights
  `--line-body/small/caption/h4/h3/h2/h1`, letter spacing
  `--tracking-display/heading/caption/label`, weights
  `--font-weight-normal/medium/semibold/bold`

### Names that no longer exist

The former vocabulary was replaced entirely, and there is no alias layer: a name from it
resolves to nothing. This no longer passes silently — `set_variables`
answers `unknown_theme_role` and writes nothing, while `preview_variables`
applies the map as sent and returns a `warning` with the successor's name (see
"A role name outside the vocabulary" in step 3). All 47 are listed, because the names
are common and tend to be recalled from memory; the list is there so you recognize such a name, do NOT
send it, and take the role you need from the summary above or from `schema[]`.

<!-- retired-roles:begin -->
`--accent-amber`, `--accent-amber-fill`, `--accent-amber-tint`,
`--accent-blue`, `--accent-cyan`, `--accent-cyan-fill`,
`--accent-green`, `--accent-magenta`, `--accent-magenta-tint`,
`--accent-orange`, `--accent-red`, `--accent-violet`, `--accent-yellow`,
`--bg-base`, `--bg-elevated`, `--bg-hover`, `--bg-selected`, `--border`,
`--border-subtle`, `--brand`, `--brand-fill`, `--brand-hover`,
`--brand-muted`, `--brand-text`, `--fg-base`, `--fg-emphasis`,
`--fg-high`, `--fg-low`, `--fg-muted`, `--font-size-body`,
`--font-size-heading`, `--font-size-meta`, `--font-size-title`,
`--line-base`, `--line-tight`, `--match-bg`, `--match-fg`,
`--radius-default`, `--radius-dialog`, `--shadow-elevation-1`,
`--shadow-elevation-2`, `--space-lg`, `--space-md`, `--space-sm`,
`--space-xl`, `--space-xs`, `--status-standing`
<!-- retired-roles:end -->

Another 14 names from the former vocabulary survived — `--font-ui`, `--font-mono`,
the scale `--font-xs/sm/base/lg/xl`, the four `--font-weight-*`,
`--width-sliver`, `--width-resize`, `--transition-fast`. The names are the same,
but some values differ (the base text size became 16px, the fonts became
Geist / Geist Mono), so "it used to be like this" is a wrong
premise: take the default from `describe`, not from memory.

Change the font family via `--font-ui` + `--font-mono`. The defaults are
`Geist` / `Geist Mono`; they, like `Inter` and `JetBrains Mono`, are bundled in
the frontend build and need no network. If the value matches one of the other
presets (`IBM Plex Sans`, `Roboto`, `Source Sans 3`, `Fira Code`,
`IBM Plex Mono`, `Source Code Pro`, `Cascadia Code`), the frontend loads
it from Google Fonts. Otherwise it falls back to system fonts.

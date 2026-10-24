package themes

// SectionDTO is the wire shape for a theme-token section exposed by
// skein_theme(action="describe"). Mirrors the frontend's TokenSection in
// projects/skein-frontend/src/lib/themeTokens.ts so the agent sees the
// same grouping the editor renders.
//
// Selectors[] are intentionally NOT exposed at the section level — the
// agent doesn't need to know which CSS selectors paint each section
// (the frontend already handles "highlight the section in the live
// preview" via its own messageBus). Per-token selectors are also
// dropped for the same reason. What stays is everything that helps the
// agent decide which variable to change: id, label, description, plus
// per-token key/label/kind/light/dark.
type SectionDTO struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	Description string     `json:"description"`
	Tokens      []TokenDTO `json:"tokens"`
}

// TokenDTO is the wire shape for a single theme token. Light + Dark
// give the agent the default values for each base so it can answer
// "what's the current dark default for --primary?" without consulting
// the frontend.
//
// ⚠️ A DEFAULT MAY BE A REFERENCE, NOT A VALUE. Eleven of the roles
// below default to `var(--…)` rather than to a literal: the surface and
// ink roles are cut from the neutral ramp (--bg, --surface, --elev,
// --line, --line-2, --ink, --ink-2, --ink-3), --on-mark points at --ink,
// and --height-field / --width-sliver point at --tap-target. Reading
// Light/Dark for one of those answers "which role it is cut from", NOT
// "what colour it is" — resolve the reference through this same
// catalogue to get the concrete value. The old docblock named --bg-base
// as its example, which was doubly wrong: that role is retired, and it
// was itself a derived one.
//
// Kind is "color" or "text". The agent uses kind to decide whether the
// value should be a CSS colour (`#8a2d2d`, `rgba(...)`) or a free-form
// string (lengths, references, font stacks, shadows).
type TokenDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Light string `json:"light"`
	Dark  string `json:"dark"`
}

// TokenSchema is the static catalogue of theme-token sections that
// powers skein_theme(action="describe"). It is a HAND-MAINTAINED MIRROR
// of TOKEN_SECTIONS in projects/skein-frontend/src/lib/themeTokens.ts,
// which is the sole source of truth for the role dictionary.
//
// Dictionary as of sk-285.7 (T003/T004/T006/T007): 8 sections, 86 roles
// named after what they MEAN rather than what they paint — a neutral ramp
// (--n-50…--n-900) the surface and ink roles are cut from, one --primary
// family, --secondary, --accent (logo / HOT ZONE), the four status hues
// --ok / --warn / --err / --info with their tints, --violet for epics,
// --focus for the focus ring, a four-step radius scale, an eight-step
// spacing scale and four named heights. Values come from the design
// handoff (design/fable/handoff/tokens.css); the pre-redesign dictionary
// of 61 roles (--bg-base, --fg-low, --brand-fill, --accent-cyan, …) is
// gone and has no alias layer — those names resolve to nothing.
//
// ⚠️ THE BACKEND'S OWN CI CANNOT DETECT DRIFT AGAINST THE FRONTEND.
// skein-api and skein-frontend are separate git repositories; this repo's
// CI clones only its own tree, so themeTokens.ts is simply not on disk
// when the tests run. A test that read it would be permanently red in CI,
// or skipped — vacuous either way. TestCatalogueGolden in schema_test.go
// checks this file against its golden rendering, but it cannot see the
// frontend at all.
//
// It does NOT follow that nothing can compare the two. In the multi-repo
// WORKSPACE both trees sit side by side, and `scripts/check-theme-mirror.sh`
// at the workspace root compares them key by key, section by section and
// value by value — one command, run where both sides are on disk, never
// in this repo's CI. Run it whenever either side of the mirror moves; see
// "Known limitations" in this repository's README.md.
//
// This mirror has already gone stale once inside 72 seconds: it was exact
// against the registry when it was read and obsolete by the time it was
// committed, because the frontend's next commit landed in between. Read
// the registry, then re-read it right before you commit.
//
// When the frontend dictionary changes:
//  1. Change the TS TOKEN_SECTIONS first (with selectors[] and owner).
//  2. Mirror it here in the SAME wave — until you do, REST and MCP hand
//     agents a stale catalogue of role names with no error anywhere.
//  3. Regenerate the golden (SKEIN_UPDATE_TOKEN_CATALOGUE=1, see
//     schema_test.go) and read its diff.
//  4. Re-check the role summary in theme_instructions.md in
//     internal/mcp/tools.
//  5. Run `scripts/check-theme-mirror.sh` from the workspace root — it
//     is the only thing that actually proves steps 1–2 agree.
//
// Two fields of the TS TokenDef are deliberately NOT mirrored: `owner`
// (user / structural) drives the frontend's one-off theme migration and
// says nothing to an agent picking a variable to change, and `selectors`
// is documented as out of scope on SectionDTO above. Adding either would
// widen the MCP response shape, which sk-285.7 leaves untouched.
var TokenSchema = []SectionDTO{
	{
		ID:    "neutrals",
		Label: "Neutrals",
		Description: "The ten-step neutral ramp every surface and ink role is cut " +
			"from. Editing a step moves everything derived from it — retune " +
			"the ramp to reskin the whole shell in one pass.",
		Tokens: []TokenDTO{
			{Key: "--n-50", Label: "Neutral 50", Kind: "color", Light: "#ffffff", Dark: "#141210"},
			{Key: "--n-100", Label: "Neutral 100", Kind: "color", Light: "#f6f4f1", Dark: "#1c1917"},
			{Key: "--n-200", Label: "Neutral 200", Kind: "color", Light: "#efece7", Dark: "#26221e"},
			{Key: "--n-300", Label: "Neutral 300", Kind: "color", Light: "#e2ddd6", Dark: "#33302b"},
			{Key: "--n-400", Label: "Neutral 400", Kind: "color", Light: "#c6bfb6", Dark: "#4a453f"},
			{Key: "--n-500", Label: "Neutral 500", Kind: "color", Light: "#8c847b", Dark: "#7d766d"},
			{Key: "--n-600", Label: "Neutral 600", Kind: "color", Light: "#6a635b", Dark: "#a2998f"},
			{Key: "--n-700", Label: "Neutral 700", Kind: "color", Light: "#46413b", Dark: "#cfc7be"},
			{Key: "--n-800", Label: "Neutral 800", Kind: "color", Light: "#2b2622", Dark: "#e6e0d9"},
			{Key: "--n-900", Label: "Neutral 900", Kind: "color", Light: "#1c1917", Dark: "#f3eee8"},
		},
	},
	{
		ID:    "surfaces",
		Label: "Surfaces",
		Description: "Page background, card surface, elevated plates, hairlines and " +
			"control outlines, radii and elevation shadows. Affects every " +
			"framed surface.",
		Tokens: []TokenDTO{
			{Key: "--bg", Label: "Page background", Kind: "text", Light: "var(--n-100)", Dark: "var(--n-50)"},
			{Key: "--surface", Label: "Card surface", Kind: "text", Light: "var(--n-50)", Dark: "var(--n-100)"},
			{Key: "--elev", Label: "Elevated / inactive plate", Kind: "text", Light: "var(--n-200)", Dark: "var(--n-200)"},
			{Key: "--line", Label: "Hairline", Kind: "text", Light: "var(--n-300)", Dark: "var(--n-300)"},
			{Key: "--line-2", Label: "Control outline", Kind: "text", Light: "var(--n-500)", Dark: "var(--n-500)"},
			{Key: "--radius-control", Label: "Control radius", Kind: "text", Light: "6px", Dark: "6px"},
			{Key: "--radius-card", Label: "Card radius", Kind: "text", Light: "8px", Dark: "8px"},
			{Key: "--radius-panel", Label: "Panel / dialog radius", Kind: "text", Light: "12px", Dark: "12px"},
			{Key: "--radius-pill", Label: "Pill radius", Kind: "text", Light: "999px", Dark: "999px"},
			{Key: "--shadow-segment", Label: "Shadow (segment)", Kind: "text",
				Light: "0 1px 2px rgba(20, 18, 16, 0.12)",
				Dark:  "0 1px 2px rgba(0, 0, 0, 0.30)"},
			{Key: "--shadow-popover", Label: "Shadow (popover)", Kind: "text",
				Light: "0 8px 24px rgba(20, 18, 16, 0.16)",
				Dark:  "0 8px 24px rgba(0, 0, 0, 0.40)"},
		},
	},
	{
		ID:    "text",
		Label: "Text",
		Description: "The three ink steps — primary, secondary, muted. Global; " +
			"affects every rendered glyph. All three clear AA on --bg and " +
			"--surface in both themes.",
		Tokens: []TokenDTO{
			{Key: "--ink", Label: "Text primary", Kind: "text", Light: "var(--n-900)", Dark: "var(--n-900)"},
			{Key: "--ink-2", Label: "Text secondary", Kind: "text", Light: "var(--n-700)", Dark: "var(--n-700)"},
			{Key: "--ink-3", Label: "Text muted", Kind: "text", Light: "var(--n-600)", Dark: "var(--n-600)"},
		},
	},
	{
		ID:    "brand",
		Label: "Brand",
		Description: "The oxblood primary family — primary buttons, active pills and " +
			"lenses, issue ids, the selected-row wash.",
		Tokens: []TokenDTO{
			{Key: "--primary", Label: "Primary", Kind: "color", Light: "#8a2d2d", Dark: "#e49090"},
			{Key: "--primary-h", Label: "Primary hover", Kind: "color", Light: "#6f2222", Dark: "#ee9f9f"},
			{Key: "--primary-t", Label: "Primary tint", Kind: "text",
				Light: "rgba(138, 45, 45, 0.09)",
				Dark:  "rgba(227, 139, 139, 0.14)"},
			{Key: "--on-primary", Label: "Text on primary", Kind: "color", Light: "#ffffff", Dark: "#2a0f0f"},
		},
	},
	{
		ID:    "accents",
		Label: "Accents",
		Description: "Semantic hues — standing status, the logo / HOT ZONE ember, " +
			"the four status colours with their tints, the epic violet, and " +
			"the focus ring.",
		Tokens: []TokenDTO{
			{Key: "--secondary", Label: "Secondary (teal)", Kind: "color", Light: "#1f5f66", Dark: "#6fc4cd"},
			{Key: "--secondary-t", Label: "Secondary tint", Kind: "text",
				Light: "rgba(31, 95, 102, 0.10)",
				Dark:  "rgba(111, 196, 205, 0.14)"},
			{Key: "--accent", Label: "Accent (ember)", Kind: "color", Light: "#d2521a", Dark: "#f0834d"},
			{Key: "--accent-text", Label: "Accent (text)", Kind: "color", Light: "#9b390e", Dark: "#f59a6c"},
			{Key: "--accent-t", Label: "Accent tint", Kind: "text", Light: "rgba(210, 82, 26, 0.12)", Dark: "rgba(240, 131, 77, 0.16)"},
			// sk-368 darkened the LIGHT grade of --ok and --warn one step for
			// text contrast; sk-384 mirrors that here. The tints (--ok-t /
			// --warn-t) were deliberately NOT re-decided there and stay put —
			// leaving the chip's ground where it was is what buys the ink its
			// contrast. Registry side: src/lib/themeTokens.ts in skein-frontend.
			{Key: "--ok", Label: "Success", Kind: "color", Light: "#196534", Dark: "#6cc98a"},
			{Key: "--ok-t", Label: "Success tint", Kind: "text", Light: "rgba(31, 122, 63, 0.12)", Dark: "rgba(108, 201, 138, 0.14)"},
			{Key: "--warn", Label: "Warning", Kind: "color", Light: "#794e04", Dark: "#e6a93f"},
			{Key: "--warn-t", Label: "Warning tint", Kind: "text", Light: "rgba(154, 93, 5, 0.14)", Dark: "rgba(230, 169, 63, 0.16)"},
			{Key: "--err", Label: "Error", Kind: "color", Light: "#a82121", Dark: "#f38787"},
			{Key: "--err-t", Label: "Error tint", Kind: "text", Light: "rgba(180, 35, 35, 0.10)", Dark: "rgba(242, 128, 128, 0.14)"},
			{Key: "--info", Label: "Info", Kind: "color", Light: "#1b57a9", Dark: "#7fb2f7"},
			{Key: "--info-t", Label: "Info tint", Kind: "text", Light: "rgba(30, 95, 184, 0.10)", Dark: "rgba(127, 178, 247, 0.14)"},
			{Key: "--violet", Label: "Violet (epic)", Kind: "color", Light: "#5e39d1", Dark: "#aa9dee"},
			{Key: "--violet-t", Label: "Violet tint", Kind: "text", Light: "rgba(105, 71, 212, 0.10)", Dark: "rgba(168, 155, 238, 0.14)"},
			{Key: "--focus", Label: "Focus ring", Kind: "color", Light: "#1e5fb8", Dark: "#7fb2f7"},
		},
	},
	{
		ID:    "search",
		Label: "Search",
		Description: "Inline search-match highlight — background wash and its " +
			"foreground, for matched substrings inside titles, comments and " +
			"descriptions.",
		Tokens: []TokenDTO{
			{Key: "--mark", Label: "Match background", Kind: "text", Light: "rgba(234, 179, 8, 0.38)", Dark: "rgba(181, 137, 0, 0.3)"},
			{Key: "--on-mark", Label: "Match foreground", Kind: "text", Light: "var(--ink)", Dark: "var(--ink)"},
		},
	},
	{
		ID:    "geometry",
		Label: "Geometry",
		Description: "Spacing scale, sliver / resize column widths, the touch-target " +
			"size, and transition speed. Affects layout density and motion.",
		Tokens: []TokenDTO{
			{Key: "--space-1", Label: "Space 1", Kind: "text", Light: "4px", Dark: "4px"},
			{Key: "--space-2", Label: "Space 2", Kind: "text", Light: "8px", Dark: "8px"},
			{Key: "--space-3", Label: "Space 3", Kind: "text", Light: "12px", Dark: "12px"},
			{Key: "--space-4", Label: "Space 4", Kind: "text", Light: "16px", Dark: "16px"},
			{Key: "--space-6", Label: "Space 6", Kind: "text", Light: "24px", Dark: "24px"},
			{Key: "--space-8", Label: "Space 8", Kind: "text", Light: "32px", Dark: "32px"},
			{Key: "--space-12", Label: "Space 12", Kind: "text", Light: "48px", Dark: "48px"},
			{Key: "--space-16", Label: "Space 16", Kind: "text", Light: "64px", Dark: "64px"},
			{Key: "--tap-target", Label: "Touch target", Kind: "text", Light: "44px", Dark: "44px"},
			{Key: "--height-header", Label: "App header height", Kind: "text", Light: "56px", Dark: "56px"},
			{Key: "--height-column-head", Label: "Column header height", Kind: "text", Light: "48px", Dark: "48px"},
			{Key: "--height-field", Label: "Input field height", Kind: "text", Light: "var(--tap-target)", Dark: "var(--tap-target)"},
			{Key: "--height-control", Label: "Dense control height", Kind: "text", Light: "36px", Dark: "36px"},
			{Key: "--width-sliver", Label: "Sliver column width", Kind: "text", Light: "var(--tap-target)", Dark: "var(--tap-target)"},
			{Key: "--width-resize", Label: "Resize handle width", Kind: "text", Light: "4px", Dark: "4px"},
			{Key: "--transition-fast", Label: "Transition (fast)", Kind: "text", Light: "80ms ease-out", Dark: "80ms ease-out"},
		},
	},
	{
		ID:    "type",
		Label: "Type",
		Description: "Typography — UI font stack (body / labels / buttons), " +
			"monospace stack (inline code / id badges), the font-size " +
			"scale, line-height ramp and per-purpose weights. Global; sets " +
			"the typographic voice.",
		Tokens: []TokenDTO{
			{Key: "--font-ui", Label: "UI stack", Kind: "text",
				Light: "'Geist', system-ui, sans-serif",
				Dark:  "'Geist', system-ui, sans-serif"},
			{Key: "--font-mono", Label: "Monospace stack", Kind: "text",
				Light: "'Geist Mono', ui-monospace, Menlo, Consolas, monospace",
				Dark:  "'Geist Mono', ui-monospace, Menlo, Consolas, monospace"},
			{Key: "--font-xs", Label: "Font size XS", Kind: "text", Light: "12px", Dark: "12px"},
			{Key: "--font-sm", Label: "Font size SM", Kind: "text", Light: "14px", Dark: "14px"},
			{Key: "--font-base", Label: "Font size base", Kind: "text", Light: "16px", Dark: "16px"},
			{Key: "--font-lg", Label: "Font size LG", Kind: "text", Light: "20px", Dark: "20px"},
			{Key: "--font-xl", Label: "Font size XL", Kind: "text", Light: "24px", Dark: "24px"},
			{Key: "--font-2xl", Label: "Font size 2XL", Kind: "text", Light: "32px", Dark: "32px"},
			{Key: "--font-3xl", Label: "Font size 3XL", Kind: "text", Light: "48px", Dark: "48px"},
			{Key: "--line-body", Label: "Line height (body)", Kind: "text", Light: "1.5", Dark: "1.5"},
			{Key: "--line-small", Label: "Line height (small)", Kind: "text", Light: "1.45", Dark: "1.45"},
			{Key: "--line-caption", Label: "Line height (caption)", Kind: "text", Light: "1.4", Dark: "1.4"},
			{Key: "--line-h4", Label: "Line height (h4)", Kind: "text", Light: "1.3", Dark: "1.3"},
			{Key: "--line-h3", Label: "Line height (h3)", Kind: "text", Light: "1.2", Dark: "1.2"},
			{Key: "--line-h2", Label: "Line height (h2)", Kind: "text", Light: "1.15", Dark: "1.15"},
			{Key: "--line-h1", Label: "Line height (h1)", Kind: "text", Light: "1.1", Dark: "1.1"},
			{Key: "--tracking-display", Label: "Tracking (display)", Kind: "text", Light: "-0.02em", Dark: "-0.02em"},
			{Key: "--tracking-heading", Label: "Tracking (heading)", Kind: "text", Light: "-0.01em", Dark: "-0.01em"},
			{Key: "--tracking-caption", Label: "Tracking (caption)", Kind: "text", Light: "0.02em", Dark: "0.02em"},
			{Key: "--tracking-label", Label: "Tracking (uppercase label)", Kind: "text", Light: "0.07em", Dark: "0.07em"},
			{Key: "--font-weight-normal", Label: "Body weight", Kind: "text", Light: "400", Dark: "400"},
			{Key: "--font-weight-medium", Label: "Medium weight", Kind: "text", Light: "500", Dark: "500"},
			{Key: "--font-weight-semibold", Label: "Title weight", Kind: "text", Light: "600", Dark: "600"},
			{Key: "--font-weight-bold", Label: "Strong weight", Kind: "text", Light: "700", Dark: "700"},
		},
	},
}

// DefaultVariablesForBase builds the seed variables map for a brand-new
// theme draft. Mirrors the TS defaultVariablesForBase function in
// themeTokens.ts — every token's light or dark value, depending on base.
//
// Returns a fresh map (never aliased with TokenSchema) so callers can
// mutate freely.
func DefaultVariablesForBase(base string) map[string]string {
	// Size hint matches TokenSchemaKeys length (86 fixed tokens today).
	n := 0
	for _, sec := range TokenSchema {
		n += len(sec.Tokens)
	}
	out := make(map[string]string, n)
	for _, sec := range TokenSchema {
		for _, tok := range sec.Tokens {
			if base == "dark" {
				out[tok.Key] = tok.Dark
			} else {
				out[tok.Key] = tok.Light
			}
		}
	}
	return out
}

// TokenSchemaKeys returns every variable key in TokenSchema as a flat
// slice. Used by the schema test to assert order parity with the
// frontend.
func TokenSchemaKeys() []string {
	var n int
	for _, sec := range TokenSchema {
		n += len(sec.Tokens)
	}
	out := make([]string, 0, n)
	for _, sec := range TokenSchema {
		for _, tok := range sec.Tokens {
			out = append(out, tok.Key)
		}
	}
	return out
}

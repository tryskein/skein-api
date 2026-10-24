package tools

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/themes"
)

// themeActions is the action table of skein_theme — the enum of the
// schema, the runtime allowlist and the `allowedActions` an agent gets
// back on a miss, all from one slice (see actionSet).
//
// Order is the workflow the guide prescribes, and the enum renders in
// this order in tools/list, so it doubles as the shortest statement of
// that workflow: read the guide, see what exists, get an editable copy,
// show it, save it, tidy up. `instructions` is deliberately first — the
// tool it replaced said "call this BEFORE improvising with the other
// theme tools", and after the fold "the other theme tools" are actions in
// the same enum, where first position is the only ordering signal left.
var themeActions = actionSet{order: []string{
	"instructions", "describe", "fork", "preview_variables", "set_variables", "inspect_clear",
}}

// RegisterTheme wires skein_theme — the six sk-10.x theme tools
// (skein_theme_instructions / _describe / _fork / _preview_variables /
// _set_variables / _inspect_clear) folded into one tool with an `action`
// discriminator (sk-255.3).
//
// The per-action handlers are untouched and still live in their own files
// (handleThemeDescribe and friends); this file only supplies the schema
// and the dispatch table. That is what keeps the ~570 lines of
// theme_tools_test.go, which call those handlers directly, green without
// an edit.
//
// Schema shape (sk-255.3, forced by the platform): `action` is the only
// required parameter — three of the six actions take no arguments at all
// — and every other field is optional at the JSON Schema level with its
// condition stated in prose. Anthropic's API rejects a top-level
// oneOf/allOf/anyOf in input_schema with HTTP 400, so per-action required
// sets cannot be expressed structurally.
//
// No parameter had to be renamed. `variables` is the same partial
// name→value map in preview_variables and set_variables (the difference
// is where it lands, not what it is), and `source` / `name` / `themeId`
// each belong to exactly one action.
//
// One action does not return JSON. action="instructions" answers with the
// raw markdown guide via NewToolResultText — the only response on the
// whole MCP surface that is not an object — and that is load-bearing for
// the metrics classifier: classifyToolCall probes for an error envelope
// by requiring the body to start with `{`, so a markdown body is scored
// outcome="ok" with no error code, exactly as it was before the fold.
// Pinned by TestThemeInstructions_RawMarkdownStaysOK rather than assumed.
//
// Annotations are coarsened, as everywhere in this consolidation:
// describe and instructions are pure reads, but MCP annotations are per
// tool, so the tool declares readOnly=false and destructive=true — the
// latter inherited from inspect_clear, whose dropped selections the
// caller cannot recover (the frontend has to re-post them). The
// "Read-only actions:" line in the description is the compensation for a
// client that filters on readOnlyHint.
func RegisterTheme(s *server.MCPServer, store *themes.Store, publisher themes.Publisher, inspectCache *themes.InspectCache) {
	tool := mcpgo.NewTool(
		"skein_theme",
		mcpgo.WithDescription(
			"The caller's visual theme — colours, fonts, sizes, spacing. "+
				"Call action=\"instructions\" FIRST when the user asks for "+
				"visual changes (\"change the color\", \"another font\", \"make it "+
				"darker\", \"like Alice's\"): it returns the canonical "+
				"fork → preview → confirm → persist workflow and the "+
				"onboarding script, and improvising without it is the "+
				"documented way to get this wrong. Built-in light/dark are "+
				"read-only, so editing always starts with action=\"fork\". "+
				"Read-only actions: instructions, describe.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(themeActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"instructions\" → the canonical theme-editing guide (workflow, onboarding script, anti-patterns). Returns markdown, not JSON; cache it for the session.\n"+
					"  - \"describe\" → the token schema, the caller's active theme, every saved theme, and their recent Inspect-mode clicks.\n"+
					"  - \"fork\" → copy a theme (or a builtin) into a new editable one and activate it for the caller.\n"+
					"  - \"preview_variables\" → push an ephemeral preview to the caller's open tabs; nothing is saved. Applies your map verbatim, and warns (response `warning` + `roles[]`) about any key the current role dictionary does not declare — such a key paints nothing.\n"+
					"  - \"set_variables\" → merge variables into a saved theme and persist them. Refuses the WHOLE patch with `unknown_theme_role` (+ `roles[]`, with the successor of each retired name) if any key is not a current role: nothing is written, because a dead key in a saved row paints nothing and gets silently deleted by the frontend's next migration write-back.\n"+
					"  - \"inspect_clear\" → drop the caller's accumulated Inspect-mode selections.",
			),
		),
		mcpgo.WithString("source",
			mcpgo.Description("What to fork: \"active\" (the caller's current theme, falling back to their builtin), \"builtin-light\", \"builtin-dark\", or a theme UUID from describe. Required for fork."),
		),
		mcpgo.WithString("name",
			mcpgo.Description("Name for the new theme; unique among your own themes (another user's theme of the same name never collides), 1..100 chars. Required for fork."),
		),
		mcpgo.WithString("themeId",
			mcpgo.Description("UUID of the theme to write to, as returned by fork or describe. Required for set_variables."),
		),
		mcpgo.WithObject("variables",
			mcpgo.Description("Partial map of CSS variable name → value. Supplied keys overwrite, absent keys are preserved (unlike the REST PATCH, which replaces the whole map). Required for set_variables and preview_variables. Every key must be a role of the CURRENT dictionary (action=\"describe\", or the summary in action=\"instructions\"): the pre-redesign names have no alias layer and resolve to nothing. set_variables refuses such a patch outright (`unknown_theme_role`); preview_variables applies it as sent and warns."),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, themeActions.with(map[string]server.ToolHandlerFunc{
		"instructions":      handleThemeInstructions(),
		"describe":          handleThemeDescribe(store, inspectCache),
		"fork":              handleThemeFork(store, publisher),
		"preview_variables": handleThemePreviewVariables(publisher),
		"set_variables":     handleThemeSetVariables(store, publisher),
		"inspect_clear":     handleThemeInspectClear(inspectCache),
	}))
}

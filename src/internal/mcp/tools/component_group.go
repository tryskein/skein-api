package tools

import (
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/projects"
)

// componentActions is the action table of skein_component — the enum of
// the schema, the runtime allowlist and the `allowedActions` an agent
// gets back on a miss, all from one slice (see actionSet).
//
// Order is the lifecycle: find out what exists, add one, edit it, retire
// it. `list` sits first on purpose — it is the action an agent is
// supposed to reach for before every skein_create (see the component
// resolution section of the operational guide), and the enum renders in
// this order in tools/list.
var componentActions = actionSet{order: []string{
	"list", "create", "update", "archive",
}}

// RegisterComponent wires skein_component — the four R2.2c component
// tools (skein_component_list / _create / _update / _archive) folded into
// one tool with an `action` discriminator (sk-255.3).
//
// The per-action handlers are untouched and still live in their own files
// (handleComponentList and friends); this file only supplies the schema
// and the dispatch table. That keeps per-action owner gating, store-error
// mapping and validation out of one long switch.
//
// Schema shape (sk-255.3, forced by the platform): `action` is required,
// and so is `project` — not as an exception to the flat-schema rule but
// because every one of the four actions needs it, which makes it a plain
// entry in the `required` ARRAY rather than a conditional. (A conditional
// requirement could not be expressed anyway: Anthropic's API rejects a
// top-level oneOf/allOf/anyOf in input_schema with HTTP 400.) Everything
// else is optional at the JSON Schema level with its conditions stated in
// prose in the parameter description.
//
// One rename was unavoidable here, and it is the only place in the whole
// consolidation where "leave the parameter as it was" is not physically
// possible: the folded tools disagreed on the TYPE of `archived`. In
// skein_component_list it was a three-valued string enum ("false" |
// "true" | "all") selecting which rows to return; in
// skein_component_archive it is a boolean choosing archive vs unarchive.
// JSON Schema gives one type per property name. The read-side filter is
// therefore `archivedFilter` and the boolean keeps `archived`, because
// rewriting the callers of a filter is cheaper than rewriting the callers
// of a mutation.
//
// Annotations are coarsened, as everywhere in this consolidation: `list`
// is a pure read, but MCP annotations are per tool, so the tool declares
// readOnly=false. destructive stays false — archiving is soft, nothing is
// hard-deleted. The "Read-only actions:" line in the description is the
// compensation for a client that filters on readOnlyHint.
func RegisterComponent(s *server.MCPServer, pool *pgxpool.Pool, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_component",
		mcpgo.WithDescription(
			"Components of a project — sub-units such as a microservice, a "+
				"module or an area of code, which issues are attached to so "+
				"search and reports can group work. Call action=\"list\" before "+
				"skein_create to pick the right `componentIds`. Writing "+
				"(create / update / archive) is for the project owner, space owner or instance admin; archiving is soft "+
				"and reversible with archived=false. Read-only actions: list.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(componentActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"list\" → the project's components, filtered by `archivedFilter`, "+
					"plus `archivedCount` — how many of the project's components are "+
					"archived, so an empty `items` tells \"no components at all\" "+
					"(0) apart from \"all of them are archived\" (>0) without a "+
					"second call.\n"+
					"  - \"create\" → add a component; name must be unique within the project.\n"+
					"  - \"update\" → rename a component or replace its description.\n"+
					"  - \"archive\" → soft-archive a component, or unarchive it with archived=false.",
			),
		),
		mcpgo.WithString("project",
			mcpgo.Required(),
			mcpgo.Description("Project handle: alias, name, or UUID. Required for every action."),
		),
		mcpgo.WithString("id",
			mcpgo.Description("Component UUID, as returned by list or create. Required for update and archive."),
		),
		mcpgo.WithString("name",
			mcpgo.Description("Component name, unique within the project (case-sensitive). Required for create; on update, omit to leave the name unchanged."),
		),
		mcpgo.WithString("description",
			mcpgo.Description("Free-form component description (markdown). Optional on create; on update, omit to leave it unchanged or pass an empty string to clear it."),
		),
		mcpgo.WithString("archivedFilter",
			mcpgo.Description("Only read by list: which rows to return — \"false\" (default: active only), \"true\" (archived only), or \"all\"."),
			mcpgo.Enum("false", "true", "all"),
			mcpgo.DefaultString("false"),
		),
		mcpgo.WithBoolean("archived",
			mcpgo.Description("Only read by archive: true → archive (default), false → unarchive. Re-archiving an already-archived component is a no-op that returns the current state."),
			mcpgo.DefaultBool(true),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, componentActions.with(map[string]server.ToolHandlerFunc{
		"list":    handleComponentList(pool, projectsStore),
		"create":  handleComponentCreate(pool, projectsStore),
		"update":  handleComponentUpdate(pool, projectsStore),
		"archive": handleComponentArchive(pool, projectsStore),
	}))
}

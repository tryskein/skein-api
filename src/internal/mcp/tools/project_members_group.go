package tools

import (
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/projects"
	"skein-api/internal/users"
)

// projectMemberActions is the action table of skein_project_members —
// the enum of the schema, the runtime allowlist and the `allowedActions`
// an agent gets back on a miss, all from one slice (see actionSet).
//
// Order is the sequence a caller actually walks: read the roster, then
// add, then remove. `list` first also matters for a caller with no write
// permission, which is most of them — list is visible to every member of
// the project, add and remove are owner-or-admin.
var projectMemberActions = actionSet{order: []string{
	"list", "add", "remove",
}}

// RegisterProjectMembers wires skein_project_members — the three R3.1
// membership tools (skein_project_member_list / _add / _remove) folded
// into one tool with an `action` discriminator (sk-255.3).
//
// The per-action handlers are untouched and still live in their own
// files (handleProjectMemberAdd and friends); this file only supplies the
// schema and the dispatch table. That is what keeps the ~340 lines of
// project_member_*_test.go, which call those handlers directly, green
// without an edit, and it keeps the three DIFFERENT permission gates
// (visible-to-caller for list, owner-or-admin for add, owner-or-admin-or-
// self for remove) in separate functions instead of one nested switch.
//
// Schema shape (sk-255.3, forced by the platform): `action` is required,
// and so is `project` — not as an exception to the flat-schema rule but
// because all three actions need it, which makes it a plain entry in the
// `required` ARRAY rather than a conditional. (A conditional requirement
// could not be expressed anyway: Anthropic's API rejects a top-level
// oneOf/allOf/anyOf in input_schema with HTTP 400.) Everything else is
// optional at the JSON Schema level with its condition in prose.
//
// No parameter had to be renamed. `userId` / `username` are the same
// mutually-exclusive target pair in add and remove, and `role` belongs to
// add alone, so nothing collides on name or on type.
//
// Annotations are coarsened, as everywhere in this consolidation: `list`
// is a pure read, but MCP annotations are per tool, so the tool declares
// readOnly=false and destructive=true — `remove` deletes a membership row
// and the folded skein_project_member_remove already declared itself
// destructive. The "Read-only actions:" line in the description is the
// compensation for a client that filters on readOnlyHint.
func RegisterProjectMembers(s *server.MCPServer, pool *pgxpool.Pool, projectsStore *projects.Store, usersStore *users.Store) {
	tool := mcpgo.NewTool(
		"skein_project_members",
		mcpgo.WithDescription(
			"Who is on a project and with which role ('owner' | 'member'). "+
				"Reading the roster is open to any member of the project; "+
				"adding and removing require a project owner (the space's owner "+
				"and the instance admin included), "+
				"except that a member may remove themselves "+
				"(self-leave). Adding is idempotent — re-adding with the same "+
				"role writes nothing. A member who has access to the project through "+
				"its space cannot be removed (`member_via_space`): switch the "+
				"project's visibility to members or remove them from the space. "+
				"Read-only actions: list.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(projectMemberActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"list\" → the full member roster with per-row role and added_at / added_by.\n"+
					"  - \"add\" → add a user to the project, or change an existing member's role.\n"+
					"  - \"remove\" → drop a membership; removing a non-member returns `member_not_found`.",
			),
		),
		mcpgo.WithString("project",
			mcpgo.Required(),
			mcpgo.Description("Project handle: alias, name, or UUID. Required for every action. A project the caller cannot see renders as `project_not_found`."),
		),
		mcpgo.WithString("userId",
			mcpgo.Description("UUID of the target user. Mutually exclusive with `username`; exactly one of the two is required for add and remove."),
		),
		mcpgo.WithString("username",
			mcpgo.Description("Username of the target user (case-insensitive). Mutually exclusive with `userId`."),
		),
		mcpgo.WithString("role",
			mcpgo.Description("Only read by add: role to grant. Required for add. External users cannot be owners."),
			mcpgo.Enum("owner", "member"),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, projectMemberActions.with(map[string]server.ToolHandlerFunc{
		"list":   handleProjectMemberList(pool, projectsStore),
		"add":    handleProjectMemberAdd(pool, projectsStore, usersStore),
		"remove": handleProjectMemberRemove(pool, projectsStore, usersStore),
	}))
}

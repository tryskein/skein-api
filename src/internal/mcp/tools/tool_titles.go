package tools

import mcpgo "github.com/mark3labs/mcp-go/mcp"

// toolTitles maps tool name → annotations.title, the human-readable name
// a client shows for the tool in its tool list and in permission prompts.
//
// The Claude connector directory requires a `title` on every tool next to
// the readOnlyHint / destructiveHint annotations (connector pre-submission
// checklist, "Provide tool annotations"); other clients and catalogs show
// it in place of the snake_case name.
//
// Writing rules:
//
//   - A short Title Case label for a person reading a list, not a
//     description: the description already explains behaviour.
//   - Name the object, not the product: every tool is SKEIN's, so
//     "Show Issue", not "Show SKEIN Issue".
//   - A consolidated tool (an `action` enum) is titled by its object, not
//     by one of its actions: "Manage Attachments" covers upload, list,
//     download and delete alike.
//   - Titles are distinct: two tools with the same title are two rows a
//     person cannot tell apart in a permission prompt.
//
// Every registered tool must have an entry — TestToolTitles_EveryTool
// (cmd/api) fails otherwise.
var toolTitles = map[string]string{
	// Identity / onboarding.
	"skein_whoami":           "Who Am I",
	"skein_get_instructions": "Get Agent Guide",
	"skein_prime":            "Session Overview",

	// Reads over issues.
	"skein_show":   "Show Issue",
	"skein_list":   "List Issues",
	"skein_search": "Search",
	"skein_ready":  "List Ready Issues",

	// Issue lifecycle.
	"skein_create":      "Create Issue",
	"skein_epic_create": "Create Epic with Subtasks",
	"skein_update":      "Update Issue",
	"skein_update_many": "Update Issues in Bulk",
	"skein_claim":       "Claim Issue",
	"skein_unclaim":     "Release Issue",
	"skein_accept":      "Accept Assignment",
	"skein_close":       "Close Issue",
	"skein_close_many":  "Close Issues in Bulk",
	"skein_move":        "Move Issue to Project",

	// Comments.
	"skein_comment":        "Add Comment",
	"skein_comment_get":    "Get Comment",
	"skein_comment_update": "Edit Comment",
	"skein_comment_delete": "Delete Comment",

	// Graph edges.
	"skein_dep_add":      "Add Dependency",
	"skein_dep_add_many": "Add Dependencies in Bulk",
	"skein_dep_remove":   "Remove Dependency",
	"skein_reparent":     "Change Parent",

	// Git provenance.
	"skein_commits_attach": "Attach Commits",
	"skein_commits_detach": "Detach Commit",

	// Delegation lens.
	"skein_drop_delegated":   "Hide Delegated Issue",
	"skein_resume_delegated": "Restore Delegated Issue",

	// Projects.
	"skein_project_create":  "Create Project",
	"skein_project_list":    "List Projects",
	"skein_project_resolve": "Resolve Project",
	"skein_project_update":  "Update Project",

	// Consolidated tools (sk-255.3).
	"skein_project_members": "Manage Project Members",
	"skein_component":       "Manage Components",
	"skein_user":            "Manage Users",
	"skein_watchers":        "Manage Subscriptions",
	"skein_notifications":   "Manage Notifications",
	"skein_remind":          "Manage Reminders",
	"skein_attachments":     "Manage Attachments",
	"skein_theme":           "Manage Themes",

	// Monitoring.
	"skein_get_monitor_script": "Get Monitor Script",
}

// stampTitle copies the tool's toolTitles entry into annotations.title.
// A tool with no entry keeps whatever title its Register* function set
// (none today); the missing entry is caught by TestToolTitles_EveryTool
// against the real registration list.
func stampTitle(tool *mcpgo.Tool) {
	if title, ok := toolTitles[tool.Name]; ok {
		tool.Annotations.Title = title
	}
}

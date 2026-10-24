package tools

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// metaSearchHint is the per-tool _meta key Claude Code reads to bias the
// Tool Search ranking of deferred (not-always-loaded) tools. A hit in the
// hint scores roughly twice what a hit in the description scores, so the
// hint is the highest-leverage place to put the words an agent will
// actually type.
const metaSearchHint = "anthropic/searchHint"

// searchHints maps tool name → anthropic/searchHint.
//
// The server exposes 41 tools. With server-side alwaysLoad off, the
// client keeps only the names in the prompt and pulls a schema through
// Tool Search on demand — so a tool that nobody's query matches is a
// tool that effectively does not exist. The hint is what closes that
// gap.
//
// Writing rules (they are what keeps the table useful, not decorative):
//
//   - Search keys, not prose. No sentence structure — this is matched as
//     a bag of terms, not read by a human. Budget follows what the tool
//     covers: 3–8 words for a single-verb tool, 10–18 for a consolidated
//     one (skein_attachments and its siblings, sk-255.3), whose hint has
//     to carry the union of the verbs its `action` enum folded in. A
//     folded tool's long hint is not clutter to be trimmed back: the verb
//     it lost from its NAME ("delete", "list") now survives only here, in
//     the enum values and in the description, and the hint is the
//     highest-weighted of the three. Both budgets are enforced by
//     TestSearchHints_Budget (cmd/api), which reads the stamped _meta off
//     the registered surface and decides "consolidated" by whether the
//     tool declares an `action` enum — a rule nothing checks is a rule the
//     next hint quietly breaks.
//   - Synonyms over precision. Include the other names a caller might use
//     for the same act ("close/finish", "lock/claim").
//   - Never restate the tool name — the name is indexed on its own, so
//     spending hint budget on it buys nothing.
//   - Neighbours must not collide. Sibling tools (claim vs update, close
//     vs unclaim vs commits_attach, list vs search vs ready, dep_add vs
//     reparent) are the pairs a mis-ranked search actually confuses;
//     their hints deliberately share no distinctive term.
//
// Every registered tool must have an entry — TestSearchHints_EveryTool
// (cmd/api) fails the build otherwise.
var searchHints = map[string]string{
	// Identity / onboarding.
	"skein_whoami":           "who am I identity session projects",
	"skein_get_instructions": "agent handbook operational guide onboarding rules",
	"skein_prime":            "session start overview orientation snapshot what to do",

	// Reads over issues.
	"skein_show":   "open issue fully details tree comments",
	"skein_list":   "list project issues filters paginated browse",
	"skein_search": "find by text fuzzy full-text comments",
	"skein_ready":  "what can start now unblocked next work",

	// Issue lifecycle.
	"skein_create":      "create new issue task bug",
	"skein_epic_create": "epic with subtasks breakdown decompose feature",
	"skein_update":      "change fields status priority assignee patch",
	"skein_update_many": "bulk update several issues batch patch",
	"skein_claim":       "pick up lock start in_progress",
	"skein_unclaim":     "let go drop unlock handoff pause release",
	"skein_accept":      "confirm acceptance acknowledge assignment seen",
	"skein_close":       "finish complete done reason resolution",
	"skein_close_many":  "bulk finish several issues batch",
	"skein_move":        "relocate to another project copy supersedes",

	// Comments.
	"skein_comment":        "write comment note add message",
	"skein_comment_get":    "read one comment by uuid fetch",
	"skein_comment_update": "revise own comment edit body",
	"skein_comment_delete": "delete comment remove tombstone",

	// Graph edges.
	"skein_dep_add":      "link dependency blocks depends relates edge",
	"skein_dep_add_many": "bulk add links batch edges",
	"skein_dep_remove":   "remove link unlink break dependency",
	"skein_reparent":     "change parent subtask new parent hierarchy",

	// Git provenance.
	"skein_commits_attach": "attach git commits hashes to issue",
	"skein_commits_detach": "remove wrongly attached git commit unlink detach",

	// Delegation lens.
	"skein_drop_delegated":   "stop watching delegated dismiss hide",
	"skein_resume_delegated": "restore delegated back undo dismiss",

	// Projects.
	"skein_project_create":  "create new project register tracker",
	"skein_project_list":    "list available projects browse",
	"skein_project_resolve": "recognize project by name repo fuzzy handle",
	"skein_project_update":  "project settings aliases repos archive",

	// Project membership — one consolidated tool (sk-255.3), so the hint
	// is the union of what the three folded tools used to advertise.
	// Deliberately carries no "role": that word is the distinctive
	// term of skein_user's hint, and these two admin-shaped tools are
	// exactly the pair a mis-ranked search would confuse.
	"skein_project_members": "project participant add exclude who belongs access member membership add remove list grant revoke leave",

	// Components — one consolidated tool (sk-255.3), so the hint is the
	// union of what the four folded tools used to advertise on their own.
	"skein_component": "component module service area list create rename archive component list create update archive",

	// Users (admin) — one consolidated tool (sk-255.3), so the hint is
	// the union of what the three folded tools used to advertise.
	"skein_user": "user account signup list change role password deactivate user account create list update deactivate admin",

	// Subscriptions — one consolidated tool (sk-255.3), so the hint is the
	// union of what the five folded tools used to advertise on their own.
	// "filter" + "create"/"delete" are load bearing: filter_create and
	// filter_delete lost their verbs from the tool NAME, and a lone
	// inflected form does not match the nominative an agent types. The
	// four stopwords the previous version spent on "what I am subscribed
	// to" are gone — "subscribed" is the only token in that phrase a
	// bag-of-terms match can use.
	"skein_watchers": "subscribe unsubscribe subscribed follow issue project filter create delete watch unwatch subscribe subscription filter create delete list",

	// Notifications — one consolidated tool (sk-255.3), so the hint is
	// the union of what the three folded tools used to advertise.
	// Deliberately carries no "watch": subscribing is skein_watchers,
	// and the pair "what am I subscribed to" vs "what landed in my
	// inbox" is exactly what a mis-ranked search confuses.
	"skein_notifications":      "alerts incoming unseen view flag archive chime inbox notifications unread mark read archive bell",
	"skein_get_monitor_script": "background monitor live stream headless bootstrap",

	// Reminders — one consolidated tool (sk-255.3), so the hint is the
	// union of what the four folded tools used to advertise on their own.
	"skein_remind": "alarm reminder set postpone reschedule delete what rings remind reminder snooze dismiss armed ringing alarm",

	// Attachments — one consolidated tool (sk-255.3), so the hint is the
	// union of what the six folded tools used to advertise on their own.
	"skein_attachments": "attachment file upload download delete list metadata attachment upload download delete presigned",

	// Themes — one consolidated tool (sk-255.3), so the hint is the
	// union of what the six folded tools used to advertise on their own.
	// "instructions" carries the action the tool's own description
	// orders an agent to call FIRST; without it the one entry point to
	// theme editing was the only action the hint did not name.
	"skein_theme": "theme appearance color font preview fork copy save reset inspector instructions theme colors fonts preview fork persist instructions",
}

// addTool is the single registration funnel for this package: it stamps
// the tool's anthropic/searchHint from searchHints, wraps the handler in
// the usage counters (see metrics.go), and then hands the tool to the
// server. Register* functions call this instead of s.AddTool so neither
// the hint nor the instrumentation can be forgotten in one file and
// present in the next 32 (the eight consolidated tools go through
// addActionTool instead — 33 of the 41 land here).
//
// A tool with no table entry is registered without a hint rather than
// failing at startup — the missing-entry case is a build-time bug, and
// TestSearchHints_EveryTool catches it against the real registration
// list before it can ship. Instrumentation, unlike the hint, has no
// table and therefore no way to be missing.
//
// Consolidated tools (an `action` enum instead of a verb in the name)
// go through addActionTool instead — see action_dispatch.go. addTool
// stays the single funnel for everything else, which is what keeps the
// empty `action` metric label meaning "not a folded tool".
func addTool(s *server.MCPServer, tool mcpgo.Tool, handler server.ToolHandlerFunc) {
	stampSearchHint(&tool)
	s.AddTool(tool, instrumentTool(tool.Name, handler))
}

// stampSearchHint copies the tool's searchHints entry into its _meta,
// preserving any _meta fields the Register* function already set (e.g.
// anthropic/maxResultSizeChars).
func stampSearchHint(tool *mcpgo.Tool) {
	hint, ok := searchHints[tool.Name]
	if !ok {
		return
	}
	if tool.Meta == nil {
		tool.Meta = &mcpgo.Meta{}
	}
	if tool.Meta.AdditionalFields == nil {
		tool.Meta.AdditionalFields = make(map[string]any, 1)
	}
	tool.Meta.AdditionalFields[metaSearchHint] = hint
}

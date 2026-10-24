package tools

import (
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
)

// watcherActions is the action table of skein_watchers — the enum of the
// schema, the runtime allowlist and the `allowedActions` an agent gets
// back on a miss, all from one slice (see actionSet).
//
// Order is by subscription scope, widening left to right — one issue,
// one project, a saved filter — and then the read that spans all three.
// The enum renders in this order in tools/list, so it doubles as a map of
// what can be subscribed to.
//
// Two of the five folded tools carried a discriminator of their own:
// skein_filter_watch had `op` (create|delete|list) and
// skein_project_watch had a boolean `unwatch`. Both are flattened here
// rather than nested under `action`. A discriminator inside a
// discriminator is not something an agent should have to reason about in
// a schema that already cannot express per-action requirements, and the
// flattened names keep the verbs ("create", "delete", "unwatch") in a
// field Tool Search indexes.
var watcherActions = actionSet{order: []string{
	"watch", "unwatch",
	"project_watch", "project_unwatch",
	"filter_create", "filter_delete", "filter_list",
	"list",
	"monitor_stop",
}}

// RegisterWatchers wires skein_watchers — the five R3.3 subscription
// tools (skein_watch / skein_unwatch / skein_project_watch /
// skein_filter_watch / skein_watching) folded into one tool with an
// `action` discriminator (sk-255.3), plus `monitor_stop` (sk-579 US-6):
// the caller stops one of its own task monitors by monitorId.
//
// The per-action handlers still live in their own files
// (handleWatch, handleUnwatch, handleProjectWatch, handleWatching); only
// skein_filter_watch's internal `op` switch was actually cut apart, into
// handleFilterWatchCreate / Delete / List, because flattening `op` left
// nothing to switch on.
//
// Schema shape (sk-255.3, forced by the platform): `action` is the only
// required parameter and every other field is optional at the JSON Schema
// level, with conditional requirements stated in prose in each parameter
// description. Anthropic's API rejects a top-level oneOf/allOf/anyOf in
// input_schema with HTTP 400, so per-action required sets cannot be
// expressed structurally at all.
//
// Two parameters were renamed on the way in, because the folded tools
// disagreed about what `id` meant: it was a hierarchical issue id in
// skein_watch / skein_unwatch and a custom-watcher UUID in
// skein_filter_watch. They are `issueId` and `filterId` here — one flat
// schema cannot carry one name for two identifier kinds without the
// agent guessing which one it is holding.
//
// Annotations are coarsened, as everywhere in this consolidation: two of
// the nine actions are pure reads, but MCP annotations are per tool, so
// the tool declares readOnly=false. destructive stays false, deliberately:
// the deletions here remove a subscription row (never anything a user
// authored), and monitor_stop revokes one of the caller's OWN monitor
// tokens irreversibly — the remedy is minting a new monitor, not restoring
// the old token, and no data, issue or other user's state is touched.
// Declaring the whole tool destructive would make clients that gate on
// destructiveHint confirm every watch/unwatch too. The "Read-only actions:"
// line in the description is the compensation for a client that filters on
// readOnlyHint.
func RegisterWatchers(
	s *server.MCPServer,
	pool *pgxpool.Pool,
	projectsStore *projects.Store,
	notifStore *notifications.Store,
	monitorStore *monitorcreds.Store,
) {
	tool := mcpgo.NewTool(
		"skein_watchers",
		mcpgo.WithDescription(
			"The caller's own notification subscriptions, at three scopes: one "+
				"issue, a whole project, or a saved filter (custom watcher). "+
				"Automatic subscriptions: the assignee of an issue; its creator "+
				"(the human who owns the API key) only for issues created by "+
				"hand or as a TOP-LEVEL agent_directed issue — an issue your "+
				"agent files under a parent, or with source=agent_autonomous, "+
				"does not subscribe you. Unsubscribing from an issue you watch "+
				"records an opt-out: the auto-watch paths (creator, assignee, "+
				"reopen) will not re-subscribe you to that issue until you "+
				"watch it again with action=\"watch\", and the issue's card leaves "+
				"your Watching column. Per-user cap: "+
				"10 filter watchers. Also stops one of your task monitors "+
				"(action=\"monitor_stop\", by the `monitorId` skein_get_monitor_script "+
				"returned). Read-only actions: list, filter_list.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(watcherActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"watch\" → subscribe to one issue (reason='manual'; removes your opt-out from an earlier unwatch).\n"+
					"  - \"unwatch\" → unsubscribe from one issue: the subscription is deleted, an opt-out stops automatic re-subscription, the issue's Watching card is cleared. Not subscribed → only the card is removed, no opt-out.\n"+
					"  - \"project_watch\" → subscribe to a whole project's notifiable events.\n"+
					"  - \"project_unwatch\" → unsubscribe from a project.\n"+
					"  - \"filter_create\" → create a saved-filter watcher from `name` + `filter`.\n"+
					"  - \"filter_delete\" → remove one filter watcher by `filterId`.\n"+
					"  - \"filter_list\" → list the caller's filter watchers.\n"+
					"  - \"list\" → the caller's full subscription state: issues, projects and filters at once.\n"+
					"  - \"monitor_stop\" → stop one of your task monitors by `monitorId`: its token is revoked, its runner ends with token_expired on its next poll, other monitors (same issue included) keep running. Works from any of your API keys; stopping an already stopped monitor succeeds; another user's or an unknown id → monitor_not_found.",
			),
		),
		mcpgo.WithString("issueId",
			mcpgo.Description("Full hierarchical issue id, e.g. \"demo-42\" or \"demo-42.1\". Required for watch and unwatch."),
		),
		mcpgo.WithBoolean("subtree",
			mcpgo.Description("Only read by watch. When true, subscribe to this issue AND its "+
				"subtree by the hierarchy: every issue that has it as a parent, "+
				"grandparent, … at the moment of each event (parentIdCurrent chain — not "+
				"an id prefix); future subtasks are covered automatically. subtree dominates: "+
				"it upgrades an existing plain watch and a later subtree=false "+
				"watch never downgrades it. Default: false (this issue only)."),
		),
		mcpgo.WithString("project",
			mcpgo.Description("Project handle: alias, name, or UUID. Required for project_watch and project_unwatch."),
		),
		mcpgo.WithArray("eventTypes",
			mcpgo.Description(
				"Only read by project_watch: whitelist of event types to subscribe "+
					"to (e.g. [\"issue.created\", \"comment.created\"]). Each value must "+
					"be a notifiable event type. Omit to leave the existing whitelist "+
					"alone (or receive every notifiable type on a first subscribe); "+
					"pass an empty array to clear it back to \"all\".",
			),
			mcpgo.Items(map[string]any{"type": "string"}),
		),
		mcpgo.WithString("filterId",
			mcpgo.Description("Filter-watcher UUID, as returned by filter_create or filter_list. Required for filter_delete."),
		),
		mcpgo.WithString("name",
			mcpgo.Description("Filter-watcher name, unique per user, 100 chars max. Required for filter_create."),
		),
		mcpgo.WithString("monitorId",
			mcpgo.Description("Monitor id (UUID) as returned by skein_get_monitor_script. Required for monitor_stop."),
		),
		mcpgo.WithObject("filter",
			mcpgo.Description(
				"Filter-watcher matching rules. Required for filter_create. All "+
					"fields optional, an omitted field means \"no constraint\": "+
					"event_types[] | projects[] | types[] | labels[] | components[] | priority_max.",
			),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, watcherActions.with(map[string]server.ToolHandlerFunc{
		"watch":           handleWatch(pool, notifStore),
		"unwatch":         handleUnwatch(pool, notifStore),
		"project_watch":   handleProjectWatch(pool, projectsStore, notifStore, false),
		"project_unwatch": handleProjectWatch(pool, projectsStore, notifStore, true),
		"filter_create":   handleFilterWatchCreate(pool, notifStore),
		"filter_delete":   handleFilterWatchDelete(pool, notifStore),
		"filter_list":     handleFilterWatchList(pool, notifStore),
		"list":            handleWatching(pool, notifStore),
		"monitor_stop":    handleMonitorStop(monitorStore),
	}))
}

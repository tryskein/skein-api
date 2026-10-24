package tools

import (
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// remindActions is the action table of skein_remind — the enum of the
// schema, the runtime allowlist and the `allowedActions` an agent gets
// back on a miss, all from one slice (see actionSet).
//
// Order is the life of one reminder: arm it, see what is armed, push it
// out, kill it. The enum renders in this order in tools/list, so it
// doubles as the sequence an agent will most often walk.
var remindActions = actionSet{order: []string{
	"set", "list", "snooze", "dismiss",
}}

// RegisterRemind wires skein_remind — the four sk-62.2 reminder tools
// (skein_remind_set / _list / _snooze / _dismiss) folded into one tool
// with an `action` discriminator (sk-255.3).
//
// The per-action handlers are untouched and still live in their own
// files (handleRemindSet and friends); this file only supplies the schema
// and the dispatch table. That keeps per-action visibility gating,
// store-error mapping and the shared snooze/dismiss targeting rule
// (resolveReminderTarget) out of one long switch.
//
// Schema shape (sk-255.3, forced by the platform): `action` is the only
// required parameter. No field is needed by all four actions — `issueId`
// is required for set but meaningless for list — and Anthropic's API
// rejects a top-level oneOf/allOf/anyOf in input_schema with HTTP 400, so
// per-action required sets cannot be expressed structurally at all. Each
// conditional requirement is therefore stated in prose in the parameter's
// own description.
//
// One soft collision was resolved by keeping a single parameter. `note`
// meant "the reminder's own note" in skein_remind_set and "a note on the
// dismiss event" in skein_remind_dismiss. Those are the same role — free
// text the caller attaches to THIS action — and both land in the same
// column (reminders.last_reason / the event payload), so they stay one
// `note` rather than becoming noteOnSet / noteOnDismiss, which would ask
// an agent to pick between two names for one idea. `reason` on snooze is
// deliberately NOT merged into it: it is the third name for the same
// slot, but renaming it would rewrite a live parameter of an unfolded
// action for cosmetics, and sk-255.4 rewrites the callers anyway.
//
// Annotations are coarsened, as everywhere in this consolidation: `list`
// is a pure read, but MCP annotations are per tool, so the tool declares
// readOnly=false. destructive is TRUE here, inherited from dismiss —
// dismissing deletes the row outright, there is no soft-delete and no
// undo. The "Read-only actions:" line in the description is the
// compensation for a client that filters on readOnlyHint.
func RegisterRemind(
	s *server.MCPServer,
	pool *pgxpool.Pool,
	store *reminders.Store,
	issuesStore *issues.Store,
	projectsStore *projects.Store,
) {
	tool := mcpgo.NewTool(
		"skein_remind",
		mcpgo.WithDescription(
			"Personal reminders on issues — your own alarm "+
				"clock, invisible to everyone else. When `remindAt` passes, the "+
				"scheduler pings you with a bell notification and the issue "+
				"shows up in your ringing-reminders lens; state is derived from "+
				"remindAt, armed (future) vs ringing (due), and a past remindAt "+
				"rings on the next scheduler tick.\n\n"+
				"Multiple reminders per issue are allowed, so snooze and dismiss "+
				"need a target: pass `reminderId` (from set or list), or "+
				"`issueId` when you have exactly one active reminder on that "+
				"issue — with more than one the error lists the candidates and "+
				"you retry with reminderId. Read-only actions: list.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(remindActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"set\" → arm a new reminder on an issue at `remindAt`; each call creates a distinct one.\n"+
					"  - \"list\" → your reminders across projects, most urgent first, plus the ringing-task badge count.\n"+
					"  - \"snooze\" → push a reminder's ring time forward (`by` or `until`) so it rings again.\n"+
					"  - \"dismiss\" → delete a reminder for good; it will never ring again.",
			),
		),
		mcpgo.WithString("issueId",
			mcpgo.Description("Full hierarchical issue id, e.g. \"sk-62\" or \"demo-42\". "+
				"Required for set — the issue the reminder is set on. On snooze and "+
				"dismiss it is the alternative to `reminderId`: it targets your single "+
				"active reminder on that issue, and errors with the candidate list when "+
				"there is more than one."),
		),
		mcpgo.WithString("remindAt",
			mcpgo.Description("RFC3339 timestamp at which the reminder should ring, e.g. "+
				"2026-06-01T09:00:00Z (stored UTC). Required for set."),
		),
		mcpgo.WithString("reminderId",
			mcpgo.Description("Reminder UUID, as returned by set or list. The precise target for "+
				"snooze and dismiss; supply it instead of issueId."),
		),
		mcpgo.WithString("by",
			mcpgo.Description("Only read by snooze: relative offset from now — 1h | 1d | 3d | 5d | 1w "+
				"(or the general <n><unit>, unit h|d|w). Exactly one of by / until."),
		),
		mcpgo.WithString("until",
			mcpgo.Description("Only read by snooze: absolute RFC3339 instant to snooze to. "+
				"Exactly one of by / until."),
		),
		mcpgo.WithString("reason",
			mcpgo.Description("Only read by snooze: optional free-form note, recorded as the "+
				"reminder's last_reason."),
		),
		mcpgo.WithString("note",
			mcpgo.Description("Optional free-form text attached to this action: on set it is the new "+
				"reminder's own note (recorded as last_reason), on dismiss it is recorded "+
				"on the dismiss event."),
		),
		mcpgo.WithString("state",
			mcpgo.Description("Only read by list: filter — ringing | armed | all (default all)."),
		),
		mcpgo.WithString("project",
			mcpgo.Description("Only read by list: project handle (alias, name, or UUID) narrowing the "+
				"returned list; `ringingTaskCount` stays the full cross-project badge."),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, remindActions.with(map[string]server.ToolHandlerFunc{
		"set":     handleRemindSet(pool, store, issuesStore),
		"list":    handleRemindList(pool, store, projectsStore),
		"snooze":  handleRemindSnooze(pool, store),
		"dismiss": handleRemindDismiss(pool, store),
	}))
}

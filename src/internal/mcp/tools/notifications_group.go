package tools

import (
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/notifications"
)

// notificationActions is the action table of skein_notifications — the
// enum of the schema, the runtime allowlist and the `allowedActions` an
// agent gets back on a miss, all from one slice (see actionSet).
//
// Order is read first, then the four ways to clear unread state widening
// left to right (one row → one issue → one Watching card, the issue's
// subtree events carded to it included → everything), then the one action
// that moves a row out of the inbox entirely. The enum renders in this
// order in tools/list, so the ordering is also the warning: the further
// right you go, the more rows you touch.
//
// mark_read / mark_read_issue / mark_read_all are three actions rather
// than one because the folded skein_mark_read expressed exactly this
// choice as a presence-based one-of over notificationId / issueId /
// all=true. A one-of cannot be declared in the schema at all (Anthropic
// rejects a top-level oneOf/allOf/anyOf with HTTP 400), so it lived only
// in prose and in a runtime validator, and every wrong combination cost
// the caller a round trip. Flattening it into the discriminator the agent
// must choose anyway makes the schema state the rule instead of the error
// message.
var notificationActions = actionSet{order: []string{
	"inbox", "mark_read", "mark_read_issue", "mark_read_card", "mark_read_all", "archive",
}}

// RegisterNotifications wires skein_notifications — the three R3.3
// notification tools (skein_inbox / skein_mark_read /
// skein_archive_notification) folded into one tool with an `action`
// discriminator (sk-255.3).
//
// The inbox and archive handlers are untouched and still live in their
// own files; only skein_mark_read's internal one-of was actually cut
// apart, into handleMarkReadOne / handleMarkReadIssue / handleMarkReadAll
// (see mark_read.go for what each branch kept). That is the second and
// last handler split of the consolidation, after skein_filter_watch's
// `op`.
//
// Schema shape (sk-255.3, forced by the platform): `action` is the only
// required parameter — `inbox` needs no arguments at all, so nothing else
// can be required for every action — and each conditional requirement is
// stated in prose in the parameter's own description.
//
// One rename was forced. The folded tools disagreed on the name of the
// notification id: `notificationId` in skein_mark_read, bare `id` in
// skein_archive_notification, same bigserial column in both. The
// consolidated tool takes `notificationId` everywhere; `id` would have
// been the vaguer of the two names in a schema that also carries
// `issueId` and `projectId`.
//
// `projectId` is shared rather than split: on inbox it filters the
// listing, on mark_read_all it narrows the sweep. Same type, same
// referent, and both mean "restrict this to one project" — one parameter
// with one sentence covering both uses.
//
// Annotations are coarsened, as everywhere in this consolidation: `inbox`
// is a pure read, but MCP annotations are per tool, so the tool declares
// readOnly=false. destructive stays false — nothing is deleted, read_at
// and archived_at are flags on rows that remain — and idempotent stays
// TRUE, which is honest here rather than coarsened: re-running any of the
// six changes nothing the first run did not already do. The "Read-only
// actions:" line in the description is the compensation for a client that
// filters on readOnlyHint.
func RegisterNotifications(s *server.MCPServer, pool *pgxpool.Pool, notifStore *notifications.Store) {
	tool := mcpgo.NewTool(
		"skein_notifications",
		mcpgo.WithDescription(
			"Your notification inbox — the bell feed of what happened on the "+
				"issues and projects you watch. Everything here is per-user: "+
				"you only ever see and change your own rows. By default a row "+
				"is a summary, not the change itself: long old/new values are "+
				"cut to fit the row budget and marked `valuesTruncated` when "+
				"they were (see below for where the full value lives).\n\n"+
				"action=\"inbox\" reads the feed (scope unread | all | archived, "+
				"cursor-paginated); its first page of unread / all also lists your pending "+
				"space `invitations` (no token or link — a person accepts them in the web UI). "+
				"Only rows of projects you can access now are shown. Four actions clear unread state at four "+
				"widths — mark_read for one row, mark_read_issue for everything "+
				"on one issue including its comments, mark_read_card for "+
				"everything on one Watching card (the issue's own notifications "+
				"plus its descendants' when you watch it with its subtree — "+
				"inside and outside the Watching window; answers "+
				"{\"marked\": N}), mark_read_all for the "+
				"whole backlog (optionally scoped to one project). archive moves "+
				"one row out of the inbox into the archive view; read it back "+
				"with inbox + scope=\"archived\". Read-only actions: inbox.\n\n"+
				"Retention: notifications are kept NOTIFICATIONS_RETENTION_DAYS "+
				"days (90 by default). A background sweep, at least once an "+
				"hour, deletes a row that is not a mention and is older than "+
				"that window, read or not, archived or not — EXCEPT a row whose "+
				"web push is still owed (queued or being delivered): it stays, "+
				"and is visible in the feed, until the delivery is over. A "+
				"mention is never deleted, whatever its age.\n\n"+
				"What an inbox row does NOT carry in full: on a field-change "+
				"notification the `oldValue` / `newValue` pair is cut to fit "+
				"the row's size budget, and a row whose values were actually "+
				"cut carries `valuesTruncated: true`. A row without that key "+
				"was not cut, so the marker means «there is more», never «this "+
				"might be all». The two halves of the pair then differ, and the "+
				"difference decides whether reading on is possible at all. "+
				"`newValue` is the entity's CURRENT state, so the route is the "+
				"entity the row points at: read the issue with skein_show, or, "+
				"for a comment notification, that comment with "+
				"skein_comment_get(commentId). `oldValue` has NO route: the "+
				"previous value is not on the card (it was overwritten) and no "+
				"MCP tool returns it — skein_show(includeEvents:true) names the "+
				"field that changed and who changed it, not what it held "+
				"before. On that half the truncated preview is all there is; "+
				"when you need the whole of a long old value, open the issue's "+
				"activity in the web UI. The row's `summary` "+
				"also has the entity reference removed where the row already "+
				"names the entity, so the same id is not carried twice.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(notificationActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"inbox\" → list your notifications; `scope` picks unread (default), all or archived.\n"+
					"  - \"mark_read\" → mark the ONE notification named by `notificationId` as read.\n"+
					"  - \"mark_read_issue\" → mark every notification tied to `issueId` read, comments on it included.\n"+
					"  - \"mark_read_card\" → mark every notification on the Watching card `issueId` read: the notifications whose card is that issue, its descendants' events included when you watch it with its subtree.\n"+
					"  - \"mark_read_all\" → mark every unread notification read; `projectId` narrows the sweep to one project.\n"+
					"  - \"archive\" → move the notification named by `notificationId` out of the inbox (sets archived_at and read_at).",
			),
		),
		mcpgo.WithNumber("notificationId",
			mcpgo.Description("Notification id (positive integer), as returned by inbox. "+
				"Required for mark_read and archive. Rejected by mark_read_issue, "+
				"mark_read_card and mark_read_all, which act on more than one row by definition."),
		),
		mcpgo.WithString("issueId",
			mcpgo.Description("Full hierarchical issue id, e.g. \"demo-42\" or \"demo-42.1\". "+
				"Required for mark_read_issue and mark_read_card."),
		),
		mcpgo.WithString("projectId",
			mcpgo.Description("Project UUID. On inbox it narrows the listing to that project; on "+
				"mark_read_all it narrows the sweep. Optional in both — omit for every project."),
		),
		mcpgo.WithString("scope",
			mcpgo.Description("Only read by inbox: which rows to return — unread (default) | all | archived."),
			mcpgo.Enum("unread", "all", "archived"),
			mcpgo.DefaultString("unread"),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Only read by inbox: opaque cursor from the previous page (nextCursor)."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Only read by inbox: page size 1..100, default 25."),
			mcpgo.DefaultNumber(25),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, notificationActions.with(map[string]server.ToolHandlerFunc{
		"inbox":           handleInbox(notifStore),
		"mark_read":       handleMarkReadOne(pool, notifStore),
		"mark_read_issue": handleMarkReadIssue(pool, notifStore),
		"mark_read_card":  handleMarkReadCard(notifStore),
		"mark_read_all":   handleMarkReadAll(pool, notifStore),
		"archive":         handleArchiveNotification(pool, notifStore),
	}))
}

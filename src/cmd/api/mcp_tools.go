package main

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	mcptools "skein-api/internal/mcp/tools"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/search"
	"skein-api/internal/spaces"
	"skein-api/internal/themes"
	"skein-api/internal/users"
)

// mcpDeps groups the stores and services the tool registrations need.
// Building this once at startup and capturing it in a closure keeps the
// tool wiring surface narrow — main.go does not need to know which tool
// uses which store.
//
// Adding a new tool that needs a fresh dependency goes here first, then
// to the matching Register* call below.
type mcpDeps struct {
	pool          *pgxpool.Pool
	issuesStore   *issues.Store
	depsStore     *dependencies.Store
	projectsStore *projects.Store
	usersStore    *users.Store
	notifStore    *notifications.Store
	searchSvc     *search.Service

	// projectsEmbedder is the optional embedder for the
	// skein_project_list semantic rerank layer. Reuses the same
	// embedder instance as the search service / dependencies store.
	// nil = FTS-only.
	projectsEmbedder projects.QueryEmbedder

	// issuesEmbedder mirrors projectsEmbedder for the cross-project
	// skein_list path. Same underlying *search.Embedder (the concrete
	// type structurally satisfies both interfaces); kept as a separate
	// field so the two interface-typed slots stay strongly typed.
	issuesEmbedder issues.QueryEmbedder

	// R4.2 — attachments deps. s3Access is nil when S3 is
	// disabled; the tools surface attachments_unavailable in that case.
	attachmentsStore   *attachments.Store
	s3Access           attachments.S3Access
	attachmentBucket   string
	attachmentMaxBytes int64
	attachmentURLTTL   time.Duration

	// sk-10.3 — agent-driven theme editing. themesStore is shared with
	// the REST handler family; themesPublisher wraps the same *nats.Conn
	// as the outbox dispatcher (or is a no-op when NATS_URL is unset);
	// inspectCache is the in-memory POST /me/theme/inspect cache.
	themesStore     *themes.Store
	themesPublisher themes.Publisher
	inspectCache    *themes.InspectCache

	// sk-62.2 — reminders. Backs skein_remind (set / snooze / dismiss /
	// list). Shares the same Store type as the REST handler + scheduler.
	remindersStore *reminders.Store

	// sk-142.7 — agent task-monitor setup. monitorCredsStore mints the
	// scoped skm_ tokens skein_get_monitor_script hands runners (shared
	// with the /agent/v1 RequireMonitorToken door + cleanup worker).
	// agentPublicURL is the admin-validated base URL templated into the
	// bootstrap command.
	monitorCredsStore *monitorcreds.Store
	agentPublicURL    string
	// sk-579 — the token renewal window (cfg.MonitorTokenIdleTTL) quoted in
	// the skein_get_monitor_script description.
	monitorTokenIdleTTL time.Duration
	// sk-674.18 T024 — the spaces service (skein_user action space_add,
	// the space of skein_project_create) and SKEIN_PROJECT_DEFAULT_VISIBILITY.
	spacesSvc                *spaces.Service
	projectDefaultVisibility string
}

// mcpRegisterToolsFn returns a closure that wires every R2.2a read tool.
// Kept as a function-returning-function so main.go can build mcpDeps
// once and hand it to mcppkg.Setup as a plain `func(*server.MCPServer)`.
// The pool is already captured on d.pool — the callback does not take
// a second pool argument (avoids a misleading unused parameter).
//
// R2.1 brought up two tools (skein_whoami, skein_get_instructions);
// R2.2a adds seven read-only tools. Subsequent slices (R2.2b/c writes,
// R2.2d composite) will append to this list.
func mcpRegisterToolsFn(d mcpDeps) func(*server.MCPServer) {
	return func(s *server.MCPServer) {
		// R2.1 — identity + onboarding.
		mcptools.RegisterWhoami(s, d.pool)
		mcptools.RegisterGetInstructions(s, d.pool, d.projectsStore)

		// R2.2a — read tools.
		mcptools.RegisterShow(s, d.pool, d.issuesStore, d.depsStore)
		// Sc3 — `crossProject` mode pulls a flat list across the
		// caller's project_members horizon; the FTS+semantic rerank
		// lights up via issuesEmbedder (same *search.Embedder as the
		// projects path; the two interface-typed slots exist only
		// because Go interface→interface assignment isn't structural).
		mcptools.RegisterList(s, d.pool, d.issuesStore, d.projectsStore, d.issuesEmbedder)
		mcptools.RegisterSearch(s, d.pool, d.searchSvc, d.projectsStore)
		mcptools.RegisterReady(s, d.pool, d.depsStore, d.projectsStore)
		mcptools.RegisterProjectList(s, d.projectsStore, d.projectsEmbedder)
		mcptools.RegisterProjectResolve(s, d.projectsStore)

		// R2.2c — components. sk-255.3 folded the four
		// skein_component_* tools into one skein_component with an
		// `action` enum (list / create / update / archive); the
		// per-action handlers are unchanged behind it. Registered on the
		// read side because action="list" is the one an agent is told to
		// call before every skein_create.
		mcptools.RegisterComponent(s, d.pool, d.projectsStore)

		// R2.2b — issue write tools (skein_create / skein_update / skein_comment).
		mcptools.RegisterCreate(s, d.pool, d.issuesStore, d.depsStore, d.projectsStore)
		mcptools.RegisterUpdate(s, d.pool, d.issuesStore)
		mcptools.RegisterComment(s, d.pool, d.issuesStore)
		// sk-57 — comment edit (author-only) + soft-delete (author or
		// project owner/admin). Companions to skein_comment.
		mcptools.RegisterCommentUpdate(s, d.pool, d.issuesStore)
		mcptools.RegisterCommentDelete(s, d.pool, d.issuesStore)
		// sk-242.1 US-4 — read one comment by UUID (tombstone-aware).
		mcptools.RegisterCommentGet(s, d.pool, d.issuesStore)

		// sk-32 — atomic epic-with-children decomposition affordance.
		mcptools.RegisterEpicCreate(s, d.pool, d.issuesStore, d.projectsStore)

		// R2.2c — graph + meta write tools.
		mcptools.RegisterDepAdd(s, d.pool, d.depsStore, d.issuesStore)
		// sk-43 — explicit edge cleanup (inverse of skein_dep_add) and
		// atomic re-parent. skein_dep_add(parent-child) now only ADDS and
		// rejects a 2nd parent with parent_already_set; moving an existing
		// issue to a new parent is exclusively skein_reparent.
		mcptools.RegisterDepRemove(s, d.pool, d.depsStore, d.issuesStore)
		mcptools.RegisterReparent(s, d.pool, d.depsStore, d.issuesStore)
		mcptools.RegisterProjectCreate(s, d.pool, d.projectsStore, d.projectDefaultVisibility)
		mcptools.RegisterProjectUpdate(s, d.pool, d.projectsStore)

		// R2.2d — composite session priming tool.
		mcptools.RegisterPrime(s, d.pool, d.issuesStore, d.depsStore, d.projectsStore)

		// sk-14 — `skein_move` honest signpost. Pure read tool that
		// returns a pre-filled kit (source + target components +
		// nextSteps for skein_create / skein_close) so agents that
		// reach for an in-place project change get redirected to the
		// canonical copy+supersedes+close workflow.
		mcptools.RegisterMove(s, d.pool, d.issuesStore, d.projectsStore)

		// R2.3 — concurrency: claim / unclaim / close.
		mcptools.RegisterClaim(s, d.pool, d.issuesStore)
		mcptools.RegisterUnclaim(s, d.pool, d.issuesStore)
		mcptools.RegisterClose(s, d.pool, d.issuesStore)

		// sk-35 — pure additive commit-ref attach. Retroactive
		// counterpart to skein_close's commits[]: when the agent
		// forgot commits[] on the original close (or wants to add
		// a hash that landed later), re-running skein_close would
		// silently overwrite closed_reason — this tool extends the
		// link set without touching status / closed_reason / version.
		mcptools.RegisterCommitsAttach(s, d.pool, d.issuesStore)

		// sk-394 — the removal counterpart. A hash is accepted on its
		// FORM alone (lowercase hex, 7..64) because the server holds no
		// clone to ask, so a fabricated one is written and, until this
		// tool, could never be taken back: no MCP tool, no REST route,
		// only hand-edited SQL. «Accepted on faith» plus «never
		// removable» made one mistyped argument permanent in the
		// evidentiary part of the tracker.
		mcptools.RegisterCommitsDetach(s, d.pool, d.issuesStore)

		// Phase 2 ui-redesign — explicit acceptance ack from the
		// assignee. Sibling to claim/close — single state-flip plus
		// one event + fan-out, no batch variant.
		mcptools.RegisterAccept(s, d.pool, d.issuesStore)

		// sk-34 — Delegated chain opt-out/in. drop appends caller to
		// issues.dismissed_by + runs the explicit unwatch (subscription
		// deleted, opt-out recorded when there was one); resume removes
		// caller from dismissed_by (but does NOT re-arm bell — that's
		// a separate skein_watchers(action="watch")).
		mcptools.RegisterDropDelegated(s, d.pool, d.issuesStore)
		mcptools.RegisterResumeDelegated(s, d.pool, d.issuesStore)

		// R5.3 — batch write tools (close_many / update_many / dep_add_many).
		mcptools.RegisterCloseMany(s, d.pool, d.issuesStore)
		mcptools.RegisterUpdateMany(s, d.pool, d.issuesStore)
		mcptools.RegisterDepAddMany(s, d.pool, d.depsStore, d.issuesStore)

		// R3.1 — admin user management. sk-255.3 folded the three
		// skein_user_* tools into one skein_user with an `action` enum
		// (list / create / update); the per-action handlers are
		// unchanged behind it.
		mcptools.RegisterUser(s, d.usersStore, d.pool, d.spacesSvc)

		// R3.1 — project member management (member-or-above visibility,
		// owner-or-admin write; remove allows self-leave). sk-255.3
		// folded the three skein_project_member_* tools into one
		// skein_project_members with an `action` enum (list / add /
		// remove); `project` is required in all three, so it sits in the
		// schema's `required` array next to `action`.
		mcptools.RegisterProjectMembers(s, d.pool, d.projectsStore, d.usersStore)

		// R3.3 — watchers + inbox MCP tools. All are caller-scoped (no
		// admin gate); the underlying store handles per-user isolation
		// via UNIQUE / WHERE user_id = $1. sk-255.3 folded the five
		// subscription tools (watch / unwatch / project_watch /
		// filter_watch / watching) into one skein_watchers with an
		// `action` enum — including the two discriminators those tools
		// used to carry internally (`op`, `unwatch`), which are now
		// top-level actions.
		mcptools.RegisterWatchers(s, d.pool, d.projectsStore, d.notifStore, d.monitorCredsStore)

		// R3.3 — the inbox itself. sk-255.3 folded skein_inbox,
		// skein_mark_read and skein_archive_notification into one
		// skein_notifications with an `action` enum, and flattened
		// skein_mark_read's presence-based one-of (notificationId /
		// issueId / all=true) into three of those actions: mark_read,
		// mark_read_issue, mark_read_all. sk-622.21 T023 (US-7) added
		// mark_read_card — read one Watching card.
		mcptools.RegisterNotifications(s, d.pool, d.notifStore)

		// R4.2 — attachments. sk-255.3 folded the six skein_attachment_*
		// tools into one skein_attachments with an `action` enum; the
		// per-action handlers are unchanged behind it.
		mcptools.RegisterAttachments(s, mcptools.AttachmentDepsFor(
			d.pool, d.attachmentsStore, d.s3Access, d.attachmentBucket,
			d.attachmentMaxBytes, d.attachmentURLTTL))

		// sk-10.3 / sk-10.6 / sk-10.7 — agent-driven theme editing.
		// sk-255.3 folded the six skein_theme_* tools into one
		// skein_theme with an `action` enum (instructions / describe /
		// fork / preview_variables / set_variables / inspect_clear); the
		// per-action handlers are unchanged behind it.
		//
		// action="instructions" is the canonical theme-editing guide —
		// static embedded markdown, no dependencies, pointed at from
		// skein_get_instructions so the long blob is only loaded on
		// demand. The rest split into two read paths (describe,
		// fork-from-source) and three write paths (set_variables
		// persists, preview_variables broadcasts an ephemeral frame,
		// inspect_clear drops the cached selections).
		mcptools.RegisterTheme(s, d.themesStore, d.themesPublisher, d.inspectCache)

		// sk-62.2 — reminders (alarms). All caller-scoped; the
		// store enforces per-user ownership. set targets an issue;
		// snooze / dismiss target a reminder (by id, or by issueId when
		// the caller has exactly one active reminder on it); list is the
		// "what's ringing / armed" lens. sk-255.3 folded the four
		// skein_remind_* tools into one skein_remind with an `action`
		// enum; the per-action handlers are unchanged behind it.
		mcptools.RegisterRemind(s, d.pool, d.remindersStore, d.issuesStore, d.projectsStore)

		// sk-142.7 — agent task-monitor setup. One call auto-watches the
		// issue (subtree-aware), mints a scoped short-lived skm_ token, and
		// returns a bootstrap command (token in a 0600 file, never in argv).
		mcptools.RegisterGetMonitorScript(s, d.pool, d.notifStore, d.monitorCredsStore, d.agentPublicURL, d.monitorTokenIdleTTL)

		// sk-669 — the monitorStalled response layer (sk-579 US-5,
		// mcptools.MonitorStalledMiddleware) is deliberately NOT registered:
		// the owner disabled it. Keeping a monitor alive is the agent's job
		// (its Monitor tool reports the runner's exit); the layer wrapped
		// every tool and added up to ~8 KB (a bootstrap per item) every
		// 5 minutes per stalled monitor, including other agents' monitors of
		// other projects under the same api key — context with no action for
		// the reader. The middleware, its storage and its unit tests stay;
		// re-enabling it is an owner decision, not a config switch.
	}
}

// Package notifications implements R1.6 — manual watchers (issue +
// project), materialized notifications inbox, fan-out, and per-user
// preferences. The package is a thin wrapper over five tables (see
// migration 000008): issue_watchers, project_watchers, custom_watchers,
// user_notification_prefs, notifications.
//
// Design notes:
//
//   - Fan-out is synchronous, in the same pgx.Tx as the originating event.
//     Latency is minimal (one INSERT … SELECT for both watcher pools)
//     and we get exactly-once delivery for free — no outbox needed in R1.6.
//
//   - Watchers carry an array of "reasons" (creator/assignee/manual, plus
//     the legacy commenter/mention values that are still READ but no longer
//     written — see the constants below). Auto-subscription happens on
//     creating an issue and on being assigned one, and nowhere else:
//     commenting and being mentioned create no subscription at all
//     (US-8 / R-7). DELETE /watch deletes the subscription row and, when
//     there was one, records an opt-out in issue_watch_optouts
//     (sk-622.21, US-1), so re-running auto-add (creator, assignee,
//     reopen) doesn't resurrect the subscription unless the user
//     explicitly clicks Watch again — which removes the opt-out. Every
//     issue_watchers row is a live subscription: the old version's
//     tombstone rows (reasons containing 'unwatched') are forbidden by a
//     CHECK since R2 (sk-622.21 T025, INV-1).
//
//   - The DELIVERY filter is the reason registry (reasons.go) and it is
//     applied AT WRITE TIME: Compose decides, per recipient, which channels
//     a row belongs to, and a row that belongs to neither is not written at
//     all. There is no read-side re-check of it, so a preference changed
//     after the fact does not retroactively hide or reveal rows — which is
//     exactly why an unreadable preferences table has to block the write
//     rather than let a row through "just in case" (see FanOut).
//
//   - user_notification_prefs.notify_event_types is a SEPARATE, older
//     whitelist over event TYPES. It is NO LONGER applied at read time:
//     T014 made feed_channel the single visibility condition, so what a
//     feed query returns is decided once, at write time, by the reason
//     registry. The column and its write-side contract still exist and are
//     still served by the legacy preferences endpoints; nothing reads it
//     back for filtering. It never applied to mentions, which are
//     addressed personally (INV-8).
package notifications

import (
	"time"

	"github.com/google/uuid"

	"skein-api/internal/person"
)

// Subscription reasons — the strings stored in issue_watchers.reasons[].
// They say WHY a subscription exists; they are not notification reasons (that
// closed registry lives in reasons.go under ReasonKey*).
//
// Two of them are no longer WRITTEN as of sk-303.13 / T008: 'commenter' and
// 'mention'. Auto-subscription now happens only for creating an issue and for
// being assigned one, because a subscription belongs to the owner of the API
// key while the comments and the mentions are mostly written by their agents —
// "a person ends up subscribed to everything their agent ever touched" (US-8 / R-7).
// The two constants stay because the values still EXIST in storage until the
// clean-up migration (T018) removes them, and reading data we no longer write
// is a different thing from writing it.
const (
	ReasonCreator  = "creator"
	ReasonAssignee = "assignee"
	// ReasonCommenter — legacy, read-only: no longer written (US-8).
	ReasonCommenter = "commenter"
	// ReasonMention — legacy, read-only: no longer written (US-8 / INV-8).
	ReasonMention = "mention"
	ReasonManual  = "manual"
)

// Watcher scope values for issue_watchers.scope (sk-142.8 / spec §7).
// subtree DOMINATES issue and the upgrade is monotonic — see
// AddIssueWatcherScoped.
const (
	// ScopeIssue subscribes to exactly the one issue.
	ScopeIssue = "issue"
	// ScopeSubtree subscribes to the issue plus every hierarchical
	// descendant by id-prefix (sk-42 → sk-42.1, sk-42.1.3, ...).
	ScopeSubtree = "subtree"
)

// WatcherDTO is the wire shape returned by GET /watchers endpoints. The
// JOIN to users is done in the store so the UI can render rows without
// resolving each userId separately.
type WatcherDTO struct {
	UserID   uuid.UUID `json:"userId"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
	Reasons  []string  `json:"reasons"`
	Since    time.Time `json:"since"`
	// The person marks in the project's space (sk-674.18 T027,
	// spaces/contract.go).
	person.Status
}

// NotificationDTO is the inbox-row shape for GET /notifications. We hydrate
// the actor (event.actor + event.actor_user_id), the entity headline
// (issues.title for issue events, comment-on-issue title for comment
// events), and a human-readable summary string built in formatSummary.
type NotificationDTO struct {
	// ID is the row's identifier and the paging key: pages run in
	// DESCENDING id, and the lowest id of a page is what comes back as
	// nextCursor. On the agent monitor-poll path this is the EVENT id
	// instead (that path reads the audit log, not the inbox), so the two
	// id namespaces are not interchangeable across the two endpoints.
	ID int64 `json:"id"`
	// Type is the event type the row reports, e.g. issue.status_changed,
	// comment.created, comment.mention. The enumerated set is served by
	// GET /api/v1/notification-event-types rather than being frozen here.
	Type string `json:"type"`
	// CreatedAt is when the row was minted for this recipient (UTC).
	CreatedAt time.Time `json:"createdAt"`
	// ReadAt is when the recipient marked the row read; absent while it
	// is unread. Absent and null are the same statement here: the field
	// is omitted, never sent as null.
	ReadAt *time.Time `json:"readAt,omitempty"`
	// ArchivedAt is when the recipient archived the row; absent while it
	// is not archived. An archived row is excluded from every scope but
	// `archived`.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`

	// ActorSessionID carries the originating event's actor_session_id and is
	// populated ONLY on the agent monitor-poll path (PollForMonitor). It lets
	// the monitor runner / agent skip its own echo — an event whose
	// actorSessionId equals the agent's current MCP session (sk-164 US-2,
	// INV-2, reader-side self-skip).
	//
	// `omitempty` is load-bearing: NotificationDTO is shared with the human
	// inbox (ListInbox / me), where this field stays nil and
	// must NOT surface as `actorSessionId:null` — that inert null would change
	// the human inbox JSON and break "human path unchanged" (INV-5 / SC-004;
	// spec Changelog 2026-07-20). Present only when the event carried a
	// non-empty actor_session_id; absent otherwise (nil → omitempty). The
	// reader treats absent ≡ empty session ≡ "not mine".
	ActorSessionID *string `json:"actorSessionId,omitempty"`

	// CommentID / BodyPreview / Truncated are monitor-only enrichment for
	// comment-entity events on the agent poll path (sk-242.1 US-1/2, FR-002,
	// INV-5). Filled ONLY by PollForMonitor; human materialization paths
	// (ListInbox / watching recent / MCP
	// skein_notifications(action="inbox")) leave
	// them nil so omitempty keeps the human JSON byte-stable.
	//
	// Wire (comment.created monitor items):
	//   - commentId: event entity UUID string when parseable; parallel to
	//     Entity (which re-points comment→issue, INV-9).
	//   - bodyPreview: live comments.body snapshot at first delivery, rune-
	//     truncated to BodyPreviewMaxRunes (INV-2). *string so nil = absent
	//     and non-nil "" = present empty string (string omitempty would drop "").
	//   - truncated: non-nil true only when live body exceeded the rune cap;
	//     absent (nil) when ≤ cap, soft-deleted, orphan, or non-comment.
	// Soft-deleted / orphan / unparseable: commentId may still be present;
	// bodyPreview and truncated stay nil (INV-4 — deleted text never on poll).
	CommentID   *string `json:"commentId,omitempty"`
	BodyPreview *string `json:"bodyPreview,omitempty"`
	Truncated   *bool   `json:"truncated,omitempty"`

	// SpaceID is the short id of the space of the row's project (sk-674.18
	// T029, spaces/contract.go: Inbox, Mentions, Watching rows). Absent on
	// the agent monitor-poll path and on a row without a project.
	SpaceID string `json:"spaceId,omitempty"`

	// Actor is who caused the event — resolved in the same query, so a
	// client never has to look a username up per row. A system-caused
	// event has no userId.
	Actor ActorDTO `json:"actor"`
	// Entity is what the row is about, already re-pointed to the thing a
	// client can open: for a comment event this is the ISSUE the comment
	// is on, not the comment.
	Entity EntityDTO `json:"entity"`
	// Summary is the ready-to-render one-line phrase for the row, built
	// server-side so every surface words the same event identically.
	Summary string `json:"summary"`

	// Field, OldValue and NewValue come straight off the events row — for
	// status_changed a client renders "open → in_progress" without
	// parsing the raw payload. Field names WHAT changed; the two values
	// are its before and after, and either may be absent when the event
	// has no such side (a creation has no old value). Their JSON type
	// follows the field, so they are not always strings.
	Field    *string `json:"field,omitempty"`
	OldValue any     `json:"oldValue,omitempty"`
	NewValue any     `json:"newValue,omitempty"`

	// CloseOutcome — the outcome of closing the issue the row is about (sk-489, US-3,
	// the "Notifications" contract). Present on issue.status_changed rows
	// that report a transition to "closed", and only when the outcome is known.
	//
	// This is a field of the short summary in the API response, not text: the UI prints
	// the outcome in the push title and in the feed row from Summary, and from here it is taken
	// by whoever needs a machine-distinguishable value rather than a phrase.
	//
	// For issue.outcome_changed the outcome change is carried by the regular Field / OldValue /
	// NewValue (field = "closeOutcome"), and duplicating it here is pointless —
	// "was → became" is a pair, not a single value.
	//
	// omitempty is mandatory: the field is empty on the vast majority of rows
	// (comments, mentions, assignments), and an inert `closeOutcome:null`
	// would change the JSON of the inbox on every one of them.
	CloseOutcome *string `json:"closeOutcome,omitempty"`

	// MentionContext is a ~120-character snippet of the body where the
	// mention happened (sk-38). Pre-trimmed around the first
	// `@<caller-username>` occurrence so the UI can render a meaningful
	// preview ("...we should loop in @admin on this before the merge...").
	// Only populated for mention-typed notifications (comment.mention /
	// issue.mention). Wire-omitempty — non-mention rows lack the field.
	MentionContext string `json:"mentionContext,omitempty"`
}

// ActorDTO is the resolved actor (with optional userId for human/agent;
// system events have userId = nil).
type ActorDTO struct {
	Username string     `json:"username"`
	UserID   *uuid.UUID `json:"userId,omitempty"`
	Type     string     `json:"type"` // 'human' | 'agent' | 'system'
	// The person marks of the actor in the space of the row's project
	// (sk-674.18 T029, spaces/contract.go); filled on the Inbox and Watching
	// rows.
	person.Status
}

// EntityDTO is the lightweight reference to the row a notification is
// about. ID is the entity's natural id (issue id, comment uuid string);
// Title is the issue's title (for comment notifications we still link the
// issue, not the comment text).
type EntityDTO struct {
	Type            string     `json:"type"`
	ID              string     `json:"id"`
	Title           string     `json:"title,omitempty"`
	ProjectID       *uuid.UUID `json:"projectId,omitempty"`
	ProjectIDPrefix string     `json:"projectIdPrefix,omitempty"`
}

// NotificationListResult is the paginated wire shape for inbox queries.
// Cursor is the lowest id seen in the page (id DESC paging) — pass it
// back as ?cursor= to resume.
type NotificationListResult struct {
	// Items is this page of rows, newest first. Always an array; an empty
	// page is [] rather than null.
	Items []NotificationDTO `json:"items"`
	// NextCursor is the opaque token to pass back as ?cursor= for the
	// following page. Absent on the last page — HasMore is the field to
	// branch on, not the presence of this one.
	NextCursor *string `json:"nextCursor,omitempty"`
	// HasMore says whether another page exists. It is the authority on
	// end-of-feed: a full page with HasMore=false is the end, and a short
	// page is not by itself a promise of one.
	HasMore bool `json:"hasMore"`
}

// CountsDTO is the response of GET /notifications/count. Total counts
// every row in the user's inbox (read + unread, but only non-archived);
// Unread is the strict subset.
type CountsDTO struct {
	Unread int64 `json:"unread"`
	Total  int64 `json:"total"`
}

// (we deliberately do NOT use `omitempty` on NotifyEventTypes — the
// frontend distinguishes `null` (all events) from `[]` (none) and would
// see `undefined` for an omitted field).
//
// PrefsDTO is the wire shape for /me/notification-prefs. NotifyEventTypes
// is nil-able so the UI can distinguish "filter not set" (everything
// passes) from "explicit empty list" (nothing passes).
//
// SuppressAgentActions toggles the agent-action filter in
// notifications.FanOut: when true, the SUBSCRIPTION pools drop this
// recipient whenever the event's actor is an agent — any agent, not only
// one of the recipient's own (sk-335). The role path — assignment,
// acceptance, delegated status, mention, reminder — is never suppressed:
// it addresses the recipient personally, and in a system where almost
// everything is done by agents, silencing it would leave the person with
// no notifications at all. sk-343 sharpened that: a role recipient is now
// exempt from the predicate on the SUBSCRIPTION arms too, so their row can
// belong to a subscriber-audience feed and not only to a personal push.
//
// Default TRUE since sk-343. It became the default precisely because the
// personal signals are now guaranteed to arrive: "quiet by default" is
// only safe once silence cannot swallow something addressed to you.
type PrefsDTO struct {
	NotifyEventTypes     []string `json:"notifyEventTypes"`
	DefaultInboxScope    string   `json:"defaultInboxScope"`
	SuppressAgentActions bool     `json:"suppressAgentActions"`
}

// UpdatePrefsInput is the body shape for PUT /me/notification-prefs.
// Pointer fields distinguish "leave alone" from "set to value" — passing
// `notifyEventTypes: null` resets the filter to "all events".
//
// SuppressAgentActions is a pointer so the handler can distinguish
// "field omitted" (leave existing value alone) from "set to false"
// (re-enable subscription notifications about agent actions).
type UpdatePrefsInput struct {
	NotifyEventTypes  *[]string
	DefaultInboxScope *string
	// SetEventTypesNull is true when the caller explicitly passed
	// notifyEventTypes: null — distinct from omitting the field entirely.
	SetEventTypesNull    bool
	SuppressAgentActions *bool
}

// ----------------- R3.3 — custom watchers -----------------

// CustomWatcherFilter is the structured shape of custom_watchers.filter
// jsonb. Empty/nil arrays disable that axis ("any value passes"); a
// non-nil non-empty array is a strict whitelist. PriorityMax is the
// numeric upper bound on issue priority — nil means "any priority".
//
// Struct tags and the persisted jsonb use snake_case (event_types,
// priority_max, …) so SQL-side path operators in FanOut
// (cw.filter->'event_types') match the stored keys without a marshal
// remapping layer.
type CustomWatcherFilter struct {
	EventTypes  []string `json:"event_types,omitempty"`
	Projects    []string `json:"projects,omitempty"`
	Types       []string `json:"types,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Components  []string `json:"components,omitempty"`
	PriorityMax *int     `json:"priority_max,omitempty"`
}

// CustomWatcherDTO is the wire shape for GET/POST /me/custom-watchers.
// ID is server-assigned on Create.
type CustomWatcherDTO struct {
	ID        uuid.UUID           `json:"id"`
	Name      string              `json:"name"`
	Filter    CustomWatcherFilter `json:"filter"`
	CreatedAt time.Time           `json:"createdAt"`
}

// CreateCustomWatcherInput is the body shape for POST /me/custom-watchers.
// Name is required and unique per user (enforced by UNIQUE(user_id, name)
// on the underlying table).
type CreateCustomWatcherInput struct {
	Name   string              `json:"name"`
	Filter CustomWatcherFilter `json:"filter"`
}

// ProjectWatcherDTO extends WatcherDTO with the per-row event_types
// filter (R3.3). ProjectID is included so the consolidated /me/watchers
// endpoint can render which project this row belongs to without a
// secondary lookup.
type ProjectWatcherDTO struct {
	ProjectID       uuid.UUID `json:"projectId"`
	ProjectName     string    `json:"projectName"`
	ProjectIDPrefix string    `json:"projectIdPrefix"`
	Reasons         []string  `json:"reasons"`
	Since           time.Time `json:"since"`
	// EventTypes mirrors project_watchers.event_types. nil means "all
	// notifiable types"; an explicit list is a strict whitelist.
	EventTypes []string `json:"eventTypes"`
}

// IssueWatcherForUserDTO is the per-row shape returned by
// GET /me/watchers under the "issues" section. Includes the issue's
// title and project prefix so the UI can render rows without follow-up
// lookups.
type IssueWatcherForUserDTO struct {
	IssueID         string    `json:"issueId"`
	IssueTitle      string    `json:"issueTitle"`
	ProjectID       uuid.UUID `json:"projectId"`
	ProjectIDPrefix string    `json:"projectIdPrefix"`
	Reasons         []string  `json:"reasons"`
	Since           time.Time `json:"since"`
}

// AllWatchersDTO is the wire shape for GET /me/watchers — the
// consolidated "what am I subscribed to" view used by the /watching page.
type AllWatchersDTO struct {
	Issues   []IssueWatcherForUserDTO `json:"issues"`
	Projects []ProjectWatcherDTO      `json:"projects"`
	Custom   []CustomWatcherDTO       `json:"custom"`
}

// ----------------- sk-39 — /me/watching feed -----------------

// WatchingRow is one card of the shell's Watching column (sk-622.21,
// US-5). A card is a task, of one of two kinds:
//
//   - a subscribed card — the caller has a live subscription on the task;
//     Reasons says why, Since when the subscription was written;
//   - a historical card — no subscription, but the caller's notifications
//     inside the Watching window carry the task as their card (e.g. after
//     the automatic unwatch on close). Reasons is empty ([]) and Since is
//     null.
//
// `Item` carries the IssueSummary the frontend already knows how to
// render. `UnreadEventsCount` is the caller's unread notifications with
// this card inside the Watching window (WATCHING_FEED_WINDOW_DAYS).
// `RecentEvents` is the in-card timeline, bounded on its own: the last 5
// notifications of the card within 7 days, read or unread (R10).
//
// `Item` is left as `any` here to keep the notifications package free of
// an import cycle on issues.IssueSummary; the handler fills it after
// hydration.
type WatchingRow struct {
	Item              any               `json:"item"`
	Reasons           []string          `json:"reasons"`
	UnreadEventsCount int               `json:"unreadEventsCount"`
	RecentEvents      []NotificationDTO `json:"recentEvents"`
	// Since is when the subscription was written; null on a historical
	// card, which has no subscription.
	Since *time.Time `json:"since"`
	// SpaceID is the short id of the space of the card's issue (sk-674.18
	// T029, spaces/contract.go: Watching rows).
	SpaceID string `json:"spaceId"`
}

// WatchingListResponse is the wire shape for GET /api/v1/me/watching.
// Items are sorted by the time of the card's latest notification inside
// the window, descending, then task id; cards without notifications last.
//
// Items is capped server-side, so summing it is NOT a way to learn the
// caller's unread total; UnreadTotal is that number, complete regardless of
// the cap and the filter, and equal to the header Watching badge (INV-5):
// both count the caller's unread, non-archived notifications that carry a
// Watching card and were written inside the window.
type WatchingListResponse struct {
	Items []WatchingRow `json:"items"`
	// UnreadTotal is the sum of unread notifications over ALL cards, inside
	// the window — the header badge's number. Not capped by Items, not
	// narrowed by State.
	UnreadTotal int `json:"unreadTotal"`
	// SubscriptionsTotal is how many live subscriptions the caller has,
	// uncapped. Historical cards are not subscriptions and are not counted.
	SubscriptionsTotal int `json:"subscriptionsTotal"`
	// Truncated says Items carries fewer cards than StateTotal — the
	// explicit "my number is incomplete" the column owes the reader when
	// it cannot show everything.
	Truncated bool `json:"truncated"`

	// --- sk-551.10 — the Open / Closed / All filter. ---

	// State echoes the filter the items were selected by: "open" (the
	// card's task is not closed), "closed" or "all".
	State WatchingState `json:"state"`
	// StateTotal is how many cards of both kinds match State, uncapped —
	// the number the column prints under its filter.
	StateTotal int `json:"stateTotal"`
	// OpenTotal / ClosedTotal count the cards of BOTH kinds by the status
	// of the card's task (closed = status 'closed'; every other status,
	// standing included, is open). OpenTotal + ClosedTotal is every card.
	// Both are returned whatever State is, so the filter can show the size
	// of the other choices without a second call.
	OpenTotal   int `json:"openTotal"`
	ClosedTotal int `json:"closedTotal"`
	// ClosedAutoTotal is the part of ClosedTotal without a manual
	// subscription, historical cards included — what
	// POST /me/watching/unwatch {"state":"closed"} would take, so the
	// «Stop watching closed · N» button can state its N before the click.
	ClosedAutoTotal int `json:"closedAutoTotal"`
}

// WatchingState is the Open / Closed / All filter of the Watching list
// and of the bulk unwatch (sk-551.10).
type WatchingState string

const (
	// WatchingStateOpen selects subscriptions on tasks that are not closed.
	WatchingStateOpen WatchingState = "open"
	// WatchingStateClosed selects subscriptions on closed tasks.
	WatchingStateClosed WatchingState = "closed"
	// WatchingStateAll selects every live subscription.
	WatchingStateAll WatchingState = "all"
)

// BulkUnwatchInput selects the caller's subscriptions to stop watching
// (sk-551.10). Exactly one selector: IssueIDs (non-nil) or State.
type BulkUnwatchInput struct {
	// IssueIDs is an explicit selection; manual subscriptions in it are
	// removed too. nil = use State.
	IssueIDs []string
	// State is a filter over the status of the watched task.
	State WatchingState
	// IncludeManual lets a State sweep remove manual subscriptions too.
	// Ignored with IssueIDs.
	IncludeManual bool
}

// BulkUnwatchResult reports what a bulk unwatch did. Wire literal
// (sk-622.21 T008, US-6), every key always present:
//
//	{"unwatched":3,"keptManual":1,"historicalRemoved":2}
type BulkUnwatchResult struct {
	// Unwatched is how many subscriptions were stopped.
	Unwatched int `json:"unwatched"`
	// KeptManual is how many subscriptions matched the State filter but
	// were left alone because they are manual (always 0 for IssueIDs or
	// with IncludeManual).
	KeptManual int `json:"keptManual"`
	// HistoricalRemoved is how many HISTORICAL Watching cards the operation
	// removed from the caller's Watching column (sk-622.21, US-6), counted
	// in cards — distinct card issues, not notifications. A historical card
	// is one the caller no longer watches but that still has notifications
	// in the Watching window (e.g. after auto-unwatch on close). Removing it
	// clears the card on all of the caller's notifications for that issue
	// and writes no opt-out (decision R3). Cards of issues that were still
	// watched are reported by Unwatched, never here. 0 when the operation
	// touched no historical card.
	HistoricalRemoved int `json:"historicalRemoved"`
}

// MarkCardReadResult is the response of "read card X" — mark read every
// unread notification of the caller whose Watching card is X, the card's
// descendants' events included, inside and outside the Watching window
// (sk-622.21, US-7). Wire literal (sk-622.21 T008):
//
//	{"marked":2}
//
// The operation is idempotent: a card that is someone else's, does not
// exist or has nothing unread answers 200 with zero, never an error.
type MarkCardReadResult struct {
	// Marked is how many notifications this call flipped from unread to
	// read, counted in notification rows. Rows already read are not
	// counted, so a repeated call answers 0. It may exceed the drop of the
	// Watching badge, which counts only unread rows inside the window.
	Marked int `json:"marked"`
}

// ParseWatchingState maps a wire value onto WatchingState. ok=false for
// anything outside the closed set — the caller answers 400, it is a
// malformed request rather than a business decision.
func ParseWatchingState(v string) (WatchingState, bool) {
	switch WatchingState(v) {
	case WatchingStateOpen, WatchingStateClosed, WatchingStateAll:
		return WatchingState(v), true
	}
	return "", false
}

// WatchingBase is the intermediate projection produced by the SQL —
// issue_id + reasons + counts + since. The handler hydrates IssueSummary
// + RecentEvents into the final WatchingRow.
type WatchingBase struct {
	IssueID string
	// Reasons of the subscription; empty on a historical card.
	Reasons           []string
	UnreadEventsCount int
	// Since of the subscription; nil on a historical card.
	Since        *time.Time
	LastActivity *time.Time
	// SpaceID is the short id of the space of the card's issue (T029).
	SpaceID string
}

// WatchingFeedResult is what ListWatchingFeed returns: the (capped) card
// rows plus the uncapped totals the response has to publish so a truncated
// column can say so instead of quietly under-reporting.
type WatchingFeedResult struct {
	Rows []WatchingBase
	// UnreadTotal counts every unread notification with a card inside the
	// window, cap or no cap — the badge's number (INV-5).
	UnreadTotal int
	// SubscriptionsTotal counts every live subscription, cap or no cap.
	SubscriptionsTotal int
	// Truncated is StateTotal > len(Rows).
	Truncated bool
	// StateTotal counts the cards of both kinds matching the requested
	// state; OpenTotal / ClosedTotal count the cards of both kinds by the
	// status of the card's task; ClosedAutoTotal the closed ones without a
	// manual subscription, historical ones included.
	StateTotal      int
	OpenTotal       int
	ClosedTotal     int
	ClosedAutoTotal int
}

// ----------------- sk-303.13 / C1 — reason registry and channel settings -----------------

// ReasonChannelDTO is one form cell: one channel of one reason for one
// user. Applicable and Audience come from the registry (the same for
// everyone), Enabled is the particular user's state.
//
// Applicable=false is NOT a switched-off toggle: the reason has no such
// channel, the form draws a dash with an explanation and it cannot be
// switched on. That is why applicable and enabled are two fields, not one.
type ReasonChannelDTO struct {
	// Applicable — whether this channel exists for the reason at all.
	Applicable bool `json:"applicable"`
	// Audience — whom this channel reaches; empty on an inapplicable channel.
	// The audiences of the two channels of one reason legitimately differ,
	// so the field lives on the channel, not on the reason.
	Audience string `json:"audience,omitempty"`
	// Enabled — the user's state. Always false on an inapplicable channel.
	Enabled bool `json:"enabled"`
}

// ReasonRowDTO is one settings-form row: a reason and its two channels.
type ReasonRowDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Hint is the caption under the name. For a reason whose channel
	// audiences differ it MUST explain that and must not promise
	// "delegated only" for the feed.
	Hint string           `json:"hint,omitempty"`
	Feed ReasonChannelDTO `json:"feed"`
	Push ReasonChannelDTO `json:"push"`
}

// ReasonPrefsDTO is the body of the notification-settings form response
// (contract C1). It contains ONLY active reasons: reserved ones exist in
// storage but get no toggle in this wave — a toggle without effect is the
// defect the wave was started to fix.
//
// SuppressAgentActions lives on the page outside the reasons table: it is
// not a reason, it is a cross-cutting filter of the subscription path
// (sk-335 — actions of any agents are suppressed; sk-343 — except events
// addressed to the recipient personally by role, and it is on by default).
// "Default feed slice" is NOT returned here: the server does not read that
// value, and a dead toggle must not be resurrected.
type ReasonPrefsDTO struct {
	Reasons              []ReasonRowDTO `json:"reasons"`
	SuppressAgentActions bool           `json:"suppressAgentActions"`
	// UnwatchOnClose — sk-551.10: when an issue is closed, drop the
	// caller's automatic subscriptions on it (creator / assignee); a
	// manual one stays, and a reopen brings the automatic one back.
	// On by default (sk-622.21, US-2): a user without a prefs row reads as on.
	UnwatchOnClose bool `json:"unwatchOnClose"`
}

// ReasonChannelUpdate is the desired state of the two channels of one reason.
// Pointers distinguish "field omitted" (leave as is) from "switch off".
type ReasonChannelUpdate struct {
	Reason string `json:"reason"`
	Feed   *bool  `json:"feed,omitempty"`
	Push   *bool  `json:"push,omitempty"`
}

// UpdateReasonPrefsInput is the input of a partial settings update. The
// update is partial on purpose: the listed reasons change, all others keep
// their state. A client that does not know about a new reason has no right
// to overwrite it — otherwise shipping a new version of the form would
// reset other people's settings.
type UpdateReasonPrefsInput struct {
	Reasons              []ReasonChannelUpdate
	SuppressAgentActions *bool
	// UnwatchOnClose — nil leaves the stored value alone (sk-551.10).
	UnwatchOnClose *bool
}

// ReasonChannelState is the stored state of one reason's channel pair.
type ReasonChannelState struct {
	Feed bool
	Push bool
}

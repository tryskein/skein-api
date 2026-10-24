// Package signals owns the wire contract of the PERSONAL NATS subjects that
// skein-api publishes to one user: the personal signal
// (`skein.user.{uid}.signal`), the personal reminder frame
// (`skein.user.{uid}.reminder`) and the access-changed control event
// (`skein.user.{uid}.access_changed`, sk-674.18). The consumer is skein-web, which turns them
// into SignalR frames for that user's connections (spec sk-622.21, US-11).
//
// Treat every name in this file as a stable contract between repositories:
// additive changes only — never rename or remove a subject, a field or an
// enumerated value.
//
// # Personal signal
//
// Subject `skein.user.{uid}.signal`, body [Signal]:
//
//	{"areas":["inbox","watching"],"sentAt":"2026-10-09T10:15:03.123456Z"}
//
// Semantics:
//
//   - It carries NO data. Only the list of personal areas whose header
//     counters or personal-screen contents may have changed for that user;
//     the receiver re-reads those areas through the ordinary REST reads.
//   - It is published only AFTER the change is committed (INV-12): a rolled
//     back or retried SERIALIZABLE attempt publishes nothing.
//   - It is published regardless of whether a notification row was written
//     (own actions, agent actions and the recipient's notification settings
//     suppress notifications, never the signal).
//   - Delivery is best-effort (NATS Core, no outbox). A lost signal is
//     covered by the 60 s safety poll of the tab (US-14), the "re-read"
//     frame after a transport break (US-10) and refresh on focus /
//     visibility / navigation.
//   - The publisher does NOT coalesce across transactions: one message per
//     user per committed transaction, with the union of that transaction's
//     areas. Coalescing into at most one frame per connection per
//     COUNTS_SIGNAL_WINDOW_MS is skein-web's job, per connection, on every
//     replica (decisions K9 / R15). skein-web therefore subscribes to
//     [SignalSubjectPattern] WITHOUT a queue group; its queue-group bridge
//     on `skein.user.>` must not handle these subjects.
//
// # Personal reminder frame
//
// Subject `skein.user.{owner}.reminder`, body [ReminderFrame]:
//
//	{"reminderId":"0199…","issueId":"sk-42","projectId":"0199…","kind":"rang",
//	 "remindAt":"2026-10-09T10:00:00Z","firingId":"0199…@2026-10-09T10:00:00Z"}
//
// Semantics:
//
//   - Addressed to the reminder's owner only, independently of which projects
//     the owner's tabs declared, so the bell rings in a tab where the issue's
//     project is not declared.
//   - Published by the outbox dispatcher when it publishes a `reminder.*`
//     event (decision K14) — so it is after commit by construction and
//     at-least-once: the same frame may arrive twice, and the receiver
//     deduplicates the ring on [ReminderFrame.FiringID].
//   - Reminder frames are never coalesced.
//
// # Area `spaces` (sk-674.18, K-18, US-19)
//
// The area `spaces` says "re-read my spaces, my projects, my invitations and
// the counters". It is the only area whose signal carries a payload: the
// field `spaces`, one entry per space-level event of the transaction for this
// user, so the receiver can show the right toast ("you were added to Acme",
// "you are the owner of Backend") and leave a space page that no longer
// exists. Signals without the `spaces` area never carry the field — the wire
// of the seven older areas is unchanged:
//
//	{"areas":["spaces","inbox"],
//	 "spaces":[{"reason":"invited","spaceId":"k3x9p2qa","spaceName":"Acme"}],
//	 "sentAt":"2026-10-12T10:15:03.123456Z"}
//
// The entry names the space only by its short id (the `id` of GET /me/spaces)
// and, where the receiver needs it for a toast and may no longer read it, its
// name. No issue or project data. skein-web's coalescer merges the lists of
// several signals without losing an entry (a frame may carry the union).
//
// # Access changed (sk-674.18, K-15, D-10, ADR-3)
//
// Subject `skein.user.{uid}.access_changed`, body [AccessChanged]:
//
//	{"lostProjectIds":["0199…a1"],"gainedProjectIds":[],"lostSpaceIds":["k3x9p2qa"],
//	 "sentAt":"2026-10-12T10:15:03.123456Z"}
//
// A control event for skein-web's hub, not a browser frame: after a
// committed change of the user's access (a membership primitive, a space
// deletion, a visibility change, a role change) it tells which projects the
// user lost and gained. skein-web removes the user's connections from the
// groups of the lost projects AT ONCE, on every replica, without waiting for
// the periodic re-check, and re-checks the user against
// POST /api/v1/hub/project-access for the gained ones. Rules:
//
//   - Published only after the final commit of the change, only to the
//     users whose access changed, with exact sets; a rolled back change
//     publishes nothing (the same discipline as the personal signal).
//   - Every replica must act on it: skein-web subscribes to
//     [AccessChangedSubjectPattern] WITHOUT a queue group; its queue-group
//     bridge on `skein.user.>` must not handle this subject.
//   - Best-effort like every NATS Core subject here: the periodic re-check
//     (30 s, fail-closed at 60 s) remains the safety net.
//   - The three arrays are always present; `[]` when empty. Project ids are
//     UUIDs; space ids are space short ids.
//
// # Review privacy
//
// Neither message reveals private review data of any user: the signal holds
// area names only (the `unreviewed` area says "re-read your own list", not
// which issue changed), and the reminder frame goes to the reminder's owner
// and holds no review field. A future additive field must keep both
// properties.
package signals

import (
	"time"

	"github.com/google/uuid"
)

// Area is one personal area named by a [Signal]. The value set is closed and
// is the literal set of spec sk-622.21 US-11.
type Area string

const (
	// AreaInbox — the Inbox header badge and the Inbox queue.
	AreaInbox Area = "inbox"
	// AreaMentions — the Mentions header badge and the Mentions queue.
	AreaMentions Area = "mentions"
	// AreaWatching — the Watching header badge and the Watching column.
	AreaWatching Area = "watching"
	// AreaLens — the "on me" / "delegated" lenses: in the header, in the
	// tasks column and in the projects column.
	AreaLens Area = "lens"
	// AreaUnreviewed — the author's unreviewed counter, lists filtered by
	// "unreviewed" and the private review fields of an open issue card.
	AreaUnreviewed Area = "unreviewed"
	// AreaTasks — the contents of the personal task lists: "on me",
	// "delegated", "created by me".
	AreaTasks Area = "tasks"
	// AreaProjects — the project list and the user's memberships.
	AreaProjects Area = "projects"
	// AreaSpaces — the user's spaces, their projects, invitations and
	// counters (sk-674.18, K-18). The only area with a payload:
	// [Signal.Spaces].
	AreaSpaces Area = "spaces"
)

// Areas returns every [Area] in canonical order. A fresh slice is returned
// on each call, so callers may modify it.
func Areas() []Area {
	return []Area{
		AreaInbox,
		AreaMentions,
		AreaWatching,
		AreaLens,
		AreaUnreviewed,
		AreaTasks,
		AreaProjects,
		AreaSpaces,
	}
}

// Valid reports whether a is one of the contract values.
func (a Area) Valid() bool {
	switch a {
	case AreaInbox, AreaMentions, AreaWatching, AreaLens, AreaUnreviewed, AreaTasks, AreaProjects, AreaSpaces:
		return true
	default:
		return false
	}
}

// Signal is the body of `skein.user.{uid}.signal`.
type Signal struct {
	// Areas is the non-empty set of affected areas, each value at most once.
	// It never contains data about an issue or a project.
	Areas []Area `json:"areas"`
	// Spaces are the space-level events of the transaction for this user, in
	// the order they happened. Present (non-empty) only when Areas contains
	// [AreaSpaces]; absent from every other signal.
	Spaces []SpaceEvent `json:"spaces,omitempty"`
	// SentAt is the publish time in UTC (RFC 3339 on the wire). Diagnostic
	// only: receivers must not order or deduplicate signals by it.
	SentAt time.Time `json:"sentAt"`
}

// SpaceReason is why a [SpaceEvent] was sent. The value set is closed and is
// the literal set of spec sk-674.18 US-19 (screens 1.15).
type SpaceReason string

const (
	// SpaceInvited — the user was invited to the space.
	SpaceInvited SpaceReason = "invited"
	// SpaceAccepted — the user's invitation to the space was accepted (sent
	// to whoever invited; the accepting user gets SpaceAdded).
	SpaceAccepted SpaceReason = "accepted"
	// SpaceDeclined — an invitation sent by the user was declined (to the
	// space owner and the sender).
	SpaceDeclined SpaceReason = "declined"
	// SpaceExpired — an invitation expired (to the owner and the sender).
	SpaceExpired SpaceReason = "expired"
	// SpaceRemoved — the user was removed from the space.
	SpaceRemoved SpaceReason = "removed"
	// SpaceAdded — the user became a member (accepted, or added by an admin).
	SpaceAdded SpaceReason = "added"
	// SpaceOwnerAssigned — the user became the owner (created for them by an
	// admin, or an owner change).
	SpaceOwnerAssigned SpaceReason = "owner_assigned"
	// SpaceOwnerChanged — the owner changed (to the previous owner and the
	// members).
	SpaceOwnerChanged SpaceReason = "owner_changed"
	// SpaceDeleted — the space was deleted (to its members).
	SpaceDeleted SpaceReason = "deleted"
	// SpaceRenamed — the space was renamed (to its members).
	SpaceRenamed SpaceReason = "renamed"
)

// SpaceReasons returns every [SpaceReason] in canonical order. A fresh slice
// is returned on each call.
func SpaceReasons() []SpaceReason {
	return []SpaceReason{
		SpaceInvited, SpaceAccepted, SpaceDeclined, SpaceExpired, SpaceRemoved,
		SpaceAdded, SpaceOwnerAssigned, SpaceOwnerChanged, SpaceDeleted, SpaceRenamed,
	}
}

// Valid reports whether r is one of the ten contract values.
func (r SpaceReason) Valid() bool {
	switch r {
	case SpaceInvited, SpaceAccepted, SpaceDeclined, SpaceExpired, SpaceRemoved,
		SpaceAdded, SpaceOwnerAssigned, SpaceOwnerChanged, SpaceDeleted, SpaceRenamed:
		return true
	default:
		return false
	}
}

// SpaceEvent is one entry of [Signal.Spaces].
type SpaceEvent struct {
	// Reason is why the entry was sent; see [SpaceReason].
	Reason SpaceReason `json:"reason"`
	// SpaceID is the space short id.
	SpaceID string `json:"spaceId"`
	// SpaceName is the space name at the time of the event, for the toast:
	// present for invited, added, owner_assigned, removed, deleted and
	// renamed (the new name), where the receiver may not be able to read it
	// any more or has not read it yet; absent otherwise.
	SpaceName string `json:"spaceName,omitempty"`
}

// AccessChanged is the body of `skein.user.{uid}.access_changed`.
type AccessChanged struct {
	// LostProjectIDs are the projects the user can no longer see. Always
	// present; `[]` when none.
	LostProjectIDs []uuid.UUID `json:"lostProjectIds"`
	// GainedProjectIDs are the projects the user can now see. Always
	// present; `[]` when none.
	GainedProjectIDs []uuid.UUID `json:"gainedProjectIds"`
	// LostSpaceIDs are the spaces (short ids) the user is no longer in or can
	// no longer see. Always present; `[]` when none.
	LostSpaceIDs []string `json:"lostSpaceIds"`
	// SentAt is the publish time in UTC (RFC 3339). Diagnostic only.
	SentAt time.Time `json:"sentAt"`
}

// ReminderKind is the reminder lifecycle step a [ReminderFrame] reports. It
// mirrors the `reminder.*` event that produced the frame.
type ReminderKind string

const (
	// ReminderSet — a reminder was created or re-armed (`reminder.set`).
	ReminderSet ReminderKind = "set"
	// ReminderSnoozed — a reminder was postponed (`reminder.snoozed`).
	ReminderSnoozed ReminderKind = "snoozed"
	// ReminderRang — a due reminder fired (`reminder.rang`); the only kind
	// that rings the bell.
	ReminderRang ReminderKind = "rang"
	// ReminderDismissed — a reminder was cancelled (`reminder.dismissed`).
	ReminderDismissed ReminderKind = "dismissed"
)

// Valid reports whether k is one of the four contract values.
func (k ReminderKind) Valid() bool {
	switch k {
	case ReminderSet, ReminderSnoozed, ReminderRang, ReminderDismissed:
		return true
	default:
		return false
	}
}

// ReminderFrame is the body of `skein.user.{owner}.reminder`.
type ReminderFrame struct {
	// ReminderID is the reminder's id.
	ReminderID uuid.UUID `json:"reminderId"`
	// IssueID is the full hierarchical id of the reminded issue (e.g. "sk-42").
	IssueID string `json:"issueId"`
	// ProjectID is the issue's project.
	ProjectID uuid.UUID `json:"projectId"`
	// Kind is the lifecycle step; see [ReminderKind].
	Kind ReminderKind `json:"kind"`
	// RemindAt is the reminder's due time in UTC after this step.
	RemindAt time.Time `json:"remindAt"`
	// FiringID identifies one firing instance of the reminder and is the
	// receiver's deduplication key for the bell; built by [FiringID].
	FiringID string `json:"firingId"`
}

// SignalSubjectPattern is the NATS wildcard matching every personal signal
// subject. skein-web subscribes to it without a queue group (K9).
const SignalSubjectPattern = "skein.user.*.signal"

// ReminderSubjectPattern is the NATS wildcard matching every personal
// reminder-frame subject.
const ReminderSubjectPattern = "skein.user.*.reminder"

// AccessChangedSubjectPattern is the NATS wildcard matching every
// access-changed subject. skein-web subscribes to it without a queue group:
// every replica drops its own connections.
const AccessChangedSubjectPattern = "skein.user.*.access_changed"

// SignalSubject returns the personal signal subject of user uid.
func SignalSubject(uid uuid.UUID) string {
	return "skein.user." + uid.String() + ".signal"
}

// ReminderSubject returns the personal reminder-frame subject of owner.
func ReminderSubject(owner uuid.UUID) string {
	return "skein.user." + owner.String() + ".reminder"
}

// AccessChangedSubject returns the access-changed subject of user uid.
func AccessChangedSubject(uid uuid.UUID) string {
	return "skein.user." + uid.String() + ".access_changed"
}

// FiringID builds the firing-instance key `{reminderId}@{remindAt}`, where
// remindAt is rendered in UTC as RFC 3339 with the fractional seconds the
// value actually has (time.RFC3339Nano). The same reminder due at the same
// instant always yields the same key; a snooze moves remindAt and therefore
// yields a new firing instance.
func FiringID(reminderID uuid.UUID, remindAt time.Time) string {
	return reminderID.String() + "@" + remindAt.UTC().Format(time.RFC3339Nano)
}

// Package reminders implements sk-62.2 — per-user alarms
// hung on issues. A reminder is a row in the reminders table owned by a
// single user; its state (armed vs ringing) is DERIVED from remind_at,
// never stored. The scheduler worker (scheduler.go) materialises a bell
// notification when a reminder comes due, reusing the existing events +
// notifications + outbox fan-out path so the live feed lights up without
// a new transport.
//
// Lifecycle:
//
//	set      → INSERT a row (remind_at in the future) — emits reminder.set
//	ring     → scheduler: remind_at <= now() and un-notified → notify the
//	           owner, stamp notified_at, emit reminder.rang
//	snooze   → push remind_at forward (now + by, or an explicit until),
//	           reset notified_at, bump snooze_count — emits reminder.snoozed
//	dismiss  → DELETE the row — emits reminder.dismissed
//
// State is derived: armed = remind_at > now; ringing = remind_at <= now.
package reminders

import (
	"time"

	"github.com/google/uuid"
)

// State is the derived lifecycle of a reminder. It is computed from
// remind_at at read time, never persisted.
type State string

const (
	// StateArmed — remind_at is in the future; the reminder is waiting.
	StateArmed State = "armed"
	// StateRinging — remind_at has passed; the reminder is due (the
	// scheduler has pinged, or is about to ping, the owner).
	StateRinging State = "ringing"
)

// DeriveState returns the derived state for a remind_at relative to now.
// now is passed explicitly so callers (and tests) control the clock.
func DeriveState(remindAt, now time.Time) State {
	if remindAt.After(now) {
		return StateArmed
	}
	return StateRinging
}

// ListFilter narrows ListForUser to a derived-state subset.
type ListFilter string

const (
	// FilterRinging — only reminders that are due now (remind_at <= now).
	FilterRinging ListFilter = "ringing"
	// FilterArmed — only reminders waiting to ring (remind_at > now).
	FilterArmed ListFilter = "armed"
	// FilterAll — every reminder owned by the caller, ringing first.
	FilterAll ListFilter = "all"
)

// ParseListFilter validates a free-form state string. Empty defaults to
// FilterAll. Returns ok=false for anything else so the handler can emit a
// 400 / validation_error.
func ParseListFilter(s string) (ListFilter, bool) {
	switch s {
	case "":
		return FilterAll, true
	case string(FilterRinging):
		return FilterRinging, true
	case string(FilterArmed):
		return FilterArmed, true
	case string(FilterAll):
		return FilterAll, true
	default:
		return "", false
	}
}

// Reminder is the wire + storage shape of a reminder joined to a slim
// summary of its issue. JSON tags match the camelCase convention used by
// the rest of the API surface. IssueTitle / IssueStatus / ProjectID are
// denormalised from the issues row on read so the frontend can render the
// list (and badge) without a second round-trip.
type Reminder struct {
	ID          uuid.UUID  `json:"id"`
	IssueID     string     `json:"issueId"`
	UserID      uuid.UUID  `json:"userId"`
	RemindAt    time.Time  `json:"remindAt"`
	NotifiedAt  *time.Time `json:"notifiedAt,omitempty"`
	SnoozeCount int        `json:"snoozeCount"`
	LastReason  *string    `json:"lastReason,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`

	// State is the derived armed/ringing value, computed at read time.
	State State `json:"state"`

	// Issue summary — denormalised join fields. ProjectID lets the
	// frontend route to the issue without resolving the id prefix.
	IssueTitle    string    `json:"issueTitle"`
	IssueStatus   string    `json:"issueStatus"`
	ProjectID     uuid.UUID `json:"projectId"`
	IssueType     string    `json:"issueType"`
	IssuePriority int       `json:"issuePriority"`
	// SpaceID is the short id of the space of the issue's project
	// (sk-674.18 T029, spaces/contract.go).
	SpaceID string `json:"spaceId"`
}

// ListResponse is the GET /reminders payload: the reminder list plus the
// badge count (distinct ringing issues) so the frontend gets both the
// list and the tab-badge number in one call.
type ListResponse struct {
	Reminders        []Reminder `json:"reminders"`
	RingingTaskCount int        `json:"ringingTaskCount"`
}

// Actor carries the audit-trail identity for a reminder mutation. Mirrors
// issues.Actor / attachments.Actor to keep the event-writer signature
// aligned without an import cycle.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string // 'human' | 'agent' | 'system'
	SessionID string
}

package outbox

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/signals"
)

// SubjectInput is the small piece of an event row needed to compute its
// NATS subject. It is built from the events row at dispatch time and fed
// into BuildSubject. Keeping the type narrow lets the subject computation
// stay a pure function (no DB / network IO), which is important: tests
// can exercise every subject shape without spinning up a database, and
// any subscriber that wants to mirror the scheme (skein-web Hub, future
// integrations) only needs to mirror this struct.
type SubjectInput struct {
	// EventType is the value stored in events.type — e.g. "issue.created",
	// "issue.updated", "issue.field_changed", "user.created". The first
	// segment before "." is the entity short-name; the rest is the action.
	EventType string

	// EntityType is events.entity_type — one of issue, project, comment,
	// dep, attachment, component, user. Only "user" is treated specially
	// (user-global subjects); the rest go on project-scoped subjects.
	EntityType string

	// EntityID is the row's primary key in string form. For issues this is
	// the hierarchical id ("demo-42.1"); for everything else it is a uuid
	// stringification. Used as the trailing token in project-scoped
	// subjects so subscribers can wildcard-match a specific entity.
	EntityID string

	// ProjectID is required for every entity except "user". When nil for a
	// non-user entity, BuildSubject returns a "skein.events.malformed.*"
	// subject — the dispatcher will still publish so the event is not
	// silently dropped, but operators alerting on the malformed root will
	// see it.
	ProjectID *uuid.UUID

	// Field is events.field — set on field_changed events to identify
	// which scalar changed. Translated to kebab-case for the subject path
	// (acceptanceCriteria → acceptance-criteria) so the NATS scheme
	// follows pub/sub naming conventions even though the Go/JSON payload
	// stays camelCase.
	Field string
}

// BuildSubject computes the NATS subject for an event row.
//
// Schema (R3.2 plan, finalized):
//
//	skein.events.project.{pid}.{entity}.{action}.{eid}      project-scoped lifecycle / parent
//	skein.events.project.{pid}.{entity}.field.{kebab}.{eid} project-scoped per-field child
//	skein.user.{uid}.{action}                               user-global lifecycle
//	skein.user.{uid}.field.{kebab}                          user-global per-field child
//	skein.user.{uid}.membership_changed                     side-channel (synthetic)
//	skein.user.{uid}.notification                           reserved for R3.3
//
// "Entity" in the subject is the short-name from EventType's first segment
// (e.g. "issue.created" → "issue"). EntityType from the row is only
// consulted to detect the "user" case, because the events table uses the
// short token "dep" for dependency.* events and the subject scheme prefers
// the event-type token to match the action.
//
// BuildSubject is pure — no DB, no network, no time. It is safe to call
// from any goroutine and trivial to test exhaustively.
func BuildSubject(in SubjectInput) string {
	// Parse "<entity>.<action>" from event_type. action may itself contain
	// dots (e.g. "field_changed" stays one token), so SplitN with n=2.
	parts := strings.SplitN(in.EventType, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		// Malformed event_type. We never expect to hit this with current
		// writers (every WriteFooEvent uses a const) but downgrade to a
		// "malformed" root rather than panic so a bad row doesn't kill the
		// dispatcher.
		return "skein.events.malformed." + sanitizeToken(in.EventType)
	}
	entity := parts[0]
	action := parts[1]

	// User events live on a separate top-level namespace because their
	// authorization rule is different (subscriber's own user_id, not a
	// project membership check). Field-changed user events still get the
	// "field.<kebab>" infix.
	if entity == "user" || in.EntityType == "user" {
		return "skein.user." + in.EntityID + "." + fieldifyAction(action, in.Field)
	}

	// All other entities require a project_id. A missing project_id is a
	// data error (every non-user write site sets it), so we surface it as
	// the malformed root. The event is still published so an operator
	// sees the row through the firehose.
	if in.ProjectID == nil {
		return "skein.events.malformed." + sanitizeToken(in.EventType)
	}

	pid := in.ProjectID.String()
	actionTok := fieldifyAction(action, in.Field)
	// Byte-identical to the prior fmt.Sprintf("skein.events.project.%s.%s.%s.%s", ...).
	const prefix = "skein.events.project."
	var b strings.Builder
	b.Grow(len(prefix) + len(pid) + 1 + len(entity) + 1 + len(actionTok) + 1 + len(in.EntityID))
	b.WriteString(prefix)
	b.WriteString(pid)
	b.WriteByte('.')
	b.WriteString(entity)
	b.WriteByte('.')
	b.WriteString(actionTok)
	b.WriteByte('.')
	b.WriteString(in.EntityID)
	return b.String()
}

// fieldifyAction handles the field-changed case. When the action is
// "field_changed" AND a field is present, the subject infix becomes
// "field.<kebab>" so subscribers can wildcard-match a specific field
// (e.g. `skein.events.project.>.issue.field.priority.>`). Other actions
// pass through unchanged.
func fieldifyAction(action, field string) string {
	if action == "field_changed" && field != "" {
		return "field." + camelToKebab(field)
	}
	return action
}

// camelToKebab maps lowerCamelCase to kebab-case. ASCII-only — every
// patchable field name in the Skein domain is plain English (title,
// description, priority, acceptanceCriteria, idMode, etc.) so we don't
// need full-Unicode case folding here. Non-letter runes pass through.
//
// Examples:
//
//	"priority"            -> "priority"
//	"acceptanceCriteria"  -> "acceptance-criteria"
//	"idPrefix"            -> "id-prefix"
//	"name"                -> "name"
func camelToKebab(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('-')
		}
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeToken produces a NATS-safe token from arbitrary input. NATS
// subjects forbid spaces, tabs, "*", ">", and "."; we replace everything
// non-alphanumeric/dash/underscore with "_". Only used by the malformed
// fallback path.
func sanitizeToken(s string) string {
	if s == "" {
		return "empty"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// subjectRoot returns the namespace prefix used as a Prometheus label
// value for skein_outbox_published_total. The function is bounded to
// exactly four return values (three known namespaces plus "other" as a
// safety net) so cardinality stays constant regardless of project count,
// user count, or entity count.
//
// Prefix matching (rather than dot counting) is required because
// "skein.user.<uid>.created" has only three dots — a count-based
// implementation would emit the full string (including the per-user uuid)
// as the label value, blowing up cardinality with every active user.
func subjectRoot(subject string) string {
	switch {
	case strings.HasPrefix(subject, "skein.events.project."):
		return "skein.events.project"
	case strings.HasPrefix(subject, "skein.user."):
		return "skein.user"
	case strings.HasPrefix(subject, "skein.events.malformed."):
		return "skein.events.malformed"
	default:
		return "other"
	}
}

// reminderEventPrefix is the event-type family that carries a personal
// reminder frame (sk-622.21, K14).
const reminderEventPrefix = "reminder."

// reminderFramePayload is the part of a `reminder.*` event payload the frame
// is built from (reminders/events.go writes it).
type reminderFramePayload struct {
	ReminderID  string `json:"reminderId"`
	RemindAt    string `json:"remindAt"`
	OwnerUserID string `json:"ownerUserId"`
}

// BuildReminderFrame turns one `reminder.*` event into the personal reminder
// frame of its owner (sk-622.21 T018, K14, contract signals.ReminderFrame):
// subject `skein.user.{owner}.reminder`, body with firingId =
// reminderId@remindAt. ok is false for any other event type and for a
// reminder event that cannot address a frame — one written before the owner
// joined the payload, or with a kind, id or time outside the contract; such
// an event still goes out on its project subject as before. The frame is
// derived from the stored event only, so a redelivery of the same event
// yields a byte-identical frame and the same firingId.
func BuildReminderFrame(evtType, issueID string, projectID *uuid.UUID, payload []byte) (subject string, body []byte, ok bool) {
	if !strings.HasPrefix(evtType, reminderEventPrefix) || projectID == nil {
		return "", nil, false
	}
	kind := signals.ReminderKind(strings.TrimPrefix(evtType, reminderEventPrefix))
	if !kind.Valid() {
		return "", nil, false
	}
	var p reminderFramePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", nil, false
	}
	owner, err := uuid.Parse(p.OwnerUserID)
	if err != nil || owner == uuid.Nil {
		return "", nil, false
	}
	reminderID, err := uuid.Parse(p.ReminderID)
	if err != nil {
		return "", nil, false
	}
	// RFC3339Nano layout: the payload carries the stored value's fractional
	// seconds (time.Parse accepts them under either layout; this one names
	// the producer's format).
	remindAt, err := time.Parse(time.RFC3339Nano, p.RemindAt)
	if err != nil {
		return "", nil, false
	}
	remindAt = remindAt.UTC()
	frame := signals.ReminderFrame{
		ReminderID: reminderID,
		IssueID:    issueID,
		ProjectID:  *projectID,
		Kind:       kind,
		RemindAt:   remindAt,
		FiringID:   signals.FiringID(reminderID, remindAt),
	}
	body, err = json.Marshal(frame)
	if err != nil {
		return "", nil, false
	}
	return signals.ReminderSubject(owner), body, true
}

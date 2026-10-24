package outbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Envelope is the JSON wire shape published as the body of every NATS
// message. Subscribers (skein-web SignalR Hub, future audit consumers)
// parse this and forward to clients. Treat the field set as a stable
// contract: additive changes only — never rename or remove a field.
//
// Semantics:
//
//   - ID is the events row's bigserial primary key. Subscribers MUST
//     dedup on this id to handle at-least-once delivery (a crash between
//     nc.Publish and tx.Commit re-publishes the same row on the next
//     dispatcher tick).
//   - OccurredAt is the canonical event time stored on the row, in UTC.
//   - EventType / EntityType / EntityID mirror the events row columns.
//   - ProjectID is omitted for user-scoped events (see SubjectInput).
//   - Payload is the events.payload jsonb passed through verbatim, so
//     consumers can read e.g. payload.parent_event_id (R3.2b) or
//     payload.changes (notifications fan-out) without a separate query.
type Envelope struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurredAt"`
	EventType  string          `json:"eventType"`
	EntityType string          `json:"entityType"`
	EntityID   string          `json:"entityId"`
	ProjectID  *uuid.UUID      `json:"projectId,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`

	// sk-21 — actor + entity context resolved at dispatch time via JOIN
	// against events / issues / comments / projects. Lets browser-tier
	// subscribers (the /activity SPA live ticker, primarily) render a
	// row WITHOUT a second round-trip per envelope to fetch human-
	// readable context. Pre-sk-21 envelopes carried only the bare
	// events row columns; SignalR-pushed rows rendered with the
	// fallback string "system" for the actor and an empty title,
	// since the payload didn't carry actor info.
	//
	// All fields are omitempty so a consumer that doesn't care (e.g.
	// notifications fan-out) sees no wire-size impact and old clients
	// that don't read these keys are unaffected.
	Actor       string  `json:"actor,omitempty"`
	ActorType   string  `json:"actorType,omitempty"`
	ActorUserID *string `json:"actorUserId,omitempty"`
	// EntityTitle is the human-readable label for the entity this event
	// is about. Issues / comments → resolved via JOIN; other entity
	// types (project, component, dep, attachment, user) → empty for now
	// (caller-rare in the ticker, can be widened later).
	EntityTitle string `json:"entityTitle,omitempty"`
	// ProjectIDPrefix is the project's idPrefix (e.g. "demo", "sk").
	// Used by frontend project-badge rendering without a separate
	// projects lookup. Empty when ProjectID is nil (user / global
	// events).
	ProjectIDPrefix string `json:"projectIdPrefix,omitempty"`

	// OldData / NewData mirror the events table columns. They flow on the
	// wire so subscribers can extract per-event deltas (e.g. skein-web's
	// SignalR hub maps project.member_added.new_data.userId to a cache
	// AddProject call) without an extra DB round trip. Both omitted when
	// NULL on the row — common case for lifecycle events that have no
	// before/after diff.
	OldData json.RawMessage `json:"oldData,omitempty"`
	NewData json.RawMessage `json:"newData,omitempty"`

	// Row is the phase-2 neutral list row of the issue (sk-622.21, US-15,
	// decisions K16 / K17): the JSON of `issues.NeutralRow`, carried on
	// issue events only and omitted on every other event. It holds no
	// reader-dependent field — no `iAssigned`, `isBlocked`, `epicProgress`
	// (INV-14) — and always carries `rowSeq`, the row freshness order
	// (0 = the issue row has not been written since the phase-2 rollout,
	// order unknown — see issues.IssueSummary.RowSeq).
	//
	// Freshness rule for the receiver: substitute the row only if its
	// `rowSeq` is greater than the `rowSeq` already shown for that issue; a
	// row that is not fresher changes nothing, but the event's other
	// reactions still apply. Dedup of a repeated envelope stays on ID.
	//
	// Raw bytes rather than a typed field because this package does not
	// import `issues` (K16): the dispatcher obtains rows through an
	// interface and passes them through verbatim, like Payload. Filled by
	// the dispatcher (T027) on events with entity type `issue` whose
	// project is the issue's own project, when the issue still exists; the
	// row is the issue's state when the dispatch tick read it, one statement
	// for the whole tick (dispatcher.go#NeutralRowSource).
	Row json.RawMessage `json:"row,omitempty"`
}

// buildEnvelope marshals an events row into the on-wire NATS body. It is
// called once per row inside the dispatcher tick, between SELECT and
// nc.Publish. payload / oldData / newData are the raw bytes from the
// events.payload / old_data / new_data jsonb columns — the dispatcher
// does NOT parse or re-serialize them, so any shape additions in
// event-write sites flow through automatically.
//
// Empty / NULL columns are omitted from the encoded JSON: an empty `{}`
// payload produces no `payload` field on the wire, saving ~10 bytes per
// message on lifecycle events that carry no extra data.
func buildEnvelope(
	id int64,
	occurredAt time.Time,
	eventType, entityType, entityID string,
	projectID *uuid.UUID,
	payload, oldData, newData []byte,
	enrich EnvelopeEnrichment,
) ([]byte, error) {
	env := Envelope{
		ID:              id,
		OccurredAt:      occurredAt.UTC(),
		EventType:       eventType,
		EntityType:      entityType,
		EntityID:        entityID,
		ProjectID:       projectID,
		Payload:         rawIfPresent(payload),
		OldData:         rawIfPresent(oldData),
		NewData:         rawIfPresent(newData),
		Actor:           enrich.Actor,
		ActorType:       enrich.ActorType,
		ActorUserID:     enrich.ActorUserID,
		EntityTitle:     enrich.EntityTitle,
		ProjectIDPrefix: enrich.ProjectIDPrefix,
		Row:             rawIfPresent(enrich.Row),
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope id=%d: %w", id, err)
	}
	return body, nil
}

// EnvelopeEnrichment carries the fields the dispatcher resolves via
// JOIN at SELECT time (actor identity + entity-title + project prefix).
// Bundled in a struct so the buildEnvelope signature stays readable.
// All fields optional; an empty struct produces a pre-sk-21-shaped
// envelope (still wire-compatible with old consumers).
type EnvelopeEnrichment struct {
	Actor           string
	ActorType       string
	ActorUserID     *string
	EntityTitle     string
	ProjectIDPrefix string
	// Row is the phase-2 neutral row of an issue event (sk-622.21, T027) —
	// the JSON of issues.NeutralRow, passed through verbatim into
	// Envelope.Row. Empty on every other event.
	Row json.RawMessage
}

// rawIfPresent normalises a jsonb byte slice into an Envelope field.
// Returns nil for empty input or the literal "{}" / "null", which yields
// `omitempty` suppression on the encoded wire. Anything else passes
// through verbatim — Postgres already validated the bytes as JSON, so we
// don't re-parse them here.
func rawIfPresent(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if bytes.Equal(b, []byte("{}")) || bytes.Equal(b, []byte("null")) {
		return nil
	}
	return json.RawMessage(b)
}

// NotificationEnvelope is the JSON wire shape published on
// `skein.user.{uid}.notification` (R3.3). Mirrors the columns the inbox
// query reads — by the time a subscriber sees this frame they already
// know "you have a new notification", and the frontend refreshes the
// list / count via REST. Keeping the body small (no event payload, no
// summary string) keeps per-user fan-out cheap.
//
// EventType is the underlying event's type (e.g. "comment.created").
// The frontend uses this to decide whether to bump the unread badge for
// a specific scope or just refresh counts.
type NotificationEnvelope struct {
	NotificationID int64      `json:"notificationId"`
	UserID         uuid.UUID  `json:"userId"`
	EventID        int64      `json:"eventId"`
	EventType      string     `json:"eventType"`
	ProjectID      *uuid.UUID `json:"projectId,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// buildNotificationEnvelope marshals one notifications row into the
// on-wire NATS body for the per-user notification subject.
func buildNotificationEnvelope(
	id int64,
	userID uuid.UUID,
	eventID int64,
	eventType string,
	projectID *uuid.UUID,
	createdAt time.Time,
) ([]byte, error) {
	env := NotificationEnvelope{
		NotificationID: id,
		UserID:         userID,
		EventID:        eventID,
		EventType:      eventType,
		ProjectID:      projectID,
		CreatedAt:      createdAt.UTC(),
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal notification envelope id=%d: %w", id, err)
	}
	return body, nil
}

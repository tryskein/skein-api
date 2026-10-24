package eventbus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Actor identifies who caused an event. Mirrors per-entity Actor types
// (issues.Actor, projects.Actor, users.Actor) — each call site builds one
// from its own Actor at the package boundary so eventbus does not depend
// on any of the four entity packages (avoids an import cycle).
//
// Role is one of {'human','agent','system'} and lands in events.actor_type.
// SessionID is empty for REST callers and non-empty for MCP callers; the
// hybrid writer renders empty as SQL NULL on actor_session_id.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string
	SessionID string
}

// WriteHybridUpdateEvents writes one parent <entity>.updated event whose
// payload.changes carries the full diff, plus N child <entity>.field_changed
// events — one per FieldChange — that each carry the parent's bigserial id
// in payload.parent_event_id. Every INSERT runs on the caller's pgx.Tx, so
// the parent + children + the originating UPDATE land atomically.
//
// parentEventType is the string written into the parent row's events.type
// column (e.g. "issue.updated"). childEventType is the corresponding
// "<entity>.field_changed" value the outbox dispatcher (R3.2a) maps onto
// "skein.events.project.<pid>.<entity>.field.<kebab>.<eid>" subjects.
//
// projectID is denormalised on every event row — pass nil for entities
// that have no project scope (user.* events). entityID is stringified at
// the call site (uuid.String() for project/component/user, hierarchical
// id for issues).
//
// reason is optional (empty → SQL NULL). PatchIssue uses it to surface
// "force-overridden" on admin overrides; other call sites pass "".
//
// When changes is empty the function returns (0, nil, nil) and writes
// nothing — the caller is expected to skip downstream FanOut in that case.
// With the COALESCE-based UPDATE on patch SQL this should be rare (every
// patch path computes the diff before it commits to writing), but the
// empty-set guard means the helper is safe to call unconditionally.
//
// Returns parentPayload (the marshalled {"changes": changes} bytes that
// were inserted into events.payload) so callers can pipe them into
// notifications.FanOut without a duplicate Marshal — single source of
// truth for the parent event body. Bytes are nil when changes is empty.
func WriteHybridUpdateEvents(
	ctx context.Context,
	tx pgx.Tx,
	parentEventType string,
	childEventType string,
	entityType string,
	entityID string,
	projectID *uuid.UUID,
	actor Actor,
	changes []FieldChange,
	reason string,
) (parentEventID int64, parentPayload []byte, err error) {
	if len(changes) == 0 {
		return 0, nil, nil
	}

	// Parent payload: {"changes":[{"field":"x","oldValue":...,"newValue":...},...]}.
	// Persisted as jsonb — the dispatcher reads payload as opaque bytes.
	// Typed envelope avoids map[string]any boxing of each FieldChange.
	parentPayload, err = json.Marshal(hybridParentPayload{Changes: changes})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal parent payload: %w", err)
	}

	actorType := actor.Role
	if actorType == "" {
		actorType = "human"
	}

	var actorUserID any
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}
	var sessionArg any
	if actor.SessionID != "" {
		sessionArg = actor.SessionID
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	var projectArg any
	if projectID != nil {
		projectArg = *projectID
	}

	// Parent row: field/old_data/new_data are NULL — the diff lives in
	// payload.changes. The events.entity_type CHECK admits all values we
	// pass here (issue / project / component / user); rejection here would
	// indicate a writer bug, not a runtime data error.
	const parentSQL = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, NULL, NULL, NULL, $9, $10::jsonb
)
RETURNING id`
	if err := tx.QueryRow(ctx, parentSQL,
		parentEventType,
		actor.Username,
		actorUserID,
		sessionArg,
		actorType,
		entityType,
		entityID,
		projectArg,
		reasonArg,
		string(parentPayload),
	).Scan(&parentEventID); err != nil {
		return 0, nil, fmt.Errorf("insert parent event %s: %w", parentEventType, err)
	}

	// One child per changed field. Each child mirrors the parent's actor /
	// entity / project envelope and adds (field, old_data, new_data) plus
	// payload.parent_event_id so subscribers can stitch the family back
	// together without re-reading payload.changes on the parent.
	const childSQL = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $13::jsonb
)`
	// childPayload is identical for every child of a given parent —
	// hoisted out of the loop so we marshal once instead of N times.
	childPayload, err := json.Marshal(map[string]any{"parentEventId": parentEventID})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal child payload: %w", err)
	}

	for _, fc := range changes {
		oldData, err := json.Marshal(fc.OldValue)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal old_data for field %s: %w", fc.Field, err)
		}
		newData, err := json.Marshal(fc.NewValue)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal new_data for field %s: %w", fc.Field, err)
		}

		var fieldArg any = fc.Field
		if fc.Field == "" {
			fieldArg = nil
		}

		if _, err := tx.Exec(ctx, childSQL,
			childEventType,
			actor.Username,
			actorUserID,
			sessionArg,
			actorType,
			entityType,
			entityID,
			projectArg,
			fieldArg,
			string(oldData),
			string(newData),
			reasonArg,
			string(childPayload),
		); err != nil {
			return 0, nil, fmt.Errorf("insert child event for field %s: %w", fc.Field, err)
		}
	}

	return parentEventID, parentPayload, nil
}

// hybridParentPayload is the parent event body written to events.payload.
// Private typed shape keeps JSON wire identical to the prior
// map[string]any{"changes": changes} marshal without interface boxing of
// the changes slice itself.
type hybridParentPayload struct {
	Changes []FieldChange `json:"changes"`
}

package users

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event-type constants. Mirrors projects/events.go: type = "<entity>.<action>".
// Schema events.entity_type already includes 'user' — see migration 000004
// (the events table CHECK admits 'user' alongside project, component,
// issue, dependency).
const (
	EventUserCreated         = "user.created"
	EventUserUpdated         = "user.updated"
	EventUserFieldChanged    = "user.field_changed"
	EventUserDeactivated     = "user.deactivated"
	EventUserReactivated     = "user.reactivated"
	EventUserPasswordChanged = "user.password_changed"
)

// writeUserEvent inserts a row into events for a user mutation. Mirrors
// projects.WriteProjectEvent. project_id is left NULL because user
// mutations are not scoped to a project — every audit consumer that
// filters by project will simply skip these rows.
//
// Both old and new are interface{} so callers can pass any struct that
// json.Marshal accepts; nil is allowed and stored as SQL NULL.
func writeUserEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	userID uuid.UUID,
	actor Actor,
	old, newVal any,
) error {
	var oldJSON, newJSON *string
	if old != nil {
		b, err := json.Marshal(old)
		if err != nil {
			return fmt.Errorf("marshalling old_data for %s: %w", eventType, err)
		}
		s := string(b)
		oldJSON = &s
	}
	if newVal != nil {
		b, err := json.Marshal(newVal)
		if err != nil {
			return fmt.Errorf("marshalling new_data for %s: %w", eventType, err)
		}
		s := string(b)
		newJSON = &s
	}

	var sessionID *string
	if actor.SessionID != "" {
		s := actor.SessionID
		sessionID = &s
	}

	var actorUserID any
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}

	actorType := actor.Role
	if actorType == "" {
		actorType = "human"
	}

	_, err := tx.Exec(ctx, `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, old_data, new_data, payload
) VALUES (
    $1, $2, $3, $4, $5,
    'user', $6, NULL, $7::jsonb, $8::jsonb, '{}'::jsonb
)`,
		eventType,
		actor.Username,
		actorUserID,
		sessionID,
		actorType,
		userID.String(),
		oldJSON,
		newJSON,
	)
	if err != nil {
		return fmt.Errorf("inserting event %s: %w", eventType, err)
	}
	return nil
}

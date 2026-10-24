package projects

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event-type constants. Matches the events table: type
// = "<entity>.<action>". Each constant is the wire-string written to
// events.type.
const (
	EventProjectCreated      = "project.created"
	EventProjectUpdated      = "project.updated"
	EventProjectFieldChanged = "project.field_changed"
	EventProjectArchived     = "project.archived"
	EventProjectUnarchive    = "project.unarchived"

	EventProjectRepoAdded   = "project.repo_added"
	EventProjectRepoRemoved = "project.repo_removed"
	EventProjectRepoUpdated = "project.repo_updated"

	EventProjectAliasAdded   = "project.alias_added"
	EventProjectAliasRemoved = "project.alias_removed"

	EventProjectMemberAdded       = "project.member_added"
	EventProjectMemberRoleChanged = "project.member_role_changed"
	EventProjectMemberRemoved     = "project.member_removed"

	EventComponentCreated      = "component.created"
	EventComponentUpdated      = "component.updated"
	EventComponentFieldChanged = "component.field_changed"
	EventComponentArchived     = "component.archived"
	EventComponentUnarchive    = "component.unarchived"
)

// Actor describes who triggered the event. For R1.2 every mutation is
// human-driven; agent path lands in R2.x.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string // 'human' | 'agent' | 'system'
	SessionID string // optional, agent only
}

// HumanActor is a small constructor for the common case. Username is
// REQUIRED — we never write empty actor strings to events.actor.
func HumanActor(userID uuid.UUID, username string) Actor {
	return Actor{UserID: userID, Username: username, Role: "human"}
}

// WriteProjectEvent inserts a row into events for a project mutation.
//
// Both old and new are interface{} so callers can pass any struct that
// json.Marshal accepts; nil is allowed and stored as SQL NULL. This is the
// happy path for create/delete/archive (one side is always nil) and for
// updates (both populated).
//
// The function executes a single INSERT — caller is expected to share its
// own pgx.Tx so the event lands in the same transaction as the mutation.
//
// Returns the new event's bigserial id. R1.6 watchers fan-out for project
// events is not implemented yet, but the signature is aligned with
// WriteIssueEvent for consistency.
func WriteProjectEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	projectID uuid.UUID,
	actor Actor,
	old, new any,
) (int64, error) {
	return writeEvent(ctx, tx, eventType, "project", projectID.String(),
		&projectID, actor, old, new)
}

// WriteComponentEvent is the same shape as WriteProjectEvent but writes
// entity_type='component'. project_id is the parent project (denormalized
// per §2.17 so NATS subject filters can scope on project without a JOIN).
//
// Returns the new event's bigserial id (consistency with WriteIssueEvent).
func WriteComponentEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	componentID uuid.UUID,
	parentProjectID uuid.UUID,
	actor Actor,
	old, new any,
) (int64, error) {
	return writeEvent(ctx, tx, eventType, "component", componentID.String(),
		&parentProjectID, actor, old, new)
}

const writeEventSQL = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, old_data, new_data, payload
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9::jsonb, $10::jsonb, '{}'::jsonb
)
RETURNING id
`

func writeEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType, entityType, entityID string,
	projectID *uuid.UUID,
	actor Actor,
	old, new any,
) (int64, error) {
	var oldJSON, newJSON *string
	if old != nil {
		b, err := json.Marshal(old)
		if err != nil {
			return 0, fmt.Errorf("marshalling old_data for %s: %w", eventType, err)
		}
		s := string(b)
		oldJSON = &s
	}
	if new != nil {
		b, err := json.Marshal(new)
		if err != nil {
			return 0, fmt.Errorf("marshalling new_data for %s: %w", eventType, err)
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

	var eventID int64
	err := tx.QueryRow(ctx, writeEventSQL,
		eventType,
		actor.Username,
		actorUserID,
		sessionID,
		actorType,
		entityType,
		entityID,
		projectID,
		oldJSON,
		newJSON,
	).Scan(&eventID)
	if err != nil {
		return 0, fmt.Errorf("inserting event %s: %w", eventType, err)
	}
	return eventID, nil
}

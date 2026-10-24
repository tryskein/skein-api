package dependencies

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event-type constants for dependency mutations. The audit timeline emits
// one row per CREATE / DELETE; updates aren't supported (DELETE+INSERT is
// the contract).
const (
	EventDependencyCreated = "dependency.created"
	EventDependencyDeleted = "dependency.deleted"
	// EventIssueReparented is emitted by Store.Reparent once per call
	// (alongside the per-edge dependency.deleted / dependency.created
	// rows). It is an ISSUE-scoped event (entity_type='issue',
	// entity_id=<child id>) so it lands in the issue timeline / activity
	// feed, not the dep audit. payload records the old parent(s) and the
	// new parent so consumers can replay the move from the timeline.
	EventIssueReparented = "issue.reparented"
)

// Keys that WriteDependencyEvent adds to the data of every
// dependency.created / dependency.deleted row (sk-622.21 T007, decision
// K15, R8, INV-17). They are a contract for skein-web (T033), which keeps
// in the browser envelope only the edge ends whose project equals the
// event's project, so an issue of another project is never identified to
// a browser that may not see it. Deciding by these keys, not by the id
// prefix, is the point: project aliases change.
//
// The data object (new_data on created, old_data on deleted) therefore
// reads, for an edge from sk-1 (project A) to demo-7 (project B):
//
//	{"fromId":"sk-1","toId":"demo-7","type":"blocks","metadata":{},
//	 "createdBy":"…","fromProjectId":"<uuid of A>","toProjectId":"<uuid of B>"}
//
// where:
//
//   - fromProjectId — project of the FROM issue; equals the event row's
//     project_id (the FROM-side project, see WriteDependencyEvent).
//   - toProjectId — project of the TO issue; differs from fromProjectId
//     on a cross-project edge.
//
// Both are UUID strings, resolved from the issues table in the same
// statement that writes the event, i.e. the ends' projects at the moment
// of the write. Additive: every other key of the data object is unchanged.
// A key is JSON null only if its issue row no longer exists at that
// moment, which the dependencies FKs do not allow.
const (
	DataKeyFromProjectID = "fromProjectId"
	DataKeyToProjectID   = "toProjectId"
)

// Actor mirrors issues.Actor / projects.Actor — duplicated to keep this
// package free of cross-package coupling. R1.4 always writes 'human'
// actors (BFF-mediated); agent path comes in R2.x.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string // 'human' | 'agent' | 'system'
	SessionID string // optional, agent only
}

// HumanActor is the constructor for the common path (BFF acting on behalf
// of a logged-in human).
func HumanActor(userID uuid.UUID, username string) Actor {
	return Actor{UserID: userID, Username: username, Role: "human"}
}

// dependencyEntityID encodes the composite-PK row as a single string for
// the events.entity_id column (which is text). Using a stable, parseable
// shape so timeline consumers can reconstruct (from, to, type) without a
// JSON round-trip.
func dependencyEntityID(fromID, toID, depType string) string {
	return fromID + "::" + toID + "::" + depType
}

// writeEventSQL is the single insert template. payload column is reserved
// for future structured extensions (NATS subject filter etc) — kept
// `'{}'::jsonb` for now because R1.4 packs everything we need in
// new_data / old_data.
//
// The ends' projects ($10 / $11 are the FROM / TO issue ids) are merged
// into whichever of old_data / new_data the caller supplied, under
// DataKeyFromProjectID / DataKeyToProjectID; the absent side stays NULL
// (NULL || jsonb is NULL). One statement, no extra round-trip.
//
// RETURNING id is consistent with the issues/projects writers — R1.6 fan-out
// keys on the event id even when this package doesn't itself fan out yet.
const writeEventSQL = `
WITH ends AS (
    SELECT jsonb_build_object(
        'fromProjectId', (SELECT project_id FROM issues WHERE id = $10),
        'toProjectId',   (SELECT project_id FROM issues WHERE id = $11)
    ) AS data
)
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) SELECT
    $1, $2, $3, $4, $5,
    'dep', $6, $7, NULL, $8::jsonb || ends.data, $9::jsonb || ends.data, NULL, '{}'::jsonb
  FROM ends
RETURNING id
`

// WriteDependencyEvent inserts an audit row for a CREATE or DELETE. data
// is the JSON-able payload (typically {fromId, toId, type, metadata}); it
// lands in old_data on a delete and new_data on a create, extended with
// the ends' projects (DataKeyFromProjectID / DataKeyToProjectID — the
// caller does not pass them, and a caller-supplied key of the same name is
// overwritten by the resolved value). data must marshal to a JSON object. projectID is
// the FROM-side project (denormalised on the event row so consumers can
// filter by project without a join — see db-schema §2.17).
//
// Returns the new event's bigserial id (consistent with the other
// Write*Event helpers; R1.6 doesn't fan out dep events but the signature
// matches so callers can chain consistently).
func WriteDependencyEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	fromID, toID, depType string,
	data any,
	projectID uuid.UUID,
	actor Actor,
) (int64, error) {
	var dataJSON *string
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return 0, fmt.Errorf("marshalling event data for %s: %w", eventType, err)
		}
		s := string(b)
		dataJSON = &s
	}

	// CREATE writes new_data; DELETE writes old_data — matches the same
	// convention used by issues / comments (events.go in those packages).
	var oldJSON, newJSON *string
	switch eventType {
	case EventDependencyCreated:
		newJSON = dataJSON
	case EventDependencyDeleted:
		oldJSON = dataJSON
	default:
		return 0, fmt.Errorf("unknown dependency event type %q", eventType)
	}

	sessionID, actorUserID, actorType := normaliseActor(actor)

	var eventID int64
	err := tx.QueryRow(ctx, writeEventSQL,
		eventType,
		actor.Username,
		actorUserID,
		sessionID,
		actorType,
		dependencyEntityID(fromID, toID, depType),
		projectID,
		oldJSON,
		newJSON,
		fromID,
		toID,
	).Scan(&eventID)
	if err != nil {
		return 0, fmt.Errorf("inserting event %s: %w", eventType, err)
	}
	return eventID, nil
}

// normaliseActor maps an Actor into the (session_id, actor_user_id,
// actor_type) trio the events INSERTs bind. Mirrors the identical
// normalisation in issues/projects event writers: empty session → SQL
// NULL, nil user → SQL NULL, empty role → 'human'.
func normaliseActor(actor Actor) (sessionID *string, actorUserID any, actorType string) {
	if actor.SessionID != "" {
		s := actor.SessionID
		sessionID = &s
	}
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}
	actorType = actor.Role
	if actorType == "" {
		actorType = "human"
	}
	return sessionID, actorUserID, actorType
}

// writeIssueScopedEventSQL inserts an ISSUE-scoped event row. Distinct
// from writeEventSQL (which hardcodes entity_type='dep'): the reparent
// event must land under entity_type='issue' so issue timeline / activity
// queries (which filter entity_type='issue') surface it. payload carries
// the structured {oldParentIds, newParentId} so the move is replayable.
const writeIssueScopedEventSQL = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    'issue', $6, $7, NULL, $8::jsonb, $9::jsonb, NULL, $10::jsonb
)
RETURNING id
`

// WriteReparentEvent records the issue.reparented audit row for one
// Store.Reparent call. issueID is the child that moved; projectID is its
// project (parent-child is intra-project so old/new parents share it).
//
// oldParentIDs is every parent the child had BEFORE the move (usually one,
// but ≥2 when self-healing a double-parented issue — all collapsed edges
// are recorded). newParentID is the single parent after the move.
//
// old_data / new_data / payload all carry the structured parent state so
// timeline consumers can replay the move without re-querying the dep graph.
// Returns the new event's bigserial id, consistent with the other writers.
func WriteReparentEvent(
	ctx context.Context,
	tx pgx.Tx,
	issueID string,
	projectID uuid.UUID,
	oldParentIDs []string,
	newParentID string,
	actor Actor,
) (int64, error) {
	if oldParentIDs == nil {
		oldParentIDs = []string{}
	}
	oldData := map[string]any{"parentIds": oldParentIDs}
	newData := map[string]any{"parentId": newParentID}
	payload := map[string]any{
		"oldParentIds": oldParentIDs,
		"newParentId":  newParentID,
	}

	oldJSON, err := json.Marshal(oldData)
	if err != nil {
		return 0, fmt.Errorf("marshalling old_data for %s: %w", EventIssueReparented, err)
	}
	newJSON, err := json.Marshal(newData)
	if err != nil {
		return 0, fmt.Errorf("marshalling new_data for %s: %w", EventIssueReparented, err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshalling payload for %s: %w", EventIssueReparented, err)
	}

	sessionID, actorUserID, actorType := normaliseActor(actor)

	var eventID int64
	if err := tx.QueryRow(ctx, writeIssueScopedEventSQL,
		EventIssueReparented,
		actor.Username,
		actorUserID,
		sessionID,
		actorType,
		issueID,
		projectID,
		string(oldJSON),
		string(newJSON),
		string(payloadJSON),
	).Scan(&eventID); err != nil {
		return 0, fmt.Errorf("inserting event %s: %w", EventIssueReparented, err)
	}
	return eventID, nil
}

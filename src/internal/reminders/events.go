package reminders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event-type constants for the reminder lifecycle (db-schema §2.17
// "type = <entity>.<action>"). The events.entity_type CHECK constraint
// (000001_init) is a closed set that does NOT include 'reminder', so every
// reminder event is written with entity_type='issue' and entity_id=<issue
// id>: a reminder is conceptually a fact ABOUT an issue, and routing it
// under the issue keeps the outbox subject (skein.events.project.<pid>.
// reminder.<action>.<issueId>) project-scoped without a schema change. The
// reminder's own uuid + the timing detail ride in events.payload.
const (
	// Every payload also carries ownerUserId (sk-622.21 T018, K14): the
	// outbox dispatcher addresses the owner's personal reminder frame
	// (`skein.user.{owner}.reminder`) from it.
	//
	// remindAt / previousRemindAt are the STORED reminders.remind_at values
	// (read back from the row, so in the database's microsecond precision),
	// rendered by formatRemindAt: UTC, RFC 3339 with the fractional seconds
	// the value has. The frame's firingId is built from remindAt and must
	// equal signals.FiringID of the stored value (found in review).
	//
	// EventReminderSet — a reminder was created. payload:
	//   { reminderId, remindAt }
	EventReminderSet = "reminder.set"
	// EventReminderSnoozed — remind_at pushed forward, notified_at reset,
	// snooze_count bumped. payload:
	//   { reminderId, remindAt, previousRemindAt, snoozeCount, reason? }
	EventReminderSnoozed = "reminder.snoozed"
	// EventReminderRang — the scheduler materialised a bell notification
	// for a due reminder. payload:
	//   { reminderId, remindAt }
	// Written by the scheduler with a 'system' actor.
	EventReminderRang = "reminder.rang"
	// EventReminderDismissed — the reminder row was deleted. payload:
	//   { reminderId, remindAt, note? }
	EventReminderDismissed = "reminder.dismissed"
)

// formatRemindAt renders a due time for an event payload: UTC, RFC 3339
// with the fractional seconds the value actually has (time.RFC3339Nano) —
// the same rendering signals.FiringID uses, so the firingId the dispatcher
// derives from the payload is the one the stored remind_at yields.
func formatRemindAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// writeReminderEvent inserts an events row scoped to the reminder's parent
// issue. entity_type='issue', entity_id=issueID; the reminder uuid + timing
// live in payload. Returns the bigserial event id so the caller can chain a
// notification insert (the scheduler ring path) under the same tx.
//
// Mirrors issues.WriteIssueEvent / attachments.writeAttachmentEvent —
// duplicating ~40 lines beats importing the issues package and dragging in
// its dependency graph (the lockreap package made the same call).
func writeReminderEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	payload map[string]any,
) (int64, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshalling reminder payload: %w", err)
	}

	var actorUserID any
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}
	var sessionID any
	if actor.SessionID != "" {
		sessionID = actor.SessionID
	}
	actorType := actor.Role
	if actorType == "" {
		actorType = "human"
	}
	actorName := actor.Username
	if actorName == "" {
		actorName = actorType
	}

	const sql = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    'issue', $6, $7, NULL, NULL, NULL, NULL, $8::jsonb
)
RETURNING id
`
	var eventID int64
	if err := tx.QueryRow(ctx, sql,
		eventType,
		actorName,
		actorUserID,
		sessionID,
		actorType,
		issueID,
		projectID,
		string(payloadBytes),
	).Scan(&eventID); err != nil {
		return 0, fmt.Errorf("inserting reminder event %s: %w", eventType, err)
	}
	return eventID, nil
}

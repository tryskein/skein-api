package reminders

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
)

// DueReminder is the slim row the scheduler claims: enough to write the
// ring event + notification, no issue summary join (the scheduler does not
// render the row, it only notifies).
type DueReminder struct {
	ID        uuid.UUID
	IssueID   string
	UserID    uuid.UUID
	ProjectID uuid.UUID
	RemindAt  time.Time
}

// ClaimAndNotifyDue is the scheduler's per-tick unit of work. In ONE
// SERIALIZABLE transaction it:
//
//  1. SELECTs up to `limit` due-and-un-notified reminders FOR UPDATE SKIP
//     LOCKED — so concurrent API replicas each grab a disjoint slice and
//     never double-ring the same row.
//  2. For each claimed row writes a reminder.rang event (system actor) and
//     a notification row addressed to the reminder's owner.
//  3. Stamps notified_at = now() on the claimed rows.
//
// "Due and un-notified" is
//
//	remind_at <= now() AND (notified_at IS NULL OR notified_at < remind_at)
//
// so a reminder that already rang for its current remind_at is skipped
// (idempotency), while a snoozed-then-due-again reminder (notified_at reset
// to NULL on snooze) rings afresh. The notification row is published to
// skein.user.{uid}.notification by the existing outbox dispatcher, so the
// live bell lights up with no new transport.
//
// Returns the number of reminders rung this call. A return < limit means
// the backlog is drained for now.
//
// SKIP LOCKED + the same-tx notified_at stamp give at-least-once delivery
// that is effectively exactly-once: a crash between the event/notification
// insert and COMMIT rolls back ALL of it (the notified_at stamp included),
// so the next tick re-claims and re-rings cleanly — no orphaned
// notification without a notified_at, and no notified_at without a
// notification.
//
// # The preferences gate, and why this path is the EXCEPTION (R-15 / SC-020)
//
// Everywhere else, a failure to read the notification preferences means "no
// row, but the user's own operation still commits": the comment or the edit is
// self-contained and worth keeping even when the notification about it is
// lost.
//
// A reminder has no such parent operation. The tick IS the delivery: it rings
// and, in the same transaction, records that the ring happened. Applying the
// usual rule here — "no row, tick succeeds" — would destroy the reminder
// forever, and "Reminder" is a one-channel reason: its feed channel is not
// applicable at all, so the push is the only delivery there is.
//
// So the preferences read is INSIDE the transaction and its failure aborts the
// whole tick. Nothing is inserted, notified_at is not stamped, and the
// instance stays due for the next tick. "There are no burnt-out reminders."
//
// A read that SUCCEEDS and says the channel is off is a different thing
// entirely: the ring happened, the user asked not to be pushed about it, so
// notified_at IS stamped and no notification row is written (INV-1). The
// ringing state the interface shows lives on the reminder itself and does not
// depend on any of this.
func (s *Store) ClaimAndNotifyDue(ctx context.Context, limit int, actor Actor) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	// Retry + backoff come from the package-shared withSerializableRetry
	// (store.go): same 3 attempts, same jittered backoff, same "a
	// non-serialization error aborts immediately" rule. rung is reset at the
	// top of the closure so a retried attempt never inherits the previous
	// attempt's count.
	var rung int
	err := withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		rung = 0
		now := time.Now().UTC()

		rows, qerr := tx.Query(ctx, `
SELECT r.id, r.issue_id, r.user_id, i.project_id, r.remind_at
  FROM reminders r
  JOIN issues i ON i.id = r.issue_id
 WHERE r.remind_at <= $1
   AND (r.notified_at IS NULL OR r.notified_at < r.remind_at)
 ORDER BY r.remind_at ASC
 LIMIT $2
 FOR UPDATE OF r SKIP LOCKED`, now, limit)
		if qerr != nil {
			return fmt.Errorf("claiming due reminders: %w", qerr)
		}
		due := make([]DueReminder, 0, limit)
		for rows.Next() {
			var d DueReminder
			if serr := rows.Scan(&d.ID, &d.IssueID, &d.UserID, &d.ProjectID, &d.RemindAt); serr != nil {
				rows.Close()
				return fmt.Errorf("scanning due reminder: %w", serr)
			}
			due = append(due, d)
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return fmt.Errorf("iterating due reminders: %w", rerr)
		}
		if len(due) == 0 {
			return nil
		}

		// One preferences read for the whole batch — never one per reminder.
		// A failure here is the abort described above.
		owners := make([]uuid.UUID, 0, len(due))
		seen := make(map[uuid.UUID]struct{}, len(due))
		for _, d := range due {
			if _, dup := seen[d.UserID]; dup {
				continue
			}
			seen[d.UserID] = struct{}{}
			owners = append(owners, d.UserID)
		}
		pushEnabled, perr := reminderPushPrefs(ctx, tx, owners)
		if perr != nil {
			return perr
		}

		ids := make([]uuid.UUID, 0, len(due))
		for _, d := range due {
			payload := map[string]any{
				"reminderId":  d.ID.String(),
				"remindAt":    formatRemindAt(d.RemindAt),
				"ownerUserId": d.UserID.String(),
			}
			eventID, eerr := writeReminderEvent(ctx, tx, EventReminderRang, d.IssueID, d.ProjectID, actor, payload)
			if eerr != nil {
				return eerr
			}
			// Point-to-point notification to the reminder's owner — but only
			// when the owner's push channel for "Reminder" is on. With the
			// feed channel inapplicable, a switched-off push means both
			// channels are off, and INV-1 says no row is created at all.
			//
			// The row is push-only by construction: feed_channel=false, so it
			// never appears in any feed, counter, live stream or agent read —
			// "old rings in the feed would depict behaviour that new rings
			// no longer produce".
			//
			// ON CONFLICT (user_id, event_id) DO NOTHING is defensive — each
			// ring writes a fresh event so a collision is not expected, but it
			// keeps the insert idempotent under retry.
			if pushEnabled[d.UserID] {
				if _, nerr := tx.Exec(ctx, `
INSERT INTO notifications (
    user_id, event_id, type, project_id,
    reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at,
    card_resolved)
VALUES ($1, $2, $3, $4,
        $5, false,
        'pending', now(), now() + ($6::bigint * interval '1 second'),
        true) -- no Watching card ever (sk-622.21 US-4); resolved = decided (K6)
ON CONFLICT (user_id, event_id) DO NOTHING`,
					d.UserID, eventID, EventReminderRang, d.ProjectID,
					string(notifications.ReasonKeyReminder),
					int64(notifications.PushDeliveryDeadline/time.Second)); nerr != nil {
					return fmt.Errorf("inserting reminder notification: %w", nerr)
				}
			}
			// notified_at is stamped either way: the ring HAPPENED. Whether a
			// push was owed for it is a separate question, and the interface's
			// ringing state does not read the answer.
			ids = append(ids, d.ID)
		}

		// Stamp notified_at on every claimed row in one statement.
		if _, uerr := tx.Exec(ctx, `
UPDATE reminders SET notified_at = now(), updated_at = now()
 WHERE id = ANY($1::uuid[])`, ids); uerr != nil {
			return fmt.Errorf("stamping notified_at: %w", uerr)
		}
		rung = len(ids)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rung, nil
}

// reminderPushPrefs reads, for the owners of one claimed batch, whether the
// "Reminder" push channel is on. One statement for the batch.
//
// An absent row means the registry default, exactly as everywhere else: the
// absence of a preference is "the narrow default is enough", never "everything is on".
//
// An error is returned verbatim so the caller aborts the tick — see the
// contract on ClaimAndNotifyDue.
func reminderPushPrefs(ctx context.Context, tx pgx.Tx, owners []uuid.UUID) (map[uuid.UUID]bool, error) {
	spec, ok := notifications.LookupReason(notifications.ReasonKeyReminder)
	if !ok {
		return nil, fmt.Errorf("reminder reason missing from the registry")
	}
	out := make(map[uuid.UUID]bool, len(owners))
	for _, u := range owners {
		out[u] = spec.Push.Applicable && spec.Push.DefaultOn
	}

	rows, err := tx.Query(ctx, `
SELECT user_id, push_enabled
  FROM user_notification_reason_prefs
 WHERE user_id = ANY($1::uuid[]) AND reason = $2`,
		owners, string(notifications.ReasonKeyReminder))
	if err != nil {
		return nil, fmt.Errorf("reading reminder push preferences: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			userID  uuid.UUID
			enabled bool
		)
		if err := rows.Scan(&userID, &enabled); err != nil {
			return nil, fmt.Errorf("scanning reminder push preferences: %w", err)
		}
		out[userID] = enabled && spec.Push.Applicable
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading reminder push preferences: %w", err)
	}
	return out, nil
}

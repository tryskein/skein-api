package reminders

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scheduler_push_test.go — sk-303.13 / T011: the reminder tick against the
// reason registry.
//
// "Reminder" is the one-channel reason: its feed channel is not applicable at
// all, so the push is the only delivery it has. That makes the tick the
// exception to the wave's usual failure rule — there is no parent operation to
// keep, the tick IS the delivery, and "we do not create a row, the tick succeeds" would
// destroy the reminder forever.

// reminderRow is the notification the tick wrote for a rung reminder, if any.
type reminderRow struct {
	found       bool
	reason      *string
	feedChannel bool
	pushState   string
	hasDeadline bool
}

func readReminderNotification(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, issueID string) reminderRow {
	t.Helper()
	var r reminderRow
	err := pool.QueryRow(context.Background(), `
SELECT n.reason, n.feed_channel, n.push_state, n.push_deadline_at IS NOT NULL
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1 AND e.entity_id = $2 AND e.type = $3
 ORDER BY n.id DESC LIMIT 1`, owner, issueID, EventReminderRang).
		Scan(&r.reason, &r.feedChannel, &r.pushState, &r.hasDeadline)
	if err != nil {
		return reminderRow{}
	}
	r.found = true
	return r
}

func notifiedAt(t *testing.T, pool *pgxpool.Pool, reminderID uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT notified_at FROM reminders WHERE id = $1`, reminderID).Scan(&at); err != nil {
		t.Fatalf("read notified_at: %v", err)
	}
	return at
}

func setReminderPush(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'reminder', false, $2)
ON CONFLICT (user_id, reason) DO UPDATE SET push_enabled = EXCLUDED.push_enabled`,
		userID, enabled); err != nil {
		t.Fatalf("set reminder push pref: %v", err)
	}
}

// dueReminder seeds an issue and a reminder that is already due.
func dueReminder(t *testing.T, pool *pgxpool.Pool, store *Store, owner uuid.UUID, pid uuid.UUID, prefix, key string) (string, uuid.UUID) {
	t.Helper()
	iid := prefix + "-" + key
	seedIssue(t, pool, pid, owner, iid, "ring me")
	rem, err := store.Create(context.Background(), iid, owner, time.Now().Add(-time.Hour), "", testActor(owner))
	if err != nil {
		t.Fatalf("create due reminder: %v", err)
	}
	return iid, rem.ID
}

// TestScheduler_ClaimDue_WritesPushOnlyPending — the happy path. The row exists,
// it is owed to a device, and it belongs to NO feed: "the feed channel is
// inapplicable — old rings in the feed would depict behaviour that new rings
// no longer produce".
func TestScheduler_ClaimDue_WritesPushOnlyPending(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	iid, remID := dueReminder(t, pool, store, owner, pid, prefix, "a")

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
		t.Fatalf("ClaimAndNotifyDue: %v", err)
	}

	row := readReminderNotification(t, pool, owner, iid)
	if !row.found {
		t.Fatal("no notification row for a rung reminder")
	}
	if row.reason == nil || *row.reason != "reminder" {
		t.Errorf("reason = %v, want \"reminder\"", row.reason)
	}
	if row.feedChannel {
		t.Error("feed_channel = true — \"Reminder\" has no feed channel at all")
	}
	if row.pushState != "pending" || !row.hasDeadline {
		t.Errorf("push_state/deadline = %q/%v, want pending with a deadline", row.pushState, row.hasDeadline)
	}
	if notifiedAt(t, pool, remID) == nil {
		t.Error("notified_at was not stamped for a rung reminder")
	}
}

// TestScheduler_ClaimDue_PushDisabledWritesNoRowButStillRings — a preference
// read that SUCCEEDS and says "off" is not a failure. Both channels of the
// reason are then off, so INV-1 forbids the row; but the ring itself happened,
// so notified_at is stamped and the interface's ringing state — which lives on
// the reminder, not on the notification — is untouched.
func TestScheduler_ClaimDue_PushDisabledWritesNoRowButStillRings(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	iid, remID := dueReminder(t, pool, store, owner, pid, prefix, "b")

	setReminderPush(t, pool, owner, false)

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})
	rung, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor)
	if err != nil {
		t.Fatalf("ClaimAndNotifyDue: %v", err)
	}
	if rung == 0 {
		t.Fatal("the reminder was not rung at all")
	}

	if row := readReminderNotification(t, pool, owner, iid); row.found {
		t.Errorf("a notification row was written for a switched-off reason: %+v", row)
	}
	if notifiedAt(t, pool, remID) == nil {
		t.Error("notified_at was not stamped — the ring happened whether or not a push was owed")
	}
	// SC-023: the ringing state the UI shows is independent of the delivery.
	n, err := store.CountRingingTasks(ctx, owner, time.Now())
	if err != nil {
		t.Fatalf("CountRingingTasks: %v", err)
	}
	if n != 1 {
		t.Errorf("ringing tasks = %d, want 1 — \"the ringing state lives on the reminder itself\"", n)
	}
}

// TestScheduler_ClaimDue_PrefsUnavailableBurnsNothing — SC-020, the reason this
// path is an exception.
//
// The preferences cannot be read. The tick must NOT succeed quietly: nothing
// is inserted, notified_at stays empty, and the instance is still due — so the
// next tick, once the settings are readable again, rings it. "There are no
// burnt-out reminders."
func TestScheduler_ClaimDue_PrefsUnavailableBurnsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	iid, remID := dueReminder(t, pool, store, owner, pid, prefix, "c")

	// Make the preferences table unreadable for the duration of the case. The
	// rename is restored by the cleanup below, which runs on a failed
	// assertion too.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE user_notification_reason_prefs RENAME TO unrp_hidden_for_test`); err != nil {
		t.Fatalf("hide preferences table: %v", err)
	}
	restore := func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE IF EXISTS unrp_hidden_for_test RENAME TO user_notification_reason_prefs`)
	}
	t.Cleanup(restore)

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})
	rung, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor)
	if err == nil {
		t.Fatal("the tick reported success with the preferences unreadable — the reminder would be burnt")
	}
	if rung != 0 {
		t.Errorf("rung = %d, want 0", rung)
	}
	if row := readReminderNotification(t, pool, owner, iid); row.found {
		t.Errorf("a notification row survived the aborted tick: %+v", row)
	}
	if at := notifiedAt(t, pool, remID); at != nil {
		t.Errorf("notified_at = %v — the ring must not be recorded as done", at)
	}

	// Settings readable again → the very next tick rings it. Nothing was lost.
	restore()
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
		t.Fatalf("ClaimAndNotifyDue after recovery: %v", err)
	}
	if !readReminderNotification(t, pool, owner, iid).found {
		t.Error("the reminder did not ring after the settings became readable")
	}
	if notifiedAt(t, pool, remID) == nil {
		t.Error("notified_at was not stamped on the recovering tick")
	}
}

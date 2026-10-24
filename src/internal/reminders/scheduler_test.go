package reminders

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// countReminderNotifications returns how many notifications of type
// reminder.rang exist for the given user.
func countReminderNotifications(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = $2`,
		userID, EventReminderRang).Scan(&n)
	if err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

func TestScheduler_ClaimDue_NotifiesStampsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	ctx := context.Background()

	// One due reminder, one future reminder.
	dueIID := prefix + "-1"
	seedIssue(t, pool, pid, owner, dueIID, "due")
	dueRem, err := store.Create(ctx, dueIID, owner, time.Now().Add(-time.Hour), "", testActor(owner))
	if err != nil {
		t.Fatalf("create due: %v", err)
	}
	futureIID := prefix + "-2"
	seedIssue(t, pool, pid, owner, futureIID, "future")
	if _, err := store.Create(ctx, futureIID, owner, time.Now().Add(time.Hour), "", testActor(owner)); err != nil {
		t.Fatalf("create future: %v", err)
	}

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})

	// First tick. ClaimAndNotifyDue is GLOBAL (it claims every due reminder
	// in the table, not just this user's), and the shared test DB may carry
	// due rows seeded by sibling tests — so we assert on THIS user's
	// observable effects, not on the global `rung` count.
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
		t.Fatalf("ClaimAndNotifyDue tick 1: %v", err)
	}
	if got := countReminderNotifications(t, pool, owner); got != 1 {
		t.Fatalf("notifications after tick 1 = %d, want 1", got)
	}
	assertEventExists(t, pool, dueIID, EventReminderRang)

	// notified_at stamped on the due row; the future row stays NULL.
	var notifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM reminders WHERE id = $1`, dueRem.ID).Scan(&notifiedAt); err != nil {
		t.Fatalf("read notified_at: %v", err)
	}
	if notifiedAt == nil {
		t.Fatalf("notified_at not stamped after ring")
	}

	// Second tick: the already-rung reminder is skipped (idempotent), the
	// future one is still not due → this user's notification count is
	// unchanged (no duplicate row).
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
		t.Fatalf("ClaimAndNotifyDue tick 2: %v", err)
	}
	if got := countReminderNotifications(t, pool, owner); got != 1 {
		t.Fatalf("notifications after tick 2 = %d, want 1 (no duplicate)", got)
	}
}

func TestScheduler_SnoozeRearmsRing(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	ctx := context.Background()

	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "x")
	rem, err := store.Create(ctx, iid, owner, time.Now().Add(-time.Hour), "", testActor(owner))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	actor := NewScheduler(store, Config{}).actor

	// Ring once.
	if _, err := store.ClaimAndNotifyDue(ctx, 50, actor); err != nil {
		t.Fatalf("ring 1: %v", err)
	}
	if got := countReminderNotifications(t, pool, owner); got != 1 {
		t.Fatalf("after first ring = %d, want 1", got)
	}

	// Snooze into the PAST (so it's immediately due again) — resets
	// notified_at → the scheduler must re-ring it.
	if _, err := store.Snooze(ctx, rem.ID, owner, time.Now().Add(-time.Minute), "again", testActor(owner)); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if _, err := store.ClaimAndNotifyDue(ctx, 50, actor); err != nil {
		t.Fatalf("ring 2: %v", err)
	}
	if got := countReminderNotifications(t, pool, owner); got != 2 {
		t.Fatalf("after re-ring = %d, want 2", got)
	}
}

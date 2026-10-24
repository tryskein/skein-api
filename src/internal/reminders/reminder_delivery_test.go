package reminders

import (
	"context"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reminder_delivery_test.go — sk-303.13 / T011, coverage-matrix row 22:
// "there are no burnt-out reminders".
//
// A burnt reminder is one marked delivered that was never delivered. There are
// exactly three ways to produce one, and this file closes all three:
//
//  1. the tick stamps notified_at and then fails to write the notification;
//  2. two replicas ring the same instance and one of them half-succeeds;
//  3. the delivery ends without a push and the ring is retroactively cancelled.
//
// The first two are about the transaction; the third is about who owns the
// ringing state — and the answer is the reminder, not the notification.

// TestReminderDelivery_NoBurntReminderAfterAnAbortedTick — the atomicity
// claim, checked as a DATA invariant rather than as a sequence of calls: after
// a tick that could not read the preferences, there is no reminder in the
// batch marked notified, and no notification row from it either.
//
// Two due reminders are used deliberately. The preferences read is one
// statement for the whole batch, so its failure must take the WHOLE batch
// down: a partially applied tick would leave one reminder burnt and the other
// pending, which is the worst of both.
func TestReminderDelivery_NoBurntReminderAfterAnAbortedTick(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	other := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)

	iidA, remA := dueReminder(t, pool, store, owner, pid, prefix, "burna")
	iidB, remB := dueReminder(t, pool, store, other, pid, prefix, "burnb")

	if _, err := pool.Exec(ctx,
		`ALTER TABLE user_notification_reason_prefs RENAME TO unrp_hidden_burnt_test`); err != nil {
		t.Fatalf("hide preferences table: %v", err)
	}
	restore := func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE IF EXISTS unrp_hidden_burnt_test RENAME TO user_notification_reason_prefs`)
	}
	t.Cleanup(restore)

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err == nil {
		t.Fatal("the tick reported success with the preferences unreadable")
	}
	restore()

	for _, c := range []struct {
		name  string
		user  uuid.UUID
		issue string
		rem   uuid.UUID
	}{{"first", owner, iidA, remA}, {"second", other, iidB, remB}} {
		if at := notifiedAt(t, pool, c.rem); at != nil {
			t.Errorf("%s reminder: notified_at = %v after an aborted tick — it is burnt", c.name, at)
		}
		if row := readReminderNotification(t, pool, c.user, c.issue); row.found {
			t.Errorf("%s reminder: a notification survived the aborted tick: %+v", c.name, row)
		}
	}

	// And nothing was lost: one recovered tick rings both.
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
		t.Fatalf("ClaimAndNotifyDue after recovery: %v", err)
	}
	if !readReminderNotification(t, pool, owner, iidA).found ||
		!readReminderNotification(t, pool, other, iidB).found {
		t.Error("a recovered tick did not ring both reminders")
	}
}

// TestReminderDelivery_ConcurrentTicksRingOnce — the replica claim. Two
// schedulers tick the same backlog at the same time; SKIP LOCKED plus the
// same-transaction notified_at stamp mean each instance rings exactly once and
// never ends up stamped without its notification.
func TestReminderDelivery_ConcurrentTicksRingOnce(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)

	const count = 8
	issues := make([]string, 0, count)
	rems := make([]uuid.UUID, 0, count)
	for i := range count {
		iid, rem := dueReminder(t, pool, store, owner, pid, prefix, "cc"+string(rune('a'+i)))
		issues = append(issues, iid)
		rems = append(rems, rem)
	}

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
					t.Errorf("ClaimAndNotifyDue: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for i, iid := range issues {
		var rows int
		if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1 AND e.entity_id = $2 AND e.type = $3`,
			owner, iid, EventReminderRang).Scan(&rows); err != nil {
			t.Fatalf("count reminder notifications: %v", err)
		}
		if rows != 1 {
			t.Errorf("%s: %d reminder notifications, want exactly 1", iid, rows)
		}
		if notifiedAt(t, pool, rems[i]) == nil {
			t.Errorf("%s: notified_at not stamped", iid)
		}
	}
}

// deliveryOutcomeLiteral cuts values in single quotes out of a schema
// constraint definition.
var deliveryOutcomeLiteral = regexp.MustCompile(`'([^']*)'`)

// everyDeliveryOutcome — the ENTIRE closed set of terminal delivery
// outcomes, read from the notifications_push_outcome_chk schema
// constraint.
//
// It is read, not written out. Previously this was a handwritten list of
// four values in the test, which promises BY NAME "every delivery
// outcome": when sk-303.14 introduced supersession, the list did not
// learn about it, the test stayed green, and its name became a lie. The
// reminders package cannot see outbox's unexported constants, so the
// second source of truth is the schema — the very single instance of the
// closed set for which the outcome was introduced by a migration
// (000044), not a constant.
func everyDeliveryOutcome(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`,
		"notifications_push_outcome_chk").Scan(&def); err != nil {
		t.Fatalf("read constraint notifications_push_outcome_chk: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range deliveryOutcomeLiteral.FindAllStringSubmatch(def, -1) {
		if v := m[1]; v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	// A lower bound on parse soundness, not an enumeration of the set:
	// an empty (or almost empty) result would make the promise "every
	// outcome" tautologically true, and this is the only way to notice
	// that.
	if len(out) < 2 {
		t.Fatalf("extracted %d values from the constraint definition (%q) — the parse broke, not the schema",
			len(out), def)
	}
	sort.Strings(out)
	return out
}

// TestReminderDelivery_RingingStateSurvivesEveryDeliveryOutcome — SC-023 and
// the third way to burn a reminder: cancelling the ring because the push did
// not land.
//
// The outcomes are written directly here rather than by running the push
// runner: the runner's own taxonomy is proven in its lifecycle class, and what
// this case is about is the OTHER direction — that nothing in the delivery
// path is allowed to reach back into the reminder. Writing the terminal state
// by hand is the sharpest form of that question.
//
// EVERY outcome means every one, so the set comes from the schema and not
// from a list in this file. `superseded` in particular is the likeliest
// candidate to break the promise: the row is finished by the reminder
// producer, i.e. by the very code that stamps notified_at.
func TestReminderDelivery_RingingStateSurvivesEveryDeliveryOutcome(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})

	for _, outcome := range everyDeliveryOutcome(t, pool) {
		t.Run(outcome, func(t *testing.T) {
			owner := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, owner)
			iid, rem := dueReminder(t, pool, store, owner, pid, prefix, "out")

			if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
				t.Fatalf("ClaimAndNotifyDue: %v", err)
			}
			stampedAt := notifiedAt(t, pool, rem)
			if stampedAt == nil {
				t.Fatal("the reminder did not ring")
			}

			// The delivery ends without a push.
			if _, err := pool.Exec(ctx, `
UPDATE notifications n
   SET push_state = 'final', push_outcome = $1, push_finalized_at = now()
  FROM events e
 WHERE e.id = n.event_id AND n.user_id = $2 AND e.entity_id = $3 AND e.type = $4`,
				outcome, owner, iid, EventReminderRang); err != nil {
				t.Fatalf("finalise delivery: %v", err)
			}
			// The terminal state is cleaned up after itself. The outcome
			// set is closed by a schema constraint, and the 000044
			// rollback deliberately FAILS if by rollback time the table
			// still has even one row with an outcome the previous CHECK
			// did not know: a test that left such a row in the shared
			// database paints someone else's migration package, not its
			// own. The notification row stays — only the outcome is
			// cleared, otherwise the reminder would become "burnt" for
			// the invariant below.
			t.Cleanup(func() {
				if _, err := pool.Exec(context.Background(), `
UPDATE notifications n
   SET push_state = 'none', push_outcome = NULL, push_finalized_at = NULL
  FROM events e
 WHERE e.id = n.event_id AND n.user_id = $1 AND e.entity_id = $2 AND e.type = $3`,
					owner, iid, EventReminderRang); err != nil {
					t.Errorf("clear the terminal outcome after itself: %v", err)
				}
			})

			// The ring is untouched — the signal is not lost, it just did not
			// travel to a device.
			after := notifiedAt(t, pool, rem)
			if after == nil || !after.Equal(*stampedAt) {
				t.Errorf("notified_at changed after the delivery outcome %q: %v → %v", outcome, stampedAt, after)
			}
			n, err := store.CountRingingTasks(ctx, owner, time.Now())
			if err != nil {
				t.Fatalf("CountRingingTasks: %v", err)
			}
			if n != 1 {
				t.Errorf("ringing tasks after outcome %q = %d, want 1 — \"the ringing state lives on the reminder itself\"", outcome, n)
			}
		})
	}
}

// TestReminderDelivery_NoStampWithoutItsNotification — the invariant stated as
// a query over the data, which is how a burnt reminder would be found in
// production: a reminder whose owner has the push channel on, marked notified,
// with no notification row to show for it.
func TestReminderDelivery_NoStampWithoutItsNotification(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	setReminderPush(t, pool, owner, true)

	for i := range 4 {
		dueReminder(t, pool, store, owner, pid, prefix, "inv"+string(rune('a'+i)))
	}

	sched := NewScheduler(store, Config{Interval: time.Minute, BatchSize: 50})
	if _, err := store.ClaimAndNotifyDue(ctx, 1000, sched.actor); err != nil {
		t.Fatalf("ClaimAndNotifyDue: %v", err)
	}

	var burnt int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM reminders r
 WHERE r.user_id = $1
   AND r.notified_at IS NOT NULL
   AND NOT EXISTS (
       SELECT 1
         FROM notifications n
         JOIN events e ON e.id = n.event_id
        WHERE n.user_id = r.user_id
          AND e.entity_id = r.issue_id
          AND e.type = $2
   )`, owner, EventReminderRang).Scan(&burnt); err != nil {
		t.Fatalf("count burnt reminders: %v", err)
	}
	if burnt != 0 {
		t.Errorf("%d reminders are marked notified with no notification behind them", burnt)
	}
}

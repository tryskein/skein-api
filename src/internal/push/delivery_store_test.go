package push_test

// delivery_store_test.go — the delivery side of the subscription store
// (integration, TEST_DATABASE_URL). The reference is the specification of
// device consent: INV-6 (a delivery attempt selects only subscriptions of a
// user who is neither deactivated nor deleted), INV-11 (a "the address is
// dead" answer does not remove a subscription registered or updated after
// the request it answers), Edge 5 and SC-008 (subscriptions registered before
// the release, without a device label, keep receiving deliveries).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/push"
)

// markInactive puts user into the inactive state named by how.
func markInactive(t *testing.T, w txWorld, user uuid.UUID, how string) {
	t.Helper()
	switch how {
	case "deactivated":
		w.exec(t, `UPDATE users SET deactivated_at = now() WHERE id = $1`, user)
	case "deleted":
		w.exec(t, `UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(random()::text), 1, 8) WHERE id = $1`, user)
	default:
		t.Fatalf("unknown inactive state %q", how)
	}
}

// Row 25 — INV-6, SC-008: the subscriptions of an active user are selected,
// the unlabelled one of a registration made before the release among them,
// each with the id and version the store holds; a deactivated or deleted
// user has none selected although the records are still there.
func TestListDeliverableForUser_SelectsOnlyAnActiveOwnersSubscriptions(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()

	active, _ := w.user(t)
	labelled := w.seedSub(t, active, newEndpoint(), labelPtr(uuid.New()))
	unlabelled := w.seedSub(t, active, newEndpoint(), nil)
	w.exec(t, `UPDATE push_subscriptions SET version = 4 WHERE id = $1`, labelled)

	got, err := w.store.ListDeliverableForUser(ctx, active)
	if err != nil {
		t.Fatalf("ListDeliverableForUser(active): %v", err)
	}
	want := map[uuid.UUID]int64{labelled: 4, unlabelled: 1}
	if len(got) != len(want) {
		t.Fatalf("selected %d subscriptions of the active user, want %d: %+v", len(got), len(want), got)
	}
	for _, s := range got {
		v, ok := want[s.ID]
		if !ok {
			t.Fatalf("selected subscription %s is not the active user's", s.ID)
		}
		if s.Version != v || s.UserID != active {
			t.Errorf("subscription %s selected with version %d, owner %s; want version %d, owner %s",
				s.ID, s.Version, s.UserID, v, active)
		}
		delete(want, s.ID)
	}

	for _, how := range []string{"deactivated", "deleted"} {
		t.Run(how, func(t *testing.T) {
			user, _ := w.user(t)
			kept := []uuid.UUID{
				w.seedSub(t, user, newEndpoint(), labelPtr(uuid.New())),
				w.seedSub(t, user, newEndpoint(), nil),
			}
			markInactive(t, w, user, how)

			got, err := w.store.ListDeliverableForUser(ctx, user)
			if err != nil {
				t.Fatalf("ListDeliverableForUser(%s): %v", how, err)
			}
			if len(got) != 0 {
				t.Fatalf("a %s user has %d subscriptions selected for delivery, want 0", how, len(got))
			}
			// The records exist: the selection, not their absence, excludes them.
			for _, id := range kept {
				if !w.exists(t, id) {
					t.Fatalf("seeded subscription %s of the %s user is missing — the case proves nothing", id, how)
				}
			}
		})
	}
}

// Row 25 — INV-11, Edge 5: a gone answer removes the record only while it is
// the one that was sent to — same id and version; a registration repeated
// after the send (version raised) and a record created anew at the same
// endpoint (another id) both survive it.
func TestDeleteGone_RemovesOnlyTheRecordThatWasSentTo(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()

	t.Run("same id and version", func(t *testing.T) {
		user, _ := w.user(t)
		id := w.seedSub(t, user, newEndpoint(), nil)
		removed, err := w.store.DeleteGone(ctx, id, 1)
		if err != nil {
			t.Fatalf("DeleteGone: %v", err)
		}
		if !removed || w.exists(t, id) {
			t.Fatalf("DeleteGone(id, 1) on the record at version 1: removed=%v, still present=%v; want removed",
				removed, w.exists(t, id))
		}
	})

	t.Run("registered again after the send", func(t *testing.T) {
		user, epoch := w.user(t)
		endpoint := newEndpoint()
		label := labelPtr(uuid.New())
		sent, err := register(w, user, epoch, endpoint, label, push.ModeUpsert, "first")
		if err != nil {
			t.Fatalf("first registration: %v", err)
		}
		if _, err := register(w, user, epoch, endpoint, label, push.ModeUpsert, "again"); err != nil {
			t.Fatalf("repeated registration: %v", err)
		}
		removed, err := w.store.DeleteGone(ctx, sent.ID, sent.Version)
		if err != nil {
			t.Fatalf("DeleteGone: %v", err)
		}
		row, ok := w.byEndpoint(t, endpoint)
		if removed || !ok {
			t.Fatalf("late gone answer removed a record registered again after the send (removed=%v, present=%v)", removed, ok)
		}
		if row.ID != sent.ID || row.Version != sent.Version+1 || row.P256dh != "p256dh-again" {
			t.Fatalf("record after the late answer = %+v, want id %s at version %d with the repeated keys",
				row, sent.ID, sent.Version+1)
		}
	})

	t.Run("created anew at the same endpoint", func(t *testing.T) {
		user, _ := w.user(t)
		endpoint := newEndpoint()
		old := w.seedSub(t, user, endpoint, nil)
		w.exec(t, `DELETE FROM push_subscriptions WHERE id = $1`, old)
		fresh := w.seedSub(t, user, endpoint, nil)

		removed, err := w.store.DeleteGone(ctx, old, 1)
		if err != nil {
			t.Fatalf("DeleteGone: %v", err)
		}
		if removed || !w.exists(t, fresh) {
			t.Fatalf("late gone answer for the old record removed the one created anew (removed=%v, present=%v)",
				removed, w.exists(t, fresh))
		}
	})
}

// Review fix — PurgeInactiveOwners against a concurrent switch-on: the owner
// is switched on again and refreshes their own subscription in a transaction
// the purge has to wait for (it holds the user's row and the subscription's
// row). Once that transaction commits, the subscription belongs to an active
// user and the purge must leave it, while another, still inactive user's
// subscription goes. A purge that decided on the inactivity it saw before the
// commit deletes the refreshed subscription.
func TestPurgeInactiveOwners_LeavesTheSubscriptionOfAUserSwitchedOnMeanwhile(t *testing.T) {
	w := newTxWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	user, _ := w.user(t)
	endpoint := newEndpoint()
	sub := w.seedSub(t, user, endpoint, labelPtr(uuid.New()))
	w.exec(t, `UPDATE users SET deactivated_at = now() WHERE id = $1`, user)

	stillInactive, _ := w.user(t)
	control := w.seedSub(t, stillInactive, newEndpoint(), nil)
	w.exec(t, `UPDATE users SET deactivated_at = now() WHERE id = $1`, stillInactive)

	switchOn, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the switch-on: %v", err)
	}
	defer func() { _ = switchOn.Rollback(context.Background()) }()
	if _, err := switchOn.Exec(ctx, `UPDATE users SET deactivated_at = NULL WHERE id = $1`, user); err != nil {
		t.Fatalf("switch the user on: %v", err)
	}
	if _, err := switchOn.Exec(ctx, `
UPDATE push_subscriptions SET p256dh = 'refreshed', version = version + 1, last_seen_at = now()
 WHERE id = $1`, sub); err != nil {
		t.Fatalf("refresh the subscription: %v", err)
	}
	var holder int32
	if err := switchOn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatalf("read the switch-on pid: %v", err)
	}

	purgeStore, purgeGate := gatedStore(t, false)
	purged := make(chan error, 1)
	go func() {
		_, err := purgeStore.PurgeInactiveOwners(ctx)
		purged <- err
	}()
	for {
		select {
		case err := <-purged:
			t.Fatalf("the purge finished without waiting for the switch-on: %v", err)
		case <-ctx.Done():
			t.Fatal("the purge never waited for the switch-on")
		default:
		}
		if waiter := purgeGate.pid.Load(); waiter != 0 {
			var blocked bool
			if err := w.pool.QueryRow(ctx,
				`SELECT $1::int = ANY(pg_blocking_pids($2::int))`, holder, waiter).Scan(&blocked); err != nil {
				t.Fatalf("read blocking pids: %v", err)
			}
			if blocked {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := switchOn.Commit(ctx); err != nil {
		t.Fatalf("commit the switch-on: %v", err)
	}
	if err := <-purged; err != nil {
		t.Fatalf("PurgeInactiveOwners: %v", err)
	}

	row, ok := w.byEndpoint(t, endpoint)
	if !ok || row.ID != sub || row.Version != 2 || row.P256dh != "refreshed" {
		t.Fatalf("the subscription refreshed by the user switched on: present=%v %+v; want it kept at version 2", ok, row)
	}
	if w.exists(t, control) {
		t.Error("the subscription of the user still inactive survived the purge")
	}
}

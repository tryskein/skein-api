package push_test

// store_tx_test.go — the transactional write path of push subscriptions
// (integration, TEST_DATABASE_URL). The reference is the specification of
// device consent: the revocation composition rule (items 2, 3, 6), INV-1,
// INV-4, INV-10, R-3 – R-7, Edge 1 and the meaning of 204 of the push REST
// contract, all written before this code.

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
	"skein-api/internal/testsupport"
)

type txWorld struct {
	pool  *pgxpool.Pool
	store *push.Store
}

func newTxWorld(t *testing.T) txWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	return txWorld{pool: pool, store: push.NewStore(pool)}
}

// user seeds an active user and returns it with its current session epoch.
func (w txWorld) user(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	id, _ := testsupport.SeedUser(t, w.pool, "user")
	return id, w.epoch(t, id)
}

func (w txWorld) epoch(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	var epoch int64
	if err := w.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, id).Scan(&epoch); err != nil {
		t.Fatalf("read session epoch of %s: %v", id, err)
	}
	return epoch
}

func (w txWorld) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func newEndpoint() string { return "https://push.test.local/" + uuid.NewString() }

func labelPtr(l uuid.UUID) *uuid.UUID { return &l }

// seedSub writes a subscription of owner with endpoint and label (nil: none)
// at version 1 and returns its id.
func (w txWorld) seedSub(t *testing.T, owner uuid.UUID, endpoint string, label *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	w.exec(t, `
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth, device_label)
VALUES ($1, $2, $3, 'seed-p256dh', 'seed-auth', $4)`, id, owner, endpoint, label)
	return id
}

// subRow is a stored subscription as the database holds it.
type subRow struct {
	ID      uuid.UUID
	Owner   uuid.UUID
	P256dh  string
	Auth    string
	Label   *uuid.UUID
	Version int64
}

func (w txWorld) byEndpoint(t *testing.T, endpoint string) (subRow, bool) {
	t.Helper()
	var r subRow
	err := w.pool.QueryRow(context.Background(), `
SELECT id, user_id, p256dh, auth, device_label, version FROM push_subscriptions WHERE endpoint = $1`, endpoint).
		Scan(&r.ID, &r.Owner, &r.P256dh, &r.Auth, &r.Label, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return subRow{}, false
	}
	if err != nil {
		t.Fatalf("read subscription by endpoint: %v", err)
	}
	return r, true
}

func (w txWorld) exists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM push_subscriptions WHERE id = $1`, id) == 1
}

func (w txWorld) revoked(t *testing.T, user, label uuid.UUID) bool {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM push_device_revocations WHERE user_id = $1 AND device_label = $2`, user, label) == 1
}

func (w txWorld) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// userState is every subscription and revocation of one user.
type userState struct {
	subs        map[uuid.UUID]subRow
	revocations map[uuid.UUID]bool
}

func (w txWorld) state(t *testing.T, user uuid.UUID) userState {
	t.Helper()
	ctx := context.Background()
	st := userState{subs: map[uuid.UUID]subRow{}, revocations: map[uuid.UUID]bool{}}
	rows, err := w.pool.Query(ctx, `
SELECT id, user_id, p256dh, auth, device_label, version FROM push_subscriptions WHERE user_id = $1`, user)
	if err != nil {
		t.Fatalf("read subscriptions: %v", err)
	}
	for rows.Next() {
		var r subRow
		if err := rows.Scan(&r.ID, &r.Owner, &r.P256dh, &r.Auth, &r.Label, &r.Version); err != nil {
			t.Fatalf("scan subscription: %v", err)
		}
		st.subs[r.ID] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read subscriptions: %v", err)
	}
	labels, err := w.pool.Query(ctx, `SELECT device_label FROM push_device_revocations WHERE user_id = $1`, user)
	if err != nil {
		t.Fatalf("read revocations: %v", err)
	}
	for labels.Next() {
		var l uuid.UUID
		if err := labels.Scan(&l); err != nil {
			t.Fatalf("scan revocation: %v", err)
		}
		st.revocations[l] = true
	}
	labels.Close()
	return st
}

func sameLabel(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameRow(x, y subRow) bool {
	return x.ID == y.ID && x.Owner == y.Owner && x.P256dh == y.P256dh && x.Auth == y.Auth &&
		x.Version == y.Version && sameLabel(x.Label, y.Label)
}

func sameState(a, b userState) bool {
	if len(a.subs) != len(b.subs) || len(a.revocations) != len(b.revocations) {
		return false
	}
	for id, x := range a.subs {
		if y, ok := b.subs[id]; !ok || !sameRow(x, y) {
			return false
		}
	}
	for l := range a.revocations {
		if !b.revocations[l] {
			return false
		}
	}
	return true
}

func register(w txWorld, user uuid.UUID, epoch int64, endpoint string, label *uuid.UUID, mode, key string) (push.Subscription, error) {
	return w.store.Register(context.Background(), push.RegisterInput{
		UserID: user, SessionEpoch: epoch, Endpoint: endpoint,
		P256dh: "p256dh-" + key, Auth: "auth-" + key, DeviceLabel: label, Mode: mode,
	})
}

// Row 2 — INV-1, R-3, US-6: a registration of the caller's own endpoint
// creates the subscription with the label at version 1, a repeat refreshes
// keys and label and raises the version; the endpoint of another user's
// subscription is refused and that subscription is unchanged.
func TestRegister_OwnEndpointCreatesThenUpdates_ForeignIsTaken(t *testing.T) {
	w := newTxWorld(t)
	owner, ownerEpoch := w.user(t)
	other, otherEpoch := w.user(t)
	endpoint := newEndpoint()
	l1, l2 := uuid.New(), uuid.New()

	created, err := register(w, owner, ownerEpoch, endpoint, &l1, push.ModeUpsert, "a")
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if created.UserID != owner || !sameLabel(created.DeviceLabel, &l1) || created.Version != 1 ||
		created.P256dh != "p256dh-a" || created.Auth != "auth-a" {
		t.Fatalf("first registration stored %+v, want owner %s, label %s, version 1, keys a", created, owner, l1)
	}

	updated, err := register(w, owner, ownerEpoch, endpoint, &l2, push.ModeUpsert, "b")
	if err != nil {
		t.Fatalf("repeat registration: %v", err)
	}
	if updated.ID != created.ID || !sameLabel(updated.DeviceLabel, &l2) || updated.Version != 2 ||
		updated.P256dh != "p256dh-b" || updated.Auth != "auth-b" {
		t.Fatalf("repeat registration stored %+v, want id %s, label %s, version 2, keys b", updated, created.ID, l2)
	}

	before, _ := w.byEndpoint(t, endpoint)
	for _, mode := range []string{push.ModeUpsert, push.ModeUpdateOnly} {
		if _, err := register(w, other, otherEpoch, endpoint, labelPtr(uuid.New()), mode, "c"); !errors.Is(err, push.ErrEndpointTaken) {
			t.Errorf("%s registration of another user's endpoint: err %v, want ErrEndpointTaken", mode, err)
		}
	}
	after, ok := w.byEndpoint(t, endpoint)
	if !ok || !sameRow(after, before) {
		t.Errorf("subscription after a foreign registration = %+v (present %v), want unchanged %+v", after, ok, before)
	}
	if n := w.count(t, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, other); n != 0 {
		t.Errorf("subscriptions of the refused user = %d, want 0", n)
	}
}

// Row 3 — R-7, US-9, Edge 1 (old worker), transitional contract «new api +
// old page»: updateOnly and the old form (no label) never create a
// subscription; they refresh the caller's own one, the old form keeping its
// label.
func TestRegister_UpdateOnlyAndOldFormNeverCreate(t *testing.T) {
	w := newTxWorld(t)
	owner, epoch := w.user(t)
	other, _ := w.user(t)
	label, kept := uuid.New(), uuid.New()

	t.Run("updateOnly without an own subscription is not found and creates nothing", func(t *testing.T) {
		endpoint := newEndpoint()
		if _, err := register(w, owner, epoch, endpoint, &label, push.ModeUpdateOnly, "x"); !errors.Is(err, push.ErrSubscriptionNotFound) {
			t.Fatalf("err %v, want ErrSubscriptionNotFound", err)
		}
		if _, ok := w.byEndpoint(t, endpoint); ok {
			t.Error("a subscription was created")
		}
	})
	t.Run("updateOnly on the own unlabelled subscription sets the label and the keys", func(t *testing.T) {
		endpoint := newEndpoint()
		id := w.seedSub(t, owner, endpoint, nil)
		if _, err := register(w, owner, epoch, endpoint, &label, push.ModeUpdateOnly, "u"); err != nil {
			t.Fatalf("err %v, want nil", err)
		}
		r, _ := w.byEndpoint(t, endpoint)
		if r.ID != id || !sameLabel(r.Label, &label) || r.P256dh != "p256dh-u" || r.Auth != "auth-u" {
			t.Errorf("stored %+v, want id %s with label %s and keys u", r, id, label)
		}
	})
	t.Run("old form refreshes the keys of the own subscription and keeps its label", func(t *testing.T) {
		labelled, unlabelled := newEndpoint(), newEndpoint()
		w.seedSub(t, owner, labelled, &kept)
		w.seedSub(t, owner, unlabelled, nil)
		for _, c := range []struct {
			endpoint string
			label    *uuid.UUID
		}{{labelled, &kept}, {unlabelled, nil}} {
			if _, err := register(w, owner, epoch, c.endpoint, nil, "", "o"); err != nil {
				t.Fatalf("err %v, want nil", err)
			}
			r, _ := w.byEndpoint(t, c.endpoint)
			if !sameLabel(r.Label, c.label) || r.P256dh != "p256dh-o" || r.Auth != "auth-o" {
				t.Errorf("stored %+v, want label %v kept and keys o", r, c.label)
			}
		}
	})
	t.Run("old form without an own subscription is not found and creates nothing", func(t *testing.T) {
		endpoint := newEndpoint()
		if _, err := register(w, owner, epoch, endpoint, nil, "", "x"); !errors.Is(err, push.ErrSubscriptionNotFound) {
			t.Fatalf("err %v, want ErrSubscriptionNotFound", err)
		}
		if _, ok := w.byEndpoint(t, endpoint); ok {
			t.Error("a subscription was created")
		}
	})
	t.Run("updateOnly and old form on another user's endpoint are taken", func(t *testing.T) {
		endpoint := newEndpoint()
		w.seedSub(t, other, endpoint, nil)
		for _, label := range []*uuid.UUID{&label, nil} {
			mode := push.ModeUpdateOnly
			if label == nil {
				mode = ""
			}
			if _, err := register(w, owner, epoch, endpoint, label, mode, "x"); !errors.Is(err, push.ErrEndpointTaken) {
				t.Errorf("mode %q: err %v, want ErrEndpointTaken", mode, err)
			}
		}
		if r, _ := w.byEndpoint(t, endpoint); r.Owner != other || r.Label != nil || r.P256dh != "seed-p256dh" || r.Version != 1 {
			t.Errorf("another user's subscription changed: %+v", r)
		}
	})
}

// Row 4 — INV-10, SC-004, R-5: after the user revokes label L a registration
// of that user with L is refused at the former, a new and another user's
// endpoint in both modes; L2 of the same user and L of another user are not
// affected.
func TestRegister_RevokedLabelRefusedAtAnyEndpoint(t *testing.T) {
	w := newTxWorld(t)
	user, epoch := w.user(t)
	other, otherEpoch := w.user(t)
	label, label2 := uuid.New(), uuid.New()
	former := newEndpoint()
	w.seedSub(t, user, former, &label)
	foreign := newEndpoint()
	w.seedSub(t, other, foreign, nil)

	if _, err := w.store.RevokeDevice(context.Background(), user, epoch, []uuid.UUID{label}, ""); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, endpoint := range map[string]string{"former": former, "new": newEndpoint(), "foreign": foreign} {
		for _, mode := range []string{push.ModeUpsert, push.ModeUpdateOnly} {
			if _, err := register(w, user, epoch, endpoint, &label, mode, "r"); !errors.Is(err, push.ErrDeviceRevoked) {
				t.Errorf("%s registration with the revoked label: err %v, want ErrDeviceRevoked", mode, err)
			}
		}
	}
	if n := w.count(t, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, user); n != 0 {
		t.Errorf("subscriptions of the user after refused registrations = %d, want 0", n)
	}
	if _, err := register(w, user, epoch, newEndpoint(), &label2, push.ModeUpsert, "n"); err != nil {
		t.Errorf("registration of the same user with another label: %v, want nil", err)
	}
	if _, err := register(w, other, otherEpoch, newEndpoint(), &label, push.ModeUpsert, "n"); err != nil {
		t.Errorf("registration of another user with the same label: %v, want nil", err)
	}
}

// Row 5 — composition rule item 3 (active at the moment of the write),
// INV-4 (a former epoch leaves no subscription): every write refuses a
// session epoch other than users.session_epoch and an inactive caller with
// ErrSessionStale and changes nothing.
func TestWrites_StaleSessionOrInactiveCallerRefused(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	ops := []struct {
		name string
		do   func(user uuid.UUID, epoch int64, endpoint string, id, label uuid.UUID) error
	}{
		{"Register", func(user uuid.UUID, epoch int64, endpoint string, _, label uuid.UUID) error {
			_, err := register(w, user, epoch, endpoint, &label, push.ModeUpsert, "s")
			return err
		}},
		{"RevokeDevice", func(user uuid.UUID, epoch int64, endpoint string, _, label uuid.UUID) error {
			_, err := w.store.RevokeDevice(ctx, user, epoch, []uuid.UUID{label, uuid.New()}, endpoint)
			return err
		}},
		{"RemoveChannel", func(user uuid.UUID, epoch int64, endpoint string, _, _ uuid.UUID) error {
			_, err := w.store.RemoveChannel(ctx, user, epoch, endpoint, nil, true)
			return err
		}},
		{"RemoveDeviceByID", func(user uuid.UUID, epoch int64, _ string, id, _ uuid.UUID) error {
			_, err := w.store.RemoveDeviceByID(ctx, user, epoch, id)
			return err
		}},
	}
	callers := []struct {
		name  string
		setup func(t *testing.T, user uuid.UUID, epoch int64) int64
	}{
		{"session epoch raised after the session began", func(t *testing.T, user uuid.UUID, epoch int64) int64 {
			w.exec(t, `UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1`, user)
			return epoch
		}},
		{"deactivated caller", func(t *testing.T, user uuid.UUID, epoch int64) int64 {
			w.exec(t, `UPDATE users SET deactivated_at = now() WHERE id = $1`, user)
			return epoch
		}},
	}
	for _, op := range ops {
		for _, c := range callers {
			t.Run(op.name+"/"+c.name, func(t *testing.T) {
				user, epoch := w.user(t)
				label := uuid.New()
				endpoint := newEndpoint()
				id := w.seedSub(t, user, endpoint, &label)
				passed := c.setup(t, user, epoch)
				before := w.state(t, user)

				if err := op.do(user, passed, endpoint, id, label); !errors.Is(err, push.ErrSessionStale) {
					t.Fatalf("err %v, want ErrSessionStale", err)
				}
				if after := w.state(t, user); !sameState(before, after) {
					t.Errorf("state changed: before %+v, after %+v", before, after)
				}
			})
		}
	}
}

// Row 6 — composition rule item 2 (device revocation), US-3, US-4
// («revoked even when there is no subscription»), postcondition of 204 of
// a revocation.
func TestRevokeDevice_RevokesLabelsAndRemovesTheDevice(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	user, epoch := w.user(t)
	other, otherEpoch := w.user(t)
	l1, l2, l3 := uuid.New(), uuid.New(), uuid.New()

	withL1 := []uuid.UUID{w.seedSub(t, user, newEndpoint(), &l1), w.seedSub(t, user, newEndpoint(), &l1)}
	withL2 := w.seedSub(t, user, newEndpoint(), &l2)
	presented := newEndpoint()
	unlabelledPresented := w.seedSub(t, user, presented, nil)
	withL3 := w.seedSub(t, user, newEndpoint(), &l3)
	unlabelledOther := w.seedSub(t, user, newEndpoint(), nil)
	otherWithL1 := w.seedSub(t, other, newEndpoint(), &l1)

	removed, err := w.store.RevokeDevice(ctx, user, epoch, []uuid.UUID{l1, l2}, presented)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if removed != 4 {
		t.Errorf("removed = %d, want 4", removed)
	}
	for _, id := range append(withL1, withL2, unlabelledPresented) {
		if w.exists(t, id) {
			t.Errorf("subscription %s of the revoked device remained", id)
		}
	}
	for _, id := range []uuid.UUID{withL3, unlabelledOther, otherWithL1} {
		if !w.exists(t, id) {
			t.Errorf("subscription %s outside the revoked device was deleted", id)
		}
	}
	for _, l := range []uuid.UUID{l1, l2} {
		if !w.revoked(t, user, l) {
			t.Errorf("label %s not revoked for the user", l)
		}
	}
	if w.revoked(t, other, l1) {
		t.Error("the label was revoked for another user")
	}

	t.Run("a label without subscriptions is revoked", func(t *testing.T) {
		fresh := uuid.New()
		if n, err := w.store.RevokeDevice(ctx, user, epoch, []uuid.UUID{fresh}, ""); err != nil || n != 0 {
			t.Fatalf("revoke: removed %d, err %v; want 0, nil", n, err)
		}
		if _, err := register(w, user, epoch, newEndpoint(), &fresh, push.ModeUpsert, "z"); !errors.Is(err, push.ErrDeviceRevoked) {
			t.Errorf("registration with the label: err %v, want ErrDeviceRevoked", err)
		}
	})
	t.Run("the presented endpoint of another user's or a labelled subscription is kept", func(t *testing.T) {
		otherEndpoint, labelledEndpoint := newEndpoint(), newEndpoint()
		othersUnlabelled := w.seedSub(t, other, otherEndpoint, nil)
		ownLabelled := w.seedSub(t, user, labelledEndpoint, &l3)
		for _, e := range []string{otherEndpoint, labelledEndpoint} {
			if _, err := w.store.RevokeDevice(ctx, user, epoch, []uuid.UUID{uuid.New()}, e); err != nil {
				t.Fatalf("revoke: %v", err)
			}
		}
		if !w.exists(t, othersUnlabelled) || !w.exists(t, ownLabelled) {
			t.Errorf("kept: another user's unlabelled %v, own labelled %v; want both", w.exists(t, othersUnlabelled), w.exists(t, ownLabelled))
		}
		if _, err := register(w, other, otherEpoch, otherEndpoint, nil, "", "k"); err != nil {
			t.Errorf("another user's subscription is no longer registrable: %v", err)
		}
	})
	t.Run("a repeat is idempotent", func(t *testing.T) {
		before := w.state(t, user)
		if n, err := w.store.RevokeDevice(ctx, user, epoch, []uuid.UUID{l1, l2}, presented); err != nil || n != 0 {
			t.Fatalf("repeat: removed %d, err %v; want 0, nil", n, err)
		}
		if after := w.state(t, user); !sameState(before, after) {
			t.Errorf("state changed by a repeat: before %+v, after %+v", before, after)
		}
	})
}

// Row 8 — R-4, composition rule items 2 and 6 (guard), US-6.
func TestRemoveChannel_RemovesWithoutRevoking(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	owner, ownerEpoch := w.user(t)
	caller, callerEpoch := w.user(t)

	t.Run("new form removes whoever owns it, revokes nothing and names the owner", func(t *testing.T) {
		label := uuid.New()
		endpoint := newEndpoint()
		w.seedSub(t, owner, endpoint, &label)
		sibling := w.seedSub(t, owner, newEndpoint(), &label)

		got, err := w.store.RemoveChannel(ctx, caller, callerEpoch, endpoint, nil, true)
		if err != nil {
			t.Fatalf("remove: %v", err)
		}
		if got == nil || *got != owner {
			t.Errorf("owner = %v, want %s", got, owner)
		}
		if _, ok := w.byEndpoint(t, endpoint); ok {
			t.Error("the subscription with the endpoint remained")
		}
		if !w.exists(t, sibling) {
			t.Error("another subscription of the owner was deleted")
		}
		if n := w.count(t, `SELECT count(*) FROM push_device_revocations WHERE user_id = $1`, owner); n != 0 {
			t.Errorf("revocations of the owner = %d, want 0", n)
		}
		if _, err := register(w, owner, ownerEpoch, endpoint, &label, push.ModeUpsert, "back"); err != nil {
			t.Errorf("the owner registers the same label again: %v, want nil", err)
		}
	})
	t.Run("old form removes only the caller's own", func(t *testing.T) {
		endpoint := newEndpoint()
		foreign := w.seedSub(t, owner, endpoint, nil)
		if got, err := w.store.RemoveChannel(ctx, caller, callerEpoch, endpoint, nil, false); err != nil || got != nil {
			t.Fatalf("old form on another user's endpoint: owner %v, err %v; want nil, nil", got, err)
		}
		if !w.exists(t, foreign) {
			t.Error("old form removed another user's subscription")
		}
		own := newEndpoint()
		w.seedSub(t, caller, own, nil)
		if _, err := w.store.RemoveChannel(ctx, caller, callerEpoch, own, nil, false); err != nil {
			t.Fatalf("old form on the own endpoint: %v", err)
		}
		if _, ok := w.byEndpoint(t, own); ok {
			t.Error("old form kept the caller's own subscription")
		}
	})
	t.Run("guard removes only on matching id and version", func(t *testing.T) {
		endpoint := newEndpoint()
		label := uuid.New()
		id := w.seedSub(t, owner, endpoint, &label)
		if _, err := register(w, owner, ownerEpoch, endpoint, &label, push.ModeUpsert, "v2"); err != nil {
			t.Fatalf("raise the version: %v", err)
		}
		for name, guard := range map[string]push.VersionGuard{
			"an older version":                  {ID: id, Version: 1},
			"another id at the current version": {ID: uuid.New(), Version: 2},
		} {
			got, err := w.store.RemoveChannel(ctx, caller, callerEpoch, endpoint, &guard, true)
			if err != nil || got != nil {
				t.Errorf("guard with %s: owner %v, err %v; want nil, nil", name, got, err)
			}
			if !w.exists(t, id) {
				t.Fatalf("guard with %s removed the subscription", name)
			}
		}
		got, err := w.store.RemoveChannel(ctx, caller, callerEpoch, endpoint, &push.VersionGuard{ID: id, Version: 2}, true)
		if err != nil || got == nil || *got != owner {
			t.Fatalf("matching guard: owner %v, err %v; want %s, nil", got, err, owner)
		}
		if w.exists(t, id) {
			t.Error("matching guard kept the subscription")
		}
	})
	t.Run("no subscription with the endpoint is the same success", func(t *testing.T) {
		if got, err := w.store.RemoveChannel(ctx, caller, callerEpoch, newEndpoint(), nil, true); err != nil || got != nil {
			t.Errorf("owner %v, err %v; want nil, nil", got, err)
		}
	})
}

// Row 10 — US-3 «removing another device», postcondition of 204 of a
// removal by id.
func TestRemoveDeviceByID_RemovesTheDeviceAndRevokesItsLabel(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	user, epoch := w.user(t)
	other, _ := w.user(t)
	label, label2 := uuid.New(), uuid.New()

	target := w.seedSub(t, user, newEndpoint(), &label)
	sameDevice := w.seedSub(t, user, newEndpoint(), &label)
	otherDevice := w.seedSub(t, user, newEndpoint(), &label2)
	unlabelled := w.seedSub(t, user, newEndpoint(), nil)
	foreign := w.seedSub(t, other, newEndpoint(), &label)

	if _, err := w.store.RemoveDeviceByID(ctx, user, epoch, target); err != nil {
		t.Fatalf("remove by id: %v", err)
	}
	if w.exists(t, target) || w.exists(t, sameDevice) {
		t.Errorf("after removal: target %v, same-label %v; want neither", w.exists(t, target), w.exists(t, sameDevice))
	}
	if !w.revoked(t, user, label) {
		t.Error("the label of the removed subscription is not revoked")
	}
	if !w.exists(t, otherDevice) || !w.exists(t, unlabelled) || !w.exists(t, foreign) {
		t.Error("a subscription outside the removed device was deleted")
	}
	if w.revoked(t, other, label) {
		t.Error("the label was revoked for another user")
	}

	t.Run("an unlabelled subscription is removed without a revocation", func(t *testing.T) {
		before := len(w.state(t, user).revocations)
		if _, err := w.store.RemoveDeviceByID(ctx, user, epoch, unlabelled); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if w.exists(t, unlabelled) {
			t.Error("the subscription remained")
		}
		if after := len(w.state(t, user).revocations); after != before {
			t.Errorf("revocations %d → %d, want unchanged", before, after)
		}
	})
	t.Run("another user's id and a missing id are not found", func(t *testing.T) {
		for name, id := range map[string]uuid.UUID{"another user's": foreign, "missing": uuid.New()} {
			if _, err := w.store.RemoveDeviceByID(ctx, user, epoch, id); !errors.Is(err, push.ErrSubscriptionNotFound) {
				t.Errorf("%s id: err %v, want ErrSubscriptionNotFound", name, err)
			}
		}
		if !w.exists(t, foreign) {
			t.Error("another user's subscription was deleted")
		}
	})
}

// commitGate is a query tracer of a test pool: it records the backend pid
// of every statement and, when armed, holds the first COMMIT of the pool
// until released — the transaction keeps its locks while it is held.
type commitGate struct {
	pid      atomic.Int32
	armed    atomic.Bool
	held     chan struct{}
	release  chan struct{}
	released sync.Once
}

func (g *commitGate) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	g.pid.Store(int32(conn.PgConn().PID()))
	if data.SQL == "commit" && g.armed.CompareAndSwap(true, false) {
		close(g.held)
		<-g.release
	}
	return ctx
}

func (g *commitGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (g *commitGate) open() { g.released.Do(func() { close(g.release) }) }

// gatedStore is a Store over a one-connection pool traced by a commitGate;
// hold arms the gate on the first commit.
func gatedStore(t *testing.T, hold bool) (*push.Store, *commitGate) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatalf("parse %s: %v", testsupport.TestDatabaseURL, err)
	}
	g := &commitGate{held: make(chan struct{}), release: make(chan struct{})}
	g.armed.Store(hold)
	cfg.ConnConfig.Tracer = g
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(g.open)
	return push.NewStore(pool), g
}

// overlap runs first and second as two product transactions that overlap:
// first runs until its commit and is held there with its locks; second
// starts and the test waits until the pid of first is among
// pg_blocking_pids of second; then first commits. Returns both errors.
func overlap(t *testing.T, w txWorld, first, second func(*push.Store) error) (error, error) {
	t.Helper()
	firstStore, firstGate := gatedStore(t, true)
	secondStore, secondGate := gatedStore(t, false)
	deadline := time.After(15 * time.Second)

	firstDone := make(chan error, 1)
	go func() { firstDone <- first(firstStore) }()
	select {
	case <-firstGate.held:
	case err := <-firstDone:
		t.Fatalf("first transaction ended before its commit was held: %v", err)
	case <-deadline:
		t.Fatal("first transaction did not reach its commit")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- second(secondStore) }()
	holder := firstGate.pid.Load()
	for {
		select {
		case err := <-secondDone:
			t.Fatalf("second transaction ended without waiting for the first: %v", err)
		case <-deadline:
			t.Fatal("second transaction never waited for the first")
		default:
		}
		if waiter := secondGate.pid.Load(); waiter != 0 {
			var blocked bool
			if err := w.pool.QueryRow(context.Background(),
				`SELECT $1::int = ANY(pg_blocking_pids($2::int))`, holder, waiter).Scan(&blocked); err != nil {
				t.Fatalf("read blocking pids: %v", err)
			}
			if blocked {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	firstGate.open()
	return <-firstDone, <-secondDone
}

// Row 7 — Edge 1, US-4 («a registration in flight after leaving leaves no
// subscription»), INV-10 at the moment of the write: Register(L) and
// RevokeDevice(L) in two overlapping transactions.
func TestRegisterAndRevokeDevice_OverlappingLeaveNoSubscription(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	cases := []struct {
		name           string
		revokeFirst    bool
		wantRegisterIs error
	}{
		{"revocation, then registration", true, push.ErrDeviceRevoked},
		{"registration, then revocation", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, epoch := w.user(t)
			label := uuid.New()
			endpoint := newEndpoint()
			w.seedSub(t, user, endpoint, &label)

			revoke := func(s *push.Store) error {
				_, err := s.RevokeDevice(ctx, user, epoch, []uuid.UUID{label}, "")
				return err
			}
			reg := func(s *push.Store) error {
				_, err := s.Register(ctx, push.RegisterInput{
					UserID: user, SessionEpoch: epoch, Endpoint: endpoint,
					P256dh: "p", Auth: "a", DeviceLabel: &label, Mode: push.ModeUpsert,
				})
				return err
			}
			var revokeErr, registerErr error
			if c.revokeFirst {
				revokeErr, registerErr = overlap(t, w, revoke, reg)
			} else {
				registerErr, revokeErr = overlap(t, w, reg, revoke)
			}

			if revokeErr != nil {
				t.Errorf("revocation: %v, want nil", revokeErr)
			}
			if c.wantRegisterIs == nil && registerErr != nil || c.wantRegisterIs != nil && !errors.Is(registerErr, c.wantRegisterIs) {
				t.Errorf("registration: %v, want %v", registerErr, c.wantRegisterIs)
			}
			if n := w.count(t, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1 AND device_label = $2`, user, label); n != 0 {
				t.Errorf("subscriptions with the label after both = %d, want 0", n)
			}
			if !w.revoked(t, user, label) {
				t.Error("the label is not revoked")
			}
		})
	}
}

// Row 9 — the meaning of 204 of an unconditional channel removal (plan
// residual OST-3-5), SC-005: RemoveChannel(E) and Register(E) in two
// overlapping transactions both succeed, and the state is the one of their
// serial order.
func TestRemoveChannelAndRegister_OverlappingFollowSerialOrder(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	for _, c := range []struct {
		name        string
		removeFirst bool
		wantRecord  bool
	}{
		{"removal, then registration", true, true},
		{"registration, then removal", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			owner, epoch := w.user(t)
			caller, callerEpoch := w.user(t)
			label := uuid.New()
			endpoint := newEndpoint()
			w.seedSub(t, owner, endpoint, &label)

			remove := func(s *push.Store) error {
				_, err := s.RemoveChannel(ctx, caller, callerEpoch, endpoint, nil, true)
				return err
			}
			reg := func(s *push.Store) error {
				_, err := s.Register(ctx, push.RegisterInput{
					UserID: owner, SessionEpoch: epoch, Endpoint: endpoint,
					P256dh: "p-new", Auth: "a-new", DeviceLabel: &label, Mode: push.ModeUpsert,
				})
				return err
			}
			var removeErr, registerErr error
			if c.removeFirst {
				removeErr, registerErr = overlap(t, w, remove, reg)
			} else {
				registerErr, removeErr = overlap(t, w, reg, remove)
			}

			if removeErr != nil || registerErr != nil {
				t.Fatalf("removal %v, registration %v; want both nil", removeErr, registerErr)
			}
			r, ok := w.byEndpoint(t, endpoint)
			if ok != c.wantRecord {
				t.Fatalf("record with the endpoint present = %v, want %v", ok, c.wantRecord)
			}
			if ok && (r.Owner != owner || r.P256dh != "p-new" || !sameLabel(r.Label, &label)) {
				t.Errorf("record = %+v, want the registration's (owner %s, keys p-new, label %s)", r, owner, label)
			}
		})
	}
}

// Two registrations of an endpoint no subscription has yet, in two
// overlapping transactions: the second waits on the first's insert of the
// endpoint and, once the first commits, is ordered after it — another
// owner is refused (INV-1), the same owner updates the record.
func TestRegister_OverlappingFirstRegistrationsOfAnEndpoint(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	reg := func(user uuid.UUID, epoch int64, endpoint string, label uuid.UUID, key string) func(*push.Store) error {
		return func(s *push.Store) error {
			_, err := s.Register(ctx, push.RegisterInput{
				UserID: user, SessionEpoch: epoch, Endpoint: endpoint,
				P256dh: "p256dh-" + key, Auth: "auth-" + key, DeviceLabel: &label, Mode: push.ModeUpsert,
			})
			return err
		}
	}

	t.Run("two owners: the second is taken", func(t *testing.T) {
		first, firstEpoch := w.user(t)
		second, secondEpoch := w.user(t)
		endpoint := newEndpoint()
		firstErr, secondErr := overlap(t, w,
			reg(first, firstEpoch, endpoint, uuid.New(), "first"),
			reg(second, secondEpoch, endpoint, uuid.New(), "second"))
		if firstErr != nil {
			t.Errorf("first registration: %v, want nil", firstErr)
		}
		if !errors.Is(secondErr, push.ErrEndpointTaken) {
			t.Errorf("second registration: %v, want ErrEndpointTaken", secondErr)
		}
		r, ok := w.byEndpoint(t, endpoint)
		if !ok || r.Owner != first || r.P256dh != "p256dh-first" || r.Version != 1 {
			t.Errorf("record = %+v (present %v), want the first owner's at version 1", r, ok)
		}
	})
	t.Run("one owner: the second updates the record", func(t *testing.T) {
		user, epoch := w.user(t)
		endpoint := newEndpoint()
		label := uuid.New()
		firstErr, secondErr := overlap(t, w,
			reg(user, epoch, endpoint, label, "first"),
			reg(user, epoch, endpoint, label, "second"))
		if firstErr != nil || secondErr != nil {
			t.Fatalf("registrations: %v, %v; want both nil", firstErr, secondErr)
		}
		r, ok := w.byEndpoint(t, endpoint)
		if !ok || r.Owner != user || r.P256dh != "p256dh-second" || r.Version != 2 {
			t.Errorf("record = %+v (present %v), want the second keys at version 2", r, ok)
		}
		if n := w.count(t, `SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, endpoint); n != 1 {
			t.Errorf("records with the endpoint = %d, want 1", n)
		}
	})
}

// Edge 1, US-4, SC-004: a registration with label L at a new endpoint —
// it shares no row with the revocation, so it waits on nothing — runs to
// its end while RevokeDevice(L) is held at its commit; then the revocation
// commits. Whatever the order the database settles on, no subscription
// with L is left and L is revoked.
func TestRegisterAtANewEndpointAndRevokeDevice_LeaveNoSubscription(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	user, epoch := w.user(t)
	label := uuid.New()
	w.seedSub(t, user, newEndpoint(), &label)

	revokeStore, gate := gatedStore(t, true)
	revokeDone := make(chan error, 1)
	go func() {
		_, err := revokeStore.RevokeDevice(ctx, user, epoch, []uuid.UUID{label}, "")
		revokeDone <- err
	}()
	select {
	case <-gate.held:
	case err := <-revokeDone:
		t.Fatalf("the revocation ended before its commit was held: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the revocation did not reach its commit")
	}

	_, regErr := w.store.Register(ctx, push.RegisterInput{
		UserID: user, SessionEpoch: epoch, Endpoint: newEndpoint(),
		P256dh: "p", Auth: "a", DeviceLabel: &label, Mode: push.ModeUpsert,
	})
	gate.open()
	if err := <-revokeDone; err != nil {
		t.Errorf("revocation: %v, want nil", err)
	}
	if regErr != nil && !errors.Is(regErr, push.ErrDeviceRevoked) {
		t.Errorf("registration: %v, want nil or ErrDeviceRevoked", regErr)
	}
	if n := w.count(t, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1 AND device_label = $2`, user, label); n != 0 {
		t.Errorf("subscriptions with the label after both = %d, want 0", n)
	}
	if !w.revoked(t, user, label) {
		t.Error("the label is not revoked")
	}
}

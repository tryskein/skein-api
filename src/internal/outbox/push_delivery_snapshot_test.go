package outbox

// push_delivery_snapshot_test.go — INV-11 as a whole, from the selection of
// an attempt through the provider's answer to the database row
// (integration, TEST_DATABASE_URL). The runner, the subscription store and
// the sender are the production ones; only the provider boundary is
// replaced: the sender's HTTP client gets a transport that answers, or holds
// the answer until the test has changed the row. The reference is the
// specification of device consent: INV-11 (a "the address is dead" answer
// does not remove a subscription registered or updated after the request it
// answers), Edge 5 and SC-008 (subscriptions registered before the release,
// without a device label, keep receiving deliveries).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
)

// validP256dh / validAuth are well-formed browser key material (a real
// P-256 public key and a 16-byte auth secret), so the production sender's
// payload encryption succeeds and the request reaches the provider boundary.
const (
	validP256dh = "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM"
	validAuth   = "AAAAAAAAAAAAAAAAAAAAAA"
)

// providerTransport is the push service seen by the sender. A request to the
// held endpoint is announced on arrived and answered with the status the test
// sends on release; every other request is answered 201 at once.
type providerTransport struct {
	held    string
	arrived chan struct{}
	release chan int
}

func newProviderTransport(held string) *providerTransport {
	return &providerTransport{held: held, arrived: make(chan struct{}, 1), release: make(chan int, 1)}
}

func (p *providerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	status := http.StatusCreated
	if r.URL.String() == p.held {
		p.arrived <- struct{}{}
		select {
		case status = <-p.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
		Request:    r,
	}, nil
}

// snapshotWorld is one recipient owed one notification, a production runner
// whose sender talks to a providerTransport, and the recipient's endpoint.
type snapshotWorld struct {
	pool      *pgxpool.Pool
	store     *push.Store
	user      uuid.UUID
	endpoint  string
	transport *providerTransport
	runner    *PushRunner
}

func newSnapshotWorld(t *testing.T) snapshotWorld {
	t.Helper()
	pool := newTestPool(t)
	key := "snap" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	f := newPushFixture(t, pool, key)
	f.owed(t, pushRow{feedChannel: true})

	public, private := vapidKeys(t)
	store := push.NewStore(pool)
	transport := newProviderTransport(f.endpoint)
	sender := push.NewSender(store, &http.Client{Transport: transport}, public, private, "mailto:test@skein.local")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM push_subscriptions WHERE endpoint = $1`, f.endpoint)
	})
	return snapshotWorld{
		pool: pool, store: store, user: f.user, endpoint: f.endpoint, transport: transport,
		runner: NewPushRunner(pool, sender, store, "https://app.example.com", PushOptions{}),
	}
}

func vapidKeys(t *testing.T) (public, private string) {
	t.Helper()
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate VAPID keys: %v", err)
	}
	return public, private
}

// seedSub writes the recipient's subscription at w.endpoint with valid key
// material, without a device label (a registration made before the
// release), at version, and returns its id.
func (w snapshotWorld) seedSub(t *testing.T, version int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := w.pool.Exec(context.Background(), `
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth, version, created_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, now(), now())`, id, w.user, w.endpoint, validP256dh, validAuth, version); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return id
}

// row reads the subscription at w.endpoint; ok is false when there is none.
func (w snapshotWorld) row(t *testing.T) (id uuid.UUID, version int64, ok bool) {
	t.Helper()
	err := w.pool.QueryRow(context.Background(),
		`SELECT id, version FROM push_subscriptions WHERE endpoint = $1`, w.endpoint).Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, false
	}
	if err != nil {
		t.Fatalf("read the subscription: %v", err)
	}
	return id, version, true
}

// settle runs the runner until the recipient's due rows are processed. It
// returns the error instead of failing the test so it can run outside the
// test goroutine.
func (w snapshotWorld) settle(ctx context.Context) error {
	for i := 0; i < 20; i++ {
		claimed, err := w.runner.Tick(ctx)
		if err != nil {
			return fmt.Errorf("push runner tick: %w", err)
		}
		var due int
		if err := w.pool.QueryRow(ctx, `
SELECT count(*) FROM notifications
 WHERE user_id = $1 AND push_state = 'pending'
   AND (push_next_attempt_at IS NULL OR push_next_attempt_at <= now())`, w.user).Scan(&due); err != nil {
			return fmt.Errorf("count due rows: %w", err)
		}
		if due == 0 || claimed == 0 {
			return nil
		}
	}
	return errors.New("the recipient's rows are still due after 20 ticks")
}

// deliverHolding runs the runner, waits until the request to the recipient's
// endpoint is at the provider, runs meanwhile, then answers it with status
// and waits for the runner to finish.
func (w snapshotWorld) deliverHolding(t *testing.T, status int, meanwhile func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.settle(ctx) }()

	select {
	case <-w.transport.arrived:
	case err := <-done:
		t.Fatalf("the runner finished without a request to the recipient's endpoint: %v", err)
	case <-ctx.Done():
		t.Fatal("no request reached the recipient's endpoint before the deadline")
	}
	meanwhile()
	w.transport.release <- status
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Row 24a (a) — INV-11: the attempt selected the record at version 3 and the
// provider answers 410 — the record with that id is removed.
func TestPushDelivery_GoneAnswerRemovesTheSelectedRecord(t *testing.T) {
	w := newSnapshotWorld(t)
	id := w.seedSub(t, 3)

	w.deliverHolding(t, http.StatusGone, func() {})

	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_subscriptions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the record the gone answer speaks of is still stored")
	}
}

// Row 24a (b) — INV-11, Edge 5: while the answer is held the record is
// registered again (version 3 → 4); the late 404 leaves it, at version 4.
func TestPushDelivery_LateGoneAnswerKeepsARecordRegisteredAgain(t *testing.T) {
	w := newSnapshotWorld(t)
	id := w.seedSub(t, 3)

	w.deliverHolding(t, http.StatusNotFound, func() {
		var epoch int64
		if err := w.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, w.user).Scan(&epoch); err != nil {
			t.Fatalf("read the session epoch: %v", err)
		}
		sub, err := w.store.Register(context.Background(), push.RegisterInput{
			UserID: w.user, SessionEpoch: epoch, Endpoint: w.endpoint,
			P256dh: validP256dh, Auth: validAuth, // the old form: no label, no mode
		})
		if err != nil {
			t.Fatalf("register again while the answer is held: %v", err)
		}
		if sub.ID != id || sub.Version != 4 {
			t.Fatalf("registration again gave id %s at version %d, want %s at version 4", sub.ID, sub.Version, id)
		}
	})

	gotID, version, ok := w.row(t)
	if !ok || gotID != id || version != 4 {
		t.Fatalf("after the late 404: record present=%v id=%s version=%d, want %s at version 4",
			ok, gotID, version, id)
	}
}

// Row 24a (c) — INV-11, Edge 5: while the answer is held the record is
// removed and created anew at the same endpoint; the late 410 leaves the
// new record.
func TestPushDelivery_LateGoneAnswerKeepsARecordCreatedAnew(t *testing.T) {
	w := newSnapshotWorld(t)
	old := w.seedSub(t, 1)
	var fresh uuid.UUID

	w.deliverHolding(t, http.StatusGone, func() {
		if _, err := w.pool.Exec(context.Background(), `DELETE FROM push_subscriptions WHERE id = $1`, old); err != nil {
			t.Fatalf("remove the record while the answer is held: %v", err)
		}
		fresh = w.seedSub(t, 1)
	})

	gotID, _, ok := w.row(t)
	if !ok || gotID != fresh {
		t.Fatalf("after the late 410: record present=%v id=%s, want the new record %s", ok, gotID, fresh)
	}
}

// Row 24a (d) — SC-008: an active recipient's subscription without a device
// label — registered before the release — gets the request to the provider,
// and a 201 keeps it.
func TestPushDelivery_UnlabelledSubscriptionOfAnActiveRecipientIsSentTo(t *testing.T) {
	w := newSnapshotWorld(t)
	id := w.seedSub(t, 1)

	reached := false
	w.deliverHolding(t, http.StatusCreated, func() { reached = true })

	if !reached {
		t.Fatal("no request reached the provider for the unlabelled subscription")
	}
	if gotID, _, ok := w.row(t); !ok || gotID != id {
		t.Fatalf("after a 201: record present=%v id=%s, want %s kept", ok, gotID, id)
	}
}

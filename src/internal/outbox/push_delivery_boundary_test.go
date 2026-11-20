package outbox

// push_delivery_boundary_test.go — the server-side boundary of a delivery
// attempt's selection against a device revocation and a channel removal
// (integration, TEST_DATABASE_URL), through the production runner and
// subscription store. The reference is the device-consent specification:
// Edge 3 (an attempt's selection sees the state either before the commit of
// a revocation or after it; an attempt that saw it before may still send on
// what it selected, after the commit included; one that saw it after does not
// use the revoked subscriptions; a retry of the same notification within its
// one-hour window selects again and may reach a channel the same user
// registered after coming back), SC-001 and SC-005 on the server side, SC-004.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
	"skein-api/internal/testsupport"
)

// errProviderUnavailable is a transient provider failure: it leaves the
// notification row to be retried.
var errProviderUnavailable = errors.New("push provider unavailable")

// boundarySender is a live sender for the boundary tests. A send to holdOn
// parks until the test releases it; every send is recorded when it is made
// — after the hold — and fails transiently while failing(endpoint) says so.
type boundarySender struct {
	mu      sync.Mutex
	sent    []string
	failing map[string]bool
	holdOn  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBoundarySender(holdOn string) *boundarySender {
	return &boundarySender{
		failing: map[string]bool{},
		holdOn:  holdOn,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *boundarySender) Enabled() bool { return true }

func (s *boundarySender) Send(ctx context.Context, sub push.Subscription, _ []byte) error {
	if sub.Endpoint == s.holdOn {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sub.Endpoint)
	if s.failing[sub.Endpoint] {
		return errProviderUnavailable
	}
	return nil
}

func (s *boundarySender) setFailing(endpoint string, failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[endpoint] = failing
}

func (s *boundarySender) sendsTo(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.sent {
		if e == endpoint {
			n++
		}
	}
	return n
}

// boundaryWorld is one recipient owed one notification, with the runner and
// the subscription store.
type boundaryWorld struct {
	pool   *pgxpool.Pool
	store  *push.Store
	user   uuid.UUID
	epoch  int64
	row    int64
	key    string
	runner *PushRunner
}

func newBoundaryWorld(t *testing.T, sender PushSender) boundaryWorld {
	t.Helper()
	pool := newTestPool(t)
	key := "bnd" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	f := newPushFixture(t, pool, key)
	var epoch int64
	if err := pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, f.user).Scan(&epoch); err != nil {
		t.Fatalf("read the session epoch: %v", err)
	}
	store := push.NewStore(pool)
	return boundaryWorld{
		pool: pool, store: store, user: f.user, epoch: epoch, key: key,
		row:    f.owed(t, pushRow{feedChannel: true}),
		runner: NewPushRunner(pool, sender, store, "https://app.example.com", PushOptions{}),
	}
}

// register registers a channel of the recipient with label, as the page does.
func (w boundaryWorld) register(t *testing.T, name string, label uuid.UUID) string {
	t.Helper()
	endpoint := "https://push.example.com/" + w.key + "-" + name
	if _, err := w.store.Register(context.Background(), push.RegisterInput{
		UserID: w.user, SessionEpoch: w.epoch, Endpoint: endpoint,
		P256dh: "p256dh-" + name, Auth: "auth-" + name, DeviceLabel: &label, Mode: push.ModeUpsert,
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	})
	return endpoint
}

// settle runs the runner until the recipient's due rows are processed; it
// returns the error so it can run outside the test goroutine.
func (w boundaryWorld) settle(ctx context.Context) error {
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

// retryNow makes the row's next attempt due — still inside its one-hour
// window — and runs the runner.
func (w boundaryWorld) retryNow(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	state, _, _ := pushStateOf(t, w.pool, w.row)
	if state != "pending" {
		t.Fatalf("the row is %s before the retry, want pending — there is no retry to observe", state)
	}
	tag, err := w.pool.Exec(ctx,
		`UPDATE notifications SET push_next_attempt_at = now() WHERE id = $1 AND push_deadline_at > now()`, w.row)
	if err != nil {
		t.Fatalf("make the retry due: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("make the retry due: %d rows updated, want 1 — the row is gone or past its deadline, so no retry inside the window can run",
			tag.RowsAffected())
	}
	testsupport.TickPushUntilSettled(t, w.pool, w.user, w.runner.Tick)
}

// Row 31 — Edge 3 (both halves), SC-001 (server), SC-004: an attempt that
// selected the L1 subscription before RevokeDevice(L1) committed may still
// send on it after the commit; the retry of the same notification selects
// again — nothing to L1, the neighbouring device L2 is served.
func TestPushBoundary_RevocationDuringAnAttemptAndTheRetryAfterIt(t *testing.T) {
	l1, l2 := uuid.New(), uuid.New()
	sender := newBoundarySender("")
	w := newBoundaryWorld(t, sender)
	e1 := w.register(t, "l1", l1)
	e2 := w.register(t, "l2", l2)
	sender.holdOn = e1
	// The first attempt fails transiently on both devices, so the row stays
	// for the retry.
	sender.setFailing(e1, true)
	sender.setFailing(e2, true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.settle(ctx) }()
	select {
	case <-sender.entered:
	case err := <-done:
		t.Fatalf("the attempt finished without selecting the L1 subscription: %v", err)
	case <-ctx.Done():
		t.Fatal("the attempt did not reach the L1 subscription before the deadline")
	}

	removed, err := w.store.RevokeDevice(context.Background(), w.user, w.epoch, []uuid.UUID{l1}, "")
	if err != nil || removed != 1 {
		t.Fatalf("RevokeDevice(L1) while the send is held: removed %d, err %v; want 1 committed", removed, err)
	}
	close(sender.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Selected before the commit: the send on the selected row went out after
	// it — allowed. (Its absence is not asserted against: either is allowed
	// only in this direction, and here the hold forces the late send.)
	if got := sender.sendsTo(e1); got != 1 {
		t.Fatalf("sends to L1 by the attempt that selected it before the revocation = %d, want 1", got)
	}

	sender.setFailing(e2, false)
	sentToL2 := sender.sendsTo(e2)
	w.retryNow(t)

	if got := sender.sendsTo(e1); got != 1 {
		t.Errorf("the retry selected after the revocation sent to L1 (%d sends in all, want the 1 before it)", got)
	}
	if got := sender.sendsTo(e2) - sentToL2; got != 1 {
		t.Errorf("the retry sent %d times to the neighbouring device L2, want 1", got)
	}
	if state, outcome, _ := pushStateOf(t, w.pool, w.row); state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("row after the retry: %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
}

// Row 32 — SC-005 (server), Edge 3: after an unconditional RemoveChannel(E)
// succeeded, the retry of the same notification does not send to E; after
// the same user registers a new channel E2 with a new label, the next retry
// in the window sends to E2. A second device that keeps failing transiently
// keeps the row alive between the retries.
func TestPushBoundary_RetryAfterAChannelRemovalReachesTheChannelRegisteredLater(t *testing.T) {
	sender := newBoundarySender("")
	w := newBoundaryWorld(t, sender)
	e := w.register(t, "e", uuid.New())
	other := w.register(t, "other", uuid.New())
	sender.setFailing(e, true)
	sender.setFailing(other, true)

	testsupport.TickPushUntilSettled(t, w.pool, w.user, w.runner.Tick)
	if sender.sendsTo(e) != 1 {
		t.Fatalf("the first attempt sent %d times to E, want 1 — the case proves nothing", sender.sendsTo(e))
	}

	owner, err := w.store.RemoveChannel(context.Background(), w.user, w.epoch, e, nil, true)
	if err != nil || owner == nil || *owner != w.user {
		t.Fatalf("unconditional RemoveChannel(E): owner %v, err %v; want the recipient's channel removed", owner, err)
	}
	w.retryNow(t)
	if got := sender.sendsTo(e); got != 1 {
		t.Errorf("the retry after the removal sent to E (%d sends in all, want the 1 before it)", got)
	}
	if sender.sendsTo(other) != 2 {
		t.Fatalf("the retry sent %d times in all to the other device, want 2 — it did not run", sender.sendsTo(other))
	}

	e2 := w.register(t, "e2", uuid.New())
	w.retryNow(t)
	if got := sender.sendsTo(e2); got != 1 {
		t.Errorf("the next retry sent %d times to the channel registered after the removal, want 1", got)
	}
	if got := sender.sendsTo(e); got != 1 {
		t.Errorf("the next retry sent to the removed channel E (%d sends in all, want 1)", got)
	}
	if state, outcome, _ := pushStateOf(t, w.pool, w.row); state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("row after the last retry: %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
}

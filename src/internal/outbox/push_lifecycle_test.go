package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/push"
)

// push_lifecycle_test.go — sk-303.13 / T010, coverage-matrix row 21:
// claim, lease, crash recovery, mutual exclusion, and the three states of NATS.
//
// The contract is at-least-once and NOT exactly-once — a crash between the
// provider accepting a push and the row being finalised may deliver twice, and
// the plan says so out loud. What IS promised is narrower and testable: two
// workers never hold the same row at the same time, a crashed worker's hold
// expires instead of stranding the row, and a burnt attempt is not refunded.
//
// The NATS half is the reason this file exists at all. Until this wave push
// lived inside the NATS dispatcher, so "no bus" and "the bus is down" silently
// meant "no pushes". All three states are therefore driven here against the
// same row shape.

// TestPushLifecycle_SkipLockedExcludesAConcurrentHolder — the claim is atomic
// and skips what somebody else is holding. A row locked by another transaction
// is not claimed, not attempted, and not touched.
func TestPushLifecycle_SkipLockedExcludesAConcurrentHolder(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "plsk").withDevice(t)
	id := f.owed(t, pushRow{})

	// Another transaction holds the row.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	var held int64
	if err := holder.QueryRow(ctx,
		`SELECT id FROM notifications WHERE id = $1 FOR UPDATE`, id).Scan(&held); err != nil {
		_ = holder.Rollback(ctx)
		t.Fatalf("hold row: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		_ = holder.Rollback(ctx)
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("a row held by another transaction was sent %d times, want 0 (FOR UPDATE SKIP LOCKED)", len(got))
	}
	_ = holder.Rollback(ctx)

	state, _, attempts := pushStateOf(t, pool, id)
	if state != "pending" || attempts != 0 {
		t.Errorf("held row = %s/%d attempts, want pending/0 — skipping must not consume an attempt", state, attempts)
	}

	// Released — the very next tick takes it, so skipping deferred the work
	// rather than losing it.
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick after release: %v", err)
	}
	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Errorf("sends after the holder released = %d, want 1", len(got))
	}
}

// TestPushLifecycle_ConcurrentRunnersNeverDoubleSend — «concurrent attempts
// mutually exclusive». Two runners drain the same queue at the same time; each
// row must produce exactly one send.
func TestPushLifecycle_ConcurrentRunnersNeverDoubleSend(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const rows = 12
	fixtures := make([]*pushFixture, 0, rows)
	for i := range rows {
		f := newPushFixture(t, pool, fmt.Sprintf("plcc%02d", i)).withDevice(t)
		f.owed(t, pushRow{})
		fixtures = append(fixtures, f)
	}

	sender := &fakePushSender{enabled: true}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner := newRunner(pool, sender)
			// Several passes so a runner that lost the first race still tries.
			for range 3 {
				if _, err := runner.Tick(ctx); err != nil {
					t.Errorf("Tick: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, f := range fixtures {
		if got := callsTo(sender, f.endpoint); len(got) != 1 {
			t.Errorf("endpoint %s received %d sends, want exactly 1", f.endpoint, len(got))
		}
	}
}

// TestPushLifecycle_ExpiredLeaseIsRecoveredAndTheAttemptIsNotRefunded — crash
// recovery. A worker claimed the row and died; its lease expires and the row
// returns to the queue. The attempt it burnt STAYS burnt: refunding it would
// turn a crash into an unbounded retry loop against a provider that may
// already have delivered.
func TestPushLifecycle_ExpiredLeaseIsRecoveredAndTheAttemptIsNotRefunded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "plle").withDevice(t)
	id := f.owed(t, pushRow{})

	// Simulate the crash: leased, one attempt spent, lease already expired.
	if _, err := pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'leased', push_attempts = 1, push_leased_until = now() - interval '1 minute'
 WHERE id = $1`, id); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("sends after lease recovery = %d, want 1 — a crashed worker must not strand the row", len(got))
	}
	state, outcome, attempts := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the crashed attempt is not refunded)", attempts)
	}
}

// TestPushLifecycle_LiveLeaseIsLeftAlone — the other half of the lease: while
// it is still valid, the row belongs to its holder. Reclaiming early would be
// the double-send the exclusion exists to prevent.
func TestPushLifecycle_LiveLeaseIsLeftAlone(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "pllv").withDevice(t)
	id := f.owed(t, pushRow{})

	if _, err := pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'leased', push_attempts = 1, push_leased_until = now() + interval '10 minutes'
 WHERE id = $1`, id); err != nil {
		t.Fatalf("lease row: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("a live lease was stolen and sent %d times, want 0", len(got))
	}
	state, _, attempts := pushStateOf(t, pool, id)
	if state != "leased" || attempts != 1 {
		t.Errorf("row = %s/%d attempts, want leased/1 (untouched)", state, attempts)
	}
}

// TestPushLifecycle_DrainsWithoutAnyNATS — state (a): NATS_URL empty. There is
// no dispatcher and no connection anywhere in this test, and the push still
// goes out. This is the deployment shape that used to have no pushes at all.
func TestPushLifecycle_DrainsWithoutAnyNATS(t *testing.T) {
	pool := newTestPool(t)
	f := newPushFixture(t, pool, "plnn").withDevice(t)
	id := f.owed(t, pushRow{feedChannel: true})

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("sends with no NATS at all = %d, want 1", len(got))
	}
	// The feed side of the same row is untouched: with no dispatcher running,
	// its live-feed outcome is simply not yet decided — the row stays
	// replayable for the next deploy that has a connection.
	if at := notifPublishedAt(t, pool, id); at != nil {
		t.Errorf("published_at = %v with no dispatcher running, want NULL", at)
	}
	if state, outcome, _ := pushStateOf(t, pool, id); state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("push state/outcome = %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
}

// TestPushLifecycle_DrainsWhileNATSIsBroken — state (b): the connection exists
// but the broker is gone. The NATS tick fails and leaves its rows unpublished
// (replayable); the push runner, which shares nothing with it, delivers.
func TestPushLifecycle_DrainsWhileNATSIsBroken(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	srv, nc := startEmbeddedNATS(t)

	f := newPushFixture(t, pool, "plbr").withDevice(t)
	id := f.owed(t, pushRow{feedChannel: true})

	// Break the broker after the connection was established — the shape of a
	// mid-flight outage rather than of a failed boot.
	srv.Shutdown()
	srv.WaitForShutdown()

	dispatcher := New(pool, nc, Options{BatchSize: 50, FlushTimeout: 200 * time.Millisecond})
	if _, err := dispatcher.tickNotifications(ctx); err == nil {
		t.Log("NATS tick reported success against a dead broker; the publish is buffered locally")
	}
	if at := notifPublishedAt(t, pool, id); at != nil {
		t.Errorf("published_at = %v after a failed NATS tick — the row must stay replayable", at)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("sends while NATS is broken = %d, want 1 — the push path must not depend on the bus", len(got))
	}
}

// TestPushLifecycle_LiveNATSCarriesFeedRowsOnly — state (c): everything up.
// The feed row is published on the user's subject; the push-only row is not,
// and its push still goes out.
//
// This is the leak assertion done from the WIRE, not from a column: what the
// bridge would forward is what actually arrives on the subject.
func TestPushLifecycle_LiveNATSCarriesFeedRowsOnly(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	_, nc := startEmbeddedNATS(t)

	f := newPushFixture(t, pool, "plli").withDevice(t)
	feedRow := f.owed(t, pushRow{feedChannel: true})

	// A second row for the same recipient that belongs to no feed.
	pushOnlyEvent := seedEvent(t, pool, "issue.status_changed", "issue", f.issueID, f.projectID)
	setEventNewData(t, pool, pushOnlyEvent, `"open"`)
	pushOnly := insertPushNotification(t, pool, f.user, pushOnlyEvent, "issue.status_changed", f.projectID,
		pushRow{reason: "delegated_status", feedChannel: false})

	received := make(chan []byte, 16)
	sub, err := nc.Subscribe("skein.user."+f.user.String()+".notification", func(m *nats.Msg) {
		received <- m.Data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	dispatcher := New(pool, nc, Options{BatchSize: 100})
	if _, err := dispatcher.tickNotifications(ctx); err != nil {
		t.Fatalf("tickNotifications: %v", err)
	}

	// Exactly one envelope for this recipient: the feed row.
	var envelopes [][]byte
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case b := <-received:
			envelopes = append(envelopes, b)
		case <-deadline:
			break collect
		case <-time.After(300 * time.Millisecond):
			break collect
		}
	}
	if len(envelopes) != 1 {
		t.Errorf("envelopes on the recipient's subject = %d, want 1 (the feed row only)", len(envelopes))
	}
	if at := notifPublishedAt(t, pool, feedRow); at == nil {
		t.Error("the feed row was not published to NATS")
	}
	if at := notifPublishedAt(t, pool, pushOnly); at != nil {
		t.Errorf("the push-only row was published to NATS at %v — it belongs to no feed", at)
	}

	// And both rows still get their push, live bus or not.
	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := callsTo(sender, f.endpoint); len(got) != 2 {
		t.Errorf("sends = %d, want 2 (one per owed row)", len(got))
	}
}

// TestPushLifecycle_RunLoopsStopIndependently — the shutdown half of the
// split. Cancelling the context returns each Run on its own; neither waits on
// the other, so a NATS drain that hangs cannot hold the push runner hostage.
func TestPushLifecycle_RunLoopsStopIndependently(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	pushCtx, stopPush := context.WithCancel(context.Background())
	natsCtx, stopNATS := context.WithCancel(context.Background())

	pushDone := make(chan struct{})
	natsDone := make(chan struct{})

	runner := NewPushRunner(pool, &fakePushSender{enabled: false}, nil, "",
		PushOptions{PollInterval: 20 * time.Millisecond})
	go func() { defer close(pushDone); _ = runner.Run(pushCtx) }()

	dispatcher := New(pool, nc, Options{PollInterval: 20 * time.Millisecond})
	go func() { defer close(natsDone); _ = dispatcher.Run(natsCtx) }()

	// Stop ONLY the push runner: the NATS loop must still be running.
	stopPush()
	select {
	case <-pushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("push runner did not stop on its own context")
	}
	select {
	case <-natsDone:
		t.Fatal("the NATS dispatcher stopped when the push runner did — the loops are not independent")
	case <-time.After(100 * time.Millisecond):
	}

	stopNATS()
	select {
	case <-natsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not stop on its own context")
	}
}

// blockingPushSender is a LIVE sender that is simply slow: it records every
// call like the fake one does, and parks inside Send for one chosen endpoint
// until the test releases it. That distinction is the whole point of the test
// below — the existing lease tests simulate a worker that DIED, and a dead
// worker cannot double-send.
type blockingPushSender struct {
	mu        sync.Mutex
	sent      []sentPush
	holdOn    string
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func newBlockingPushSender(holdOn string) *blockingPushSender {
	return &blockingPushSender{
		holdOn:  holdOn,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *blockingPushSender) Enabled() bool { return true }

func (s *blockingPushSender) Send(_ context.Context, sub push.Subscription, payload []byte) error {
	s.mu.Lock()
	var p pushPayload
	_ = json.Unmarshal(payload, &p)
	s.sent = append(s.sent, sentPush{endpoint: sub.Endpoint, payload: p})
	s.mu.Unlock()
	if sub.Endpoint == s.holdOn {
		s.enterOnce.Do(func() { close(s.entered) })
		<-s.release
	}
	return nil
}

func (s *blockingPushSender) calls() []sentPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentPush, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *blockingPushSender) callsTo(endpoint string) []sentPush {
	var out []sentPush
	for _, c := range s.calls() {
		if c.endpoint == endpoint {
			out = append(out, c)
		}
	}
	return out
}

// TestPushLifecycle_ExpiredLeaseOnALiveWorkerNeverDoubleSends — row 21, the
// case FOR UPDATE SKIP LOCKED does not cover and the crash tests cannot reach.
//
// A batch is claimed under ONE lease and delivered row by row. While the
// worker is busy — alive, not crashed — with the head of its batch, the lease
// on the TAIL runs out. Another worker then legitimately reclaims that row: an
// expired lease is indistinguishable from an abandoned one, and reclaiming it
// is the documented behaviour. It delivers and finalises the row.
//
// When the first worker gets to its own copy of that row it must send NOTHING
// and record NOTHING: the row is not its any more. What proves it is the
// provider — exactly one call for the tail endpoint across both senders — and
// the outcome counter, which owes exactly one increment per notification.
func TestPushLifecycle_ExpiredLeaseOnALiveWorkerNeverDoubleSends(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// The two rows carry reason 'assignment' so their outcome counter is not
	// shared with every other push test in this package (they all use
	// 'delegated_status'), which is what lets the increment be counted exactly.
	head := newPushFixture(t, pool, "plls-head").withDevice(t)
	setReasonPush(t, pool, head.user, "assignment", true)
	tail := newPushFixture(t, pool, "plls-tail").withDevice(t)
	setReasonPush(t, pool, tail.user, "assignment", true)

	headID := head.owed(t, pushRow{reason: "assignment"})
	tailID := tail.owed(t, pushRow{reason: "assignment"})

	beforeSuccess := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("assignment", outcomeSuccess, causeNone))

	// Worker A: claims both rows in one batch, parks in the head's Send.
	slow := newBlockingPushSender(head.endpoint)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := newRunner(pool, slow).Tick(ctx); err != nil {
			t.Errorf("worker A Tick: %v", err)
		}
	}()
	<-slow.entered

	// The tail's lease runs out while A is still working the head. Nothing
	// about A has failed: it is alive and holding a batch it has not reached.
	if _, err := pool.Exec(ctx, `
UPDATE notifications
   SET push_leased_until = now() - interval '1 minute'
 WHERE id = $1 AND push_state = 'leased'`, tailID); err != nil {
		t.Fatalf("expire the tail lease: %v", err)
	}

	// Worker B reclaims the tail and delivers it — legitimately.
	fast := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, fast).Tick(ctx); err != nil {
		t.Fatalf("worker B Tick: %v", err)
	}
	if got := callsTo(fast, tail.endpoint); len(got) != 1 {
		t.Fatalf("worker B sent the reclaimed row %d times, want 1", len(got))
	}

	// A finishes the head and walks into the tail it no longer owns.
	close(slow.release)
	<-done

	if got := slow.callsTo(tail.endpoint); len(got) != 0 {
		t.Errorf("the live worker sent a row whose lease it had lost %d times, want 0 — "+
			"one notification, two provider calls", len(got))
	}
	state, outcome, attempts := pushStateOf(t, pool, tailID)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("tail row = %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
	if attempts != 2 {
		t.Errorf("tail attempts = %d, want 2 (claimed once, reclaimed once)", attempts)
	}
	headState, headOutcome, _ := pushStateOf(t, pool, headID)
	if headState != "final" || headOutcome == nil || *headOutcome != outcomeSuccess {
		t.Errorf("head row = %s/%v, want final/%s — the row A did own must still finalise",
			headState, headOutcome, outcomeSuccess)
	}

	// Two notifications, two outcomes. A third increment would be the same
	// tail row counted twice.
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("assignment", outcomeSuccess, causeNone)) - beforeSuccess; got != 2 {
		t.Errorf("success outcomes recorded = %v, want 2 (one per notification)", got)
	}
}

// TestPushLifecycle_OutcomeIsNotRecordedTwiceAfterLosingTheLease — row 21,
// the second half of the same rule and the one the ownership predicate on
// finalise exists for.
//
// Here the lease is lost DURING the provider hand-off, not before it: the
// worker is inside Send when its lease runs out, another worker reclaims the
// row and finalises it, and only then does the first one come back with an
// answer. The double SEND is the at-least-once contract and is not a defect.
// The double OUTCOME is: skein_outbox_push_outcomes_total promises exactly one
// increment per notification over its lifetime, so a row cannot be decided
// twice.
func TestPushLifecycle_OutcomeIsNotRecordedTwiceAfterLosingTheLease(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	f := newPushFixture(t, pool, "pllo").withDevice(t)
	setReasonPush(t, pool, f.user, "assignment", true)
	id := f.owed(t, pushRow{reason: "assignment"})

	before := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("assignment", outcomeSuccess, causeNone))

	slow := newBlockingPushSender(f.endpoint)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := newRunner(pool, slow).Tick(ctx); err != nil {
			t.Errorf("worker A Tick: %v", err)
		}
	}()
	<-slow.entered

	if _, err := pool.Exec(ctx, `
UPDATE notifications
   SET push_leased_until = now() - interval '1 minute'
 WHERE id = $1 AND push_state = 'leased'`, id); err != nil {
		t.Fatalf("expire the lease mid-send: %v", err)
	}

	fast := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, fast).Tick(ctx); err != nil {
		t.Fatalf("worker B Tick: %v", err)
	}

	close(slow.release)
	<-done

	state, outcome, attempts := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("row = %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (claimed once, reclaimed once)", attempts)
	}
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("assignment", outcomeSuccess, causeNone)) - before; got != 1 {
		t.Errorf("success outcomes recorded = %v, want 1 — one notification is decided once, "+
			"whichever worker gets there first", got)
	}
}

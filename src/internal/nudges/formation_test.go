package nudges

// formation_test.go — sk-303.14, task T020.
// Matrix rows: 21a (the "formation is silenced" half), 76 (the "from
// delivery" half), 80, 81, 82, 83, 84, 106.
//
// This checks SEGMENT CONSUMPTION and what is around it: instance
// coordination, atomicity on a crash, supersession, delivery outcomes of an
// already-formed row.
//
// The delivery path is brought up REAL (outbox.PushRunner) with a fake
// sender: the assertions "only the freshest is delivered" and "no devices"
// are about the path's behaviour on state the worker produced, and a query
// assembled by hand would prove them for a fictional path.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/notifications"
	"skein-api/internal/outbox"
	"skein-api/internal/push"
	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Fake sender
// ---------------------------------------------------------------------------

type sentPush struct {
	endpoint string
	payload  []byte
}

type fakeSender struct {
	mu      sync.Mutex
	enabled bool
	fail    bool
	sent    []sentPush
}

func (f *fakeSender) Enabled() bool { return f.enabled }

func (f *fakeSender) Send(_ context.Context, sub push.Subscription, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentPush{endpoint: sub.Endpoint, payload: payload})
	if f.fail {
		return errFakeTransport
	}
	return nil
}

// callsTo is the sends that reached one device. The runner drains the
// shared queue, so a sender may also see other tests' rows; an assertion
// about the addressee counts the addressee's device only.
func (f *fakeSender) callsTo(endpoint string) []sentPush {
	var out []sentPush
	for _, c := range f.calls() {
		if c.endpoint == endpoint {
			out = append(out, c)
		}
	}
	return out
}

// deviceEndpoint is the endpoint testsupport.SeedPushSubscription gives the
// device row id.
func deviceEndpoint(id uuid.UUID) string { return "https://push.test.local/" + id.String() }

func (f *fakeSender) calls() []sentPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentPush, len(f.sent))
	copy(out, f.sent)
	return out
}

var errFakeTransport = &transportError{}

type transportError struct{}

func (*transportError) Error() string { return "fake transport: refusal" }

// runner assembles a REAL path worker on top of the same database.
func (e *env) runner(sender outbox.PushSender) *outbox.PushRunner {
	return outbox.NewPushRunner(e.pool, sender, push.NewStore(e.pool),
		"http://localhost:8080", outbox.PushOptions{BatchSize: 50})
}

// pushRow reads the state of the addressee's delivery rows for the
// category's reason, newest first.
type pushRow struct {
	id      int64
	state   string
	outcome *string
}

func (e *env) pushRows(t *testing.T, userID uuid.UUID, cat review.Category) []pushRow {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `
SELECT id, push_state, push_outcome
  FROM notifications
 WHERE user_id = $1 AND reason = $2
 ORDER BY id DESC`, userID, reasonOf(t, cat))
	if err != nil {
		t.Fatalf("reading delivery rows: %v", err)
	}
	defer rows.Close()
	var out []pushRow
	for rows.Next() {
		var r pushRow
		if err := rows.Scan(&r.id, &r.state, &r.outcome); err != nil {
			t.Fatalf("scanning delivery rows: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// unacceptedCandidate — a typical precondition: an addressee with one
// unaccepted issue whose burst segment has already expired.
func (e *env) unacceptedCandidate(t *testing.T, suffix string) (uuid.UUID, Candidate) {
	t.Helper()
	author, _ := e.user(t)
	worker, workerName := e.user(t)
	e.seed(t, issue{suffix: suffix, status: "open", assignee: workerName,
		assigneeSetAt: ago(25 * time.Minute), createdBy: author})
	c, ok := candidateFor(t, t.Context(), e.store, review.CategoryUnaccepted,
		time.Now().UTC(), worker)
	if !ok {
		t.Fatalf("precondition: the addressee did not enter the selection")
	}
	return worker, c
}

// ---------------------------------------------------------------------------
// Row 21a — turning a category off silences FORMATION
// ---------------------------------------------------------------------------

// TestNudge_DisabledCategorySilencesFormation — matrix row 21a, the
// "repeats" half.
//
// It is FORMATION that is silenced, not only sending: a person who turned
// the category off gets neither a delivery row nor an audit record, and
// their pair's segment is not consumed. The difference is observable and
// material — the path could stay silent while the worker forms a digest
// every segment, consumes the segment and writes an audit, and from the
// outside it would look the same.
//
// The second half of the row — "does not silence the primary assignment
// notification" — lives in the registry and fan-out classes (T005, T015):
// the primary notification is not produced by the reminder worker. Here
// what belongs to it is checked: the two reasons' switches are independent,
// and turning a repeat off does not touch the assignment reason.
func TestNudge_DisabledCategorySilencesFormation(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	e.seed(t, issue{suffix: "muted", status: "open", assignee: workerName,
		assigneeSetAt: ago(25 * time.Minute), createdBy: author})
	e.setCategoryPush(t, worker, review.CategoryUnaccepted, false)

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())

	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 0 {
		t.Errorf("row 21a: with the category off, %d reminders were formed", n)
	}
	if _, _, ok := e.rateLimit(t, worker, review.CategoryUnaccepted); ok {
		t.Errorf("row 21a: the segment was consumed with the category off — only " +
			"sending is silenced, and formation goes on")
	}
	var events int
	if err := e.pool.QueryRow(ctx, `
SELECT count(*) FROM events WHERE entity_type = 'user' AND entity_id = $1`,
		worker.String()).Scan(&events); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 0 {
		t.Errorf("row 21a: with the category off, %d audit events were written", events)
	}

	// The reason switches are independent: the assignment reason is
	// untouched.
	var assignmentRows int
	if err := e.pool.QueryRow(ctx, `
SELECT count(*) FROM user_notification_reason_prefs WHERE user_id = $1 AND reason = $2`,
		worker, string(notifications.ReasonKeyAssignment)).Scan(&assignmentRows); err != nil {
		t.Fatalf("reading assignment-reason prefs: %v", err)
	}
	if assignmentRows != 0 {
		t.Errorf("row 21a: turning a repeat off created an assignment-reason pref — "+
			"the two reasons stopped being independent (rows: %d)", assignmentRows)
	}
}

// ---------------------------------------------------------------------------
// Row 80 — instance contention
// ---------------------------------------------------------------------------

// TestNudge_ConcurrentInstancesConsumeTheSegmentOnce — matrix row 80
// (INV-8, SC-020).
//
// Eight instances with ONE AND THE SAME candidate and one "now" — i.e. the
// worst case: all eight consider the segment expired at once. Exactly one
// must consume it.
func TestNudge_ConcurrentInstancesConsumeTheSegmentOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	worker, c := e.unacceptedCandidate(t, "race")

	now := time.Now().UTC()
	next := now.Add(3 * time.Hour)

	const instances = 8
	var wg sync.WaitGroup
	results := make([]FormResult, instances)
	errs := make([]error, instances)
	wg.Add(instances)
	for i := range instances {
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.store.Form(ctx, review.CategoryUnaccepted, c, next)
		}()
	}
	wg.Wait()

	formed := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("instance %d returned an error: %v", i, errs[i])
		}
		if r.Formed {
			formed++
		}
	}
	if formed != 1 {
		t.Errorf("row 80: the segment was consumed by %d instances of %d, expected exactly one",
			formed, instances)
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("row 80: delivery rows %d, expected one", n)
	}
}

// ---------------------------------------------------------------------------
// Row 81 — a crash between steps
// ---------------------------------------------------------------------------

// TestNudge_CrashBetweenStepsLosesAndDuplicatesNothing — matrix row 81,
// first half.
//
// The crash is reproduced honestly: the segment is consumed in a
// transaction that is then rolled back — exactly what a crashed process
// does. Neither a duplicate (there is no notification) nor a loss (the
// segment is not considered consumed, and the next tick forms a reminder
// as if nothing happened).
func TestNudge_CrashBetweenStepsLosesAndDuplicatesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	worker, c := e.unacceptedCandidate(t, "crash")

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the transaction: %v", err)
	}
	now := time.Now().UTC()
	consumed, err := consumeSegment(ctx, tx, c.UserID, review.CategoryUnaccepted,
		now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("consuming the segment: %v", err)
	}
	if !consumed {
		t.Fatalf("precondition: the segment was not consumed")
	}
	// "The process crashed" exactly here: between consuming the segment
	// and writing the notification.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, _, ok := e.rateLimit(t, worker, review.CategoryUnaccepted); ok {
		t.Errorf("row 81: the segment remained consumed after the crash — the reminder " +
			"is lost forever")
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 0 {
		t.Errorf("row 81: after the crash %d delivery rows appeared", n)
	}

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("row 81: the next tick formed %d reminders, expected one", n)
	}
}

// TestNudge_UnprocessedAddresseesAreTakenByLaterTicks — matrix row 81,
// second half.
//
// The batch limits are squeezed to one addressee per tick, so one tick
// certainly does not have time to process both. The assertion is not about
// HOW MANY ticks it took (the database is shared, foreign addressees
// legitimately stand in the queue), but about each eventually receiving
// EXACTLY ONE reminder: the unprocessed are taken by the next tick, and
// none is taken twice.
func TestNudge_UnprocessedAddresseesAreTakenByLaterTicks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	author, _ := e.user(t)
	base := ago(25 * time.Minute)

	workers := make([]uuid.UUID, 0, 2)
	for i, suffix := range []string{"drain-a", "drain-b"} {
		w, name := e.user(t)
		e.seed(t, issue{suffix: suffix, status: "open", assignee: name,
			assigneeSetAt: base, createdBy: author})
		workers = append(workers, w)
		_ = i
	}

	sc := NewScheduler(e.store, Config{BatchSize: 1, BatchesPerTick: 1})
	const maxTicks = 200
	done := false
	for range maxTicks {
		sc.tickAt(ctx, time.Now().UTC())
		done = true
		for _, w := range workers {
			if e.notifications(t, w, review.CategoryUnaccepted) == 0 {
				done = false
			}
		}
		if done {
			break
		}
	}
	if !done {
		t.Fatalf("row 81: in %d ticks of one addressee not all addressees were processed", maxTicks)
	}
	for _, w := range workers {
		if n := e.notifications(t, w, review.CategoryUnaccepted); n != 1 {
			t.Errorf("row 81: addressee %s received %d reminders, expected exactly one", w, n)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 82 — a retry batch does not consume the segment
// ---------------------------------------------------------------------------

// TestNudge_RetryBatchDoesNotConsumeASegment — matrix row 82 (INV-8).
//
// The attempts are produced by the REAL path: the sender refuses, the row
// returns to the queue, the attempt counter grows. The "at most one per
// segment" guarantee is about formation, not about the number of physical
// attempts, so the rate-limit state must stay untouched.
func TestNudge_RetryBatchDoesNotConsumeASegment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	worker, _ := e.unacceptedCandidate(t, "retry")
	testsupport.SeedPushSubscription(t, e.pool, worker)

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())
	lastBefore, nextBefore, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("precondition: the reminder was not formed")
	}

	sender := &fakeSender{enabled: true, fail: true}
	runner := e.runner(sender)
	for range 3 {
		testsupport.TickPushUntilSettled(t, e.pool, worker, runner.Tick)
		// A retry is scheduled with a delay; for the test it is lifted
		// immediately, otherwise the next tick will not see the row.
		if _, err := e.pool.Exec(ctx, `
UPDATE notifications SET push_next_attempt_at = now()
 WHERE user_id = $1 AND push_state = 'pending'`, worker); err != nil {
			t.Fatalf("lifting the retry delay: %v", err)
		}
	}

	var attempts int
	if err := e.pool.QueryRow(ctx, `
SELECT max(push_attempts) FROM notifications WHERE user_id = $1`, worker).Scan(&attempts); err != nil {
		t.Fatalf("reading the attempt counter: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("row 82 precondition: only %d attempts — there was no retry batch, "+
			"nothing to check", attempts)
	}

	lastAfter, nextAfter, _ := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !approx(lastAfter, lastBefore) || !approx(nextAfter, nextBefore) {
		t.Errorf("row 82: a batch of %d attempts moved the segment state: was (%s, %s), "+
			"became (%s, %s)", attempts, lastBefore, nextBefore, lastAfter, nextAfter)
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("row 82: the attempts spawned new delivery rows, %d in total", n)
	}
}

// ---------------------------------------------------------------------------
// Row 83 — supersession
// ---------------------------------------------------------------------------

// TestNudge_OnlyTheFreshestSurvivesAndIsDelivered — matrix row 83.
//
// The transport is down longer than a segment: in that time several
// notifications of one pair are legitimately formed. Both halves of the
// assertion are checked — state (the early ones are completed with THEIR
// OWN supersession outcome, not a config refusal) and behaviour (the path
// delivers exactly one, the freshest).
func TestNudge_OnlyTheFreshestSurvivesAndIsDelivered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	worker, _ := e.unacceptedCandidate(t, "supersede")
	device := deviceEndpoint(testsupport.SeedPushSubscription(t, e.pool, worker))

	sc := NewScheduler(e.store, Config{})
	const segments = 3
	for range segments {
		sc.tickAt(ctx, time.Now().UTC())
		e.forceNextDue(t, worker, review.CategoryUnaccepted, time.Now().UTC().Add(-time.Minute))
	}

	rows := e.pushRows(t, worker, review.CategoryUnaccepted)
	if len(rows) != segments {
		t.Fatalf("row 83 precondition: formed %d rows, expected %d", len(rows), segments)
	}
	if rows[0].state != "pending" {
		t.Errorf("row 83: the freshest row is in state %q, not queued for sending",
			rows[0].state)
	}
	for _, r := range rows[1:] {
		if r.state != "final" || r.outcome == nil || *r.outcome != outbox.OutcomeSuperseded {
			got := "<nil>"
			if r.outcome != nil {
				got = *r.outcome
			}
			t.Errorf("row 83: early row %d — state %q, outcome %q; expected "+
				"final/%s. A foreign outcome would make supersession indistinguishable "+
				"from a config refusal in the row's state", r.id, r.state, got, outbox.OutcomeSuperseded)
		}
	}

	sender := &fakeSender{enabled: true}
	testsupport.TickPushUntilSettled(t, e.pool, worker, e.runner(sender).Tick)
	if got := len(sender.callsTo(device)); got != 1 {
		t.Errorf("row 83: after the transport recovered %d notifications were delivered — "+
			"the person received a batch for every idle segment instead of one freshest", got)
	}
}

// ---------------------------------------------------------------------------
// Row 84 — no registered devices
// ---------------------------------------------------------------------------

// TestNudge_NoDeviceStillConsumesTheSegment — matrix row 84.
//
// There are no devices: the notification is FORMED, the segment is
// CONSUMED, the outcome is stamped with a separate value. The last sentence
// of the row — "a repeat notification every tick does not arise" — is the
// meaning of consumption: if the worker did not consume the segment, it
// would spin over an addressee who has nothing to deliver to, every tick
// until the end of time.
func TestNudge_NoDeviceStillConsumesTheSegment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	worker, _ := e.unacceptedCandidate(t, "nodevice")

	sc := NewScheduler(e.store, Config{})
	sc.tickAt(ctx, time.Now().UTC())

	if _, _, ok := e.rateLimit(t, worker, review.CategoryUnaccepted); !ok {
		t.Fatalf("row 84: the segment was not consumed")
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("row 84: delivery rows %d, expected one", n)
	}

	sender := &fakeSender{enabled: true}
	testsupport.TickPushUntilSettled(t, e.pool, worker, e.runner(sender).Tick)
	rows := e.pushRows(t, worker, review.CategoryUnaccepted)
	if len(rows) != 1 || rows[0].outcome == nil || *rows[0].outcome != "no_device" {
		got := "<nil>"
		if len(rows) == 1 && rows[0].outcome != nil {
			got = *rows[0].outcome
		}
		t.Errorf("row 84: outcome %q, expected no_device", got)
	}

	// Three more worker ticks — there must be no new notifications.
	for range 3 {
		sc.tickAt(ctx, time.Now().UTC())
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("row 84: for an addressee without devices a notification is spawned "+
			"every tick: %d in total", n)
	}
}

// ---------------------------------------------------------------------------
// Row 76, the "from delivery" half
// ---------------------------------------------------------------------------

// TestNudge_DeactivatedAddresseeIsNotDelivered — matrix row 76, second
// half.
//
// The addressee is deactivated AFTER formation: the formed row already sits
// in the queue. The per-element re-check at delivery time finds not a
// single issue, and the notification is completed without sending — rather
// than going to a person who is no longer in the system.
func TestNudge_DeactivatedAddresseeIsNotDelivered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	worker, _ := e.unacceptedCandidate(t, "deact-delivery")
	device := deviceEndpoint(testsupport.SeedPushSubscription(t, e.pool, worker))

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("precondition: the reminder was not formed")
	}

	if _, err := e.pool.Exec(ctx,
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, worker); err != nil {
		t.Fatalf("deactivation: %v", err)
	}

	sender := &fakeSender{enabled: true}
	testsupport.TickPushUntilSettled(t, e.pool, worker, e.runner(sender).Tick)
	if got := len(sender.callsTo(device)); got != 0 {
		t.Errorf("row 76: %d notifications were sent to a deactivated addressee", got)
	}
	rows := e.pushRows(t, worker, review.CategoryUnaccepted)
	if len(rows) != 1 || rows[0].state != "final" {
		t.Errorf("row 76: the delivery row is not completed: %+v", rows)
	}
}

// ---------------------------------------------------------------------------
// Row 106 — the audit is named after the verifiable fact
// ---------------------------------------------------------------------------

// TestNudge_AuditIsNamedAfterTheVerifiableFact — matrix row 106 (INV-10).
//
// For EACH of the two types. Not only the name is asserted: the audit
// record exists AT THE MOMENT the delivery row still sits in the queue and
// not a single send attempt has been made. That is the "verifiable fact":
// it is observable that the reminder is FORMED, not that it was sent —
// delivery is asynchronous and may end in failure.
//
// The expected type is taken by a TRANSLATION from the code, not a literal:
// a handwritten name would drift from the registry in silence.
func TestNudge_AuditIsNamedAfterTheVerifiableFact(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	e.seed(t, issue{suffix: "audit-unacc", status: "open", assignee: workerName,
		assigneeSetAt: ago(25 * time.Minute), createdBy: author})
	unrevID := e.seed(t, issue{suffix: "audit-unrev", status: "closed", assignee: workerName,
		createdBy: author, reviewRequired: true})
	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET closed_at = $2 WHERE id = $1`, unrevID,
		time.Now().UTC().Add(-13*time.Hour)); err != nil {
		t.Fatalf("shifting the close moment: %v", err)
	}

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())

	for _, tc := range []struct {
		cat  review.Category
		user uuid.UUID
	}{
		{review.CategoryUnaccepted, worker},
		{review.CategoryUnreviewed, author},
	} {
		wantType, ok := notifications.DigestEventTypeForCategory(tc.cat)
		if !ok {
			t.Fatalf("category %s has no event type", tc.cat)
		}
		var (
			gotType   string
			pushState string
			attempts  int
			outcome   *string
		)
		if err := e.pool.QueryRow(ctx, `
SELECT ev.type, n.push_state, n.push_attempts, n.push_outcome
  FROM events ev JOIN notifications n ON n.event_id = ev.id
 WHERE n.user_id = $1 AND n.reason = $2`,
			tc.user, reasonOf(t, tc.cat)).Scan(&gotType, &pushState, &attempts, &outcome); err != nil {
			t.Fatalf("row 106 (%s): there is no audit record: %v", tc.cat, err)
		}
		if gotType != wantType.String() {
			t.Errorf("row 106 (%s): record type %q, expected %q", tc.cat, gotType, wantType)
		}
		if pushState != "pending" || attempts != 0 || outcome != nil {
			t.Errorf("row 106 (%s): the audit record exists, and the delivery row is "+
				"already in state %q after %d attempts (outcome %v) — then the record "+
				"would speak of sending, not of formation", tc.cat, pushState, attempts, outcome)
		}
	}
}

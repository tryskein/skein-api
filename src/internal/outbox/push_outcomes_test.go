package outbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/push"
)

// push_outcomes_test.go — sk-303.13 / T010, coverage-matrix row 20: the
// taxonomy of what happens to a push that is owed.
//
// The taxonomy exists because the alternative — one "failed" bucket — makes
// the metric useless at the moment it is needed. Four of the outcomes are
// FINAL and must never produce a retry or leave a pending row behind; exactly
// one, the transport failure, is not an outcome at all until the attempts or
// the deadline run out. The spec forbids merging them, so the tests assert
// them one at a time and check the shape of the row afterwards.

// assertFinal checks the terminal shape of a row: the named outcome, no
// schedule left, and nothing for a later tick to pick up.
func assertFinal(t *testing.T, pool *pgxpool.Pool, id int64, wantOutcome string) {
	t.Helper()
	var (
		state       string
		outcome     *string
		attempts    int
		hasNext     bool
		hasLease    bool
		hasFinalise bool
	)
	if err := pool.QueryRow(context.Background(), `
SELECT push_state, push_outcome, push_attempts,
       push_next_attempt_at IS NOT NULL, push_leased_until IS NOT NULL,
       push_finalized_at IS NOT NULL
  FROM notifications WHERE id = $1`, id).
		Scan(&state, &outcome, &attempts, &hasNext, &hasLease, &hasFinalise); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if state != "final" {
		t.Errorf("push_state = %q, want final", state)
	}
	if outcome == nil || *outcome != wantOutcome {
		t.Errorf("push_outcome = %v, want %s", outcome, wantOutcome)
	}
	if hasNext {
		t.Error("push_next_attempt_at is still set on a final row — it would be retried")
	}
	if hasLease {
		t.Error("push_leased_until is still set on a final row")
	}
	if !hasFinalise {
		t.Error("push_finalized_at is not set — the retention sweep would never see this row")
	}
}

// hidePrefsTable makes the reason preferences unreadable for the duration of
// the case. The push runner reads them on its own connection, outside any
// transaction of ours, so the transaction-local shadow used on the write side
// cannot reach it — the table is renamed instead, and restored by the cleanup
// (which runs on a failed assertion too).
func hidePrefsTable(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE user_notification_reason_prefs RENAME TO unrp_hidden_outcomes_test`); err != nil {
		t.Fatalf("hide preferences table: %v", err)
	}
	restore := func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE IF EXISTS unrp_hidden_outcomes_test RENAME TO user_notification_reason_prefs`)
	}
	t.Cleanup(restore)
	return restore
}

// TestPushOutcomes_NoDeviceIsFinalAndNeverRetried — "Absence of devices is
// a legitimate final outcome, not an error: the signal is not repeated."
func TestPushOutcomes_NoDeviceIsFinalAndNeverRetried(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "pond") // deliberately WITHOUT a device
	id := f.owed(t, pushRow{})

	sender := &fakePushSender{enabled: true}
	runner := newRunner(pool, sender)
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertFinal(t, pool, id, outcomeNoDevice)

	// Two more passes: a final row is outside the claim predicate, so nothing
	// is retried and no attempt is spent.
	_, _, attemptsAfterFirst := pushStateOf(t, pool, id)
	for range 2 {
		if _, err := runner.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	if _, _, attempts := pushStateOf(t, pool, id); attempts != attemptsAfterFirst {
		t.Errorf("attempts grew from %d to %d after the outcome was final — the signal is being repeated",
			attemptsAfterFirst, attempts)
	}
	if len(sender.calls()) != 0 {
		t.Error("the sender was called for a recipient with no devices")
	}
}

// TestPushOutcomes_ConfigurationNoSendIsFinalAndLeavesNoBacklog — "a disabled
// sender does not leave an eternal pending". The row must not sit waiting for keys
// that may never arrive.
func TestPushOutcomes_ConfigurationNoSendIsFinalAndLeavesNoBacklog(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "poncf").withDevice(t)
	id := f.owed(t, pushRow{})

	sender := &fakePushSender{enabled: false}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	assertFinal(t, pool, id, outcomeConfigurationNoSend)
	if len(sender.calls()) != 0 {
		t.Error("a sender that reports itself disabled must not be called")
	}
	// No pending backlog anywhere for this recipient.
	var pending int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND push_state IN ('pending','leased')`,
		f.user).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d rows left pending for this recipient, want 0", pending)
	}
}

// TestPushOutcomes_PrefsUnavailableIsTerminalAndDistinct — Q6/SC-016. The
// preferences could not be READ. Nothing is sent, nothing is retried, and the
// outcome is its own value: merging it with a transport failure would hide a
// settings outage inside a provider outage.
func TestPushOutcomes_PrefsUnavailableIsTerminalAndDistinct(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "ponpu").withDevice(t)
	id := f.owed(t, pushRow{})

	before := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomePrefsUnavailable, causeNone))
	beforeTransport := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeTransportExhausted, causeNone))

	restore := hidePrefsTable(t, pool)
	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	restore()

	if len(callsTo(sender, f.endpoint)) != 0 {
		t.Error("the sender was called with the preferences unreadable — \"an error in the dangerous direction\"")
	}
	assertFinal(t, pool, id, outcomePrefsUnavailable)
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomePrefsUnavailable, causeNone)) - before; got != 1 {
		t.Errorf("prefs_unavailable series moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeTransportExhausted, causeNone)) - beforeTransport; got != 0 {
		t.Errorf("the transport series moved by %v — a settings outage was counted as a transport failure", got)
	}
}

// TestPushOutcomes_TransportFailureRetriesThenExhausts — SC-024. "A transport
// error is not an outcome: the row stays owed until attempts are exhausted,
// after which a final refusal is recorded, visible in the metric."
//
// The attempt budget is set to two so the whole arc fits in a test without
// waiting out a real back-off; the schedule is fast-forwarded between passes
// rather than slept through, because what is under test is the ARC, not the
// clock.
func TestPushOutcomes_TransportFailureRetriesThenExhausts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "pontr").withDevice(t)
	id := f.owed(t, pushRow{})

	beforeRetries := testutil.ToFloat64(pushRetriesTotal.WithLabelValues("delegated_status"))
	beforeExhausted := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeTransportExhausted, causeNone))

	sender := &fakePushSender{enabled: true}
	sender.setErr(errors.New("push endpoint returned status 500"))
	runner := NewPushRunner(pool, sender, push.NewStore(pool), "", PushOptions{MaxAttempts: 2})

	// Pass 1: the transport fails. This is NOT an outcome — the row goes back
	// to the queue with a delay.
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	state, outcome, attempts := pushStateOf(t, pool, id)
	if state != "pending" || outcome != nil || attempts != 1 {
		t.Fatalf("after one transport failure: %s/%v/%d attempts, want pending/<nil>/1", state, outcome, attempts)
	}
	var nextAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT push_next_attempt_at FROM notifications WHERE id = $1`, id).Scan(&nextAt); err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if nextAt == nil || !nextAt.After(time.Now()) {
		t.Errorf("push_next_attempt_at = %v, want a future time — the back-off must actually delay the retry", nextAt)
	}
	if got := testutil.ToFloat64(pushRetriesTotal.WithLabelValues("delegated_status")) - beforeRetries; got != 1 {
		t.Errorf("retry counter moved by %v, want 1", got)
	}

	// Fast-forward to the retry.
	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET push_next_attempt_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}

	// Pass 2: the budget is spent — now it IS an outcome.
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	assertFinal(t, pool, id, outcomeTransportExhausted)
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeTransportExhausted, causeNone)) - beforeExhausted; got != 1 {
		t.Errorf("transport_exhausted series moved by %v, want 1", got)
	}
	// The sender really was tried twice — the retry is a retry, not a no-op.
	if got := callsTo(sender, f.endpoint); len(got) != 2 {
		t.Errorf("send attempts = %d, want 2 (one per pass)", len(got))
	}
}

// TestPushOutcomes_ForbiddenIsFinalWithoutRetriesAndKeepsTheDevice — sk-627.
// A 403 from the push service is a refusal of our credentials (Apple answers
// it to a bad VAPID JWT), not a transport hiccup and not a dead device. With
// an attempt budget that would allow retries, one pass must finish the row as
// configuration_no_send / provider_forbidden, make exactly one send, schedule
// no retry, and leave the subscription in place.
func TestPushOutcomes_ForbiddenIsFinalWithoutRetriesAndKeepsTheDevice(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "po403").withDevice(t)
	id := f.owed(t, pushRow{})

	beforeRetries := testutil.ToFloat64(pushRetriesTotal.WithLabelValues("delegated_status"))
	beforeForbidden := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		"delegated_status", outcomeConfigurationNoSend, causeProviderForbidden))
	beforeDeviceForbidden := testutil.ToFloat64(
		pushDeliveriesTotal.WithLabelValues("delegated_status", "forbidden"))

	sender := &fakePushSender{enabled: true}
	sender.setErr(fmt.Errorf("sending web push: %w", &push.StatusError{
		StatusCode: 403, Host: "web.push.apple.com", Body: `{"reason":"BadJwtToken"}`,
	}))
	runner := NewPushRunner(pool, sender, push.NewStore(pool), "", PushOptions{MaxAttempts: 5})
	logs := captureOutboxLogs(t)

	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	assertFinal(t, pool, id, outcomeConfigurationNoSend)
	_, _, attempts := pushStateOf(t, pool, id)
	if attempts != 1 {
		t.Errorf("push_attempts = %d, want 1 — a 403 must not burn the attempt budget", attempts)
	}

	// The 403 path in sendPayload writes the diagnostic record.
	rec := logs.find(t, "push send failed")
	if rec == nil {
		t.Fatalf("no \"push send failed\" record on the 403 path; log: %s", logs.text())
	}
	if rec["status"] != float64(403) || rec["pushHost"] != "web.push.apple.com" ||
		rec["responseBody"] != `{"reason":"BadJwtToken"}` {
		t.Errorf("push send failed record = %v, want status 403, pushHost and the provider body", rec)
	}

	// Pull any retry the runner might have scheduled to now, as the transport
	// test does, so a second pass WOULD send again if the 403 had been
	// treated as transient. A final row is untouched by this (state guard).
	if _, err := pool.Exec(ctx, `UPDATE notifications SET push_next_attempt_at = now()
 WHERE id = $1 AND push_state = 'pending'`, id); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Errorf("send attempts after a due second pass = %d, want 1 — the 403 was retried", len(got))
	}
	if got := testutil.ToFloat64(pushRetriesTotal.WithLabelValues("delegated_status")) - beforeRetries; got != 0 {
		t.Errorf("retry counter moved by %v, want 0", got)
	}
	if got := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		"delegated_status", outcomeConfigurationNoSend, causeProviderForbidden)) - beforeForbidden; got != 1 {
		t.Errorf("configuration_no_send/provider_forbidden series moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(
		pushDeliveriesTotal.WithLabelValues("delegated_status", "forbidden")) - beforeDeviceForbidden; got != 1 {
		t.Errorf("per-device forbidden series moved by %v, want 1", got)
	}

	var devices int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, f.endpoint).Scan(&devices); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if devices != 1 {
		t.Errorf("subscriptions for the refusing endpoint = %d, want 1 — a 403 is not a dead device", devices)
	}
}

// TestPushOutcomes_DeadlineRetiresAnUnattemptedRow — the other bound. A row
// whose deadline passed while it was still owed is retired without a send: a
// push is worth making only while the action it reports is current, and the
// alternative is a pending row that outlives its own point.
func TestPushOutcomes_DeadlineRetiresAnUnattemptedRow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "pondl").withDevice(t)
	id := f.owed(t, pushRow{})

	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET push_deadline_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire deadline: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	assertFinal(t, pool, id, outcomeTransportExhausted)
	if len(callsTo(sender, f.endpoint)) != 0 {
		t.Error("a row past its deadline was still sent")
	}
	if _, _, attempts := pushStateOf(t, pool, id); attempts != 0 {
		t.Errorf("attempts = %d on a row retired by its deadline, want 0 — it was never tried", attempts)
	}
}

// TestPushOutcomes_AccessRevokedIsFinal — the last of the four final refusals,
// completing the taxonomy in one place so a future value cannot be added
// without a home here.
func TestPushOutcomes_AccessRevokedIsFinal(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "ponar").withDevice(t)
	id := f.owed(t, pushRow{})

	if _, err := pool.Exec(ctx,
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.projectID, f.user); err != nil {
		t.Fatalf("revoke access: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	runner := newRunner(pool, sender)
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertFinal(t, pool, id, outcomeAccessRevoked)
	if len(callsTo(sender, f.endpoint)) != 0 {
		t.Error("a revoked recipient was sent to")
	}
	// Final means final: another pass changes nothing.
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	assertFinal(t, pool, id, outcomeAccessRevoked)
}

// TestPushOutcomes_UnresolvableEventIsFinalAndNotBlamedOnTheProvider — W2 of
// the API-G1 review, and row 20's "every row ends in exactly ONE named
// outcome, and the name is true".
//
// A row whose event resolves to no issue used to `return` in silence: no log
// line at all, the row left leased, re-picked every lease, its attempt counter
// climbing past a budget that this path never checked — until
// retireExpiredDeadlines eventually recorded it as transport_exhausted, i.e.
// blamed a provider that was never called for an entity that no longer exists.
//
// The event and the notification are written in one transaction, so "no issue"
// is final, not "not yet". The row must end immediately, under its own
// sub-cause, and be visible in the log.
func TestPushOutcomes_UnresolvableEventIsFinalAndNotBlamedOnTheProvider(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	user := seedUser(t, pool)
	projectID := seedProject(t, pool, user)
	addProjectMember(t, pool, projectID, user)
	resetNotifications(t, pool, projectID)
	setReasonPush(t, pool, user, "assignment", true)
	endpoint := "https://push.example.com/pouev"
	seedPushSubscription(t, pool, user, endpoint)

	// An event pointing at an issue that does not exist. No issue is seeded
	// under this id, so issueForEvent finds nothing and errs on nothing.
	eventID := seedEvent(t, pool, "issue.status_changed", "issue", "pouev-gone-1", projectID)
	id := insertPushNotification(t, pool, user, eventID, "issue.status_changed", projectID,
		pushRow{reason: "assignment"})

	beforeUnresolvable := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		"assignment", outcomeConfigurationNoSend, causeEventUnresolvable))
	beforeTransport := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		"assignment", outcomeTransportExhausted, causeNone))

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, endpoint); len(got) != 0 {
		t.Errorf("the provider was called %d times for a row with no issue, want 0", len(got))
	}
	assertFinal(t, pool, id, outcomeConfigurationNoSend)
	if got := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		"assignment", outcomeConfigurationNoSend, causeEventUnresolvable)) - beforeUnresolvable; got != 1 {
		t.Errorf("event_unresolvable outcomes = %v, want 1 — the sub-cause is what keeps this "+
			"tellable apart from a disabled channel", got)
	}
	if got := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		"assignment", outcomeTransportExhausted, causeNone)) - beforeTransport; got != 0 {
		t.Errorf("transport_exhausted outcomes = %v, want 0 — a missing entity is not a "+
			"provider outage", got)
	}

	// And it stays finished: no second claim, no second attempt, no loop.
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	state, outcome, attempts := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
		t.Errorf("row = %s/%v, want final/%s", state, outcome, outcomeConfigurationNoSend)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — one claim, one decision, no re-picking", attempts)
	}
}

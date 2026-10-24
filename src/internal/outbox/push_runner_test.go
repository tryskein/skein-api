package outbox

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
)

// push_runner_test.go — sk-303.13 / T010, coverage-matrix row 19: what the
// push runner SELECTS and what it re-checks before it sends.
//
// Two properties are load-bearing and neither is visible from the send itself:
//
//   - the queue is EXPLICIT. A row is owed a push because it says so
//     (push_state='pending'), not because it happens to be in the feed. That
//     is what lets the two channels be independent instead of one implying the
//     other.
//
//   - the gates are re-read AT SEND TIME. The row was written inside somebody
//     else's transaction, possibly minutes ago; the recipient may have switched
//     the channel off or lost access since. Nothing about the issue is read
//     until both have been answered.
//
// Every assertion is scoped to this test's own row and endpoint: the runner
// drains the whole queue, and the throwaway database carries rows left by
// earlier packages.

// callsTo returns the sends the fake sender made to one endpoint.
func callsTo(sender *fakePushSender, endpoint string) []sentPush {
	var out []sentPush
	for _, c := range sender.calls() {
		if c.endpoint == endpoint {
			out = append(out, c)
		}
	}
	return out
}

// pushFixture is one recipient with one issue, one event and (optionally) one
// device, ready for the runner.
type pushFixture struct {
	pool      *pgxpool.Pool
	user      uuid.UUID
	projectID uuid.UUID
	issueID   string
	eventID   int64
	endpoint  string
}

func newPushFixture(t *testing.T, pool *pgxpool.Pool, key string) *pushFixture {
	t.Helper()
	user := seedUser(t, pool)
	projectID := seedProject(t, pool, user)
	addProjectMember(t, pool, projectID, user)
	resetNotifications(t, pool, projectID)

	issueID := key + "-1"
	seedIssue(t, pool, projectID, user, issueID, "Task "+key)
	eventID := seedEvent(t, pool, "issue.status_changed", "issue", issueID, projectID)
	setEventNewData(t, pool, eventID, `"closed"`)
	setReasonPush(t, pool, user, "delegated_status", true)

	return &pushFixture{
		pool: pool, user: user, projectID: projectID,
		issueID: issueID, eventID: eventID,
		endpoint: "https://push.example.com/" + key,
	}
}

func (f *pushFixture) withDevice(t *testing.T) *pushFixture {
	t.Helper()
	seedPushSubscription(t, f.pool, f.user, f.endpoint)
	return f
}

func (f *pushFixture) owed(t *testing.T, row pushRow) int64 {
	t.Helper()
	if row.reason == "" {
		row.reason = "delegated_status"
	}
	return insertPushNotification(t, f.pool, f.user, f.eventID, "issue.status_changed", f.projectID, row)
}

func newRunner(pool *pgxpool.Pool, sender PushSender) *PushRunner {
	return NewPushRunner(pool, sender, push.NewStore(pool), "https://app.example.com", PushOptions{})
}

// TestPushRunner_SendsToAnActiveDevice — SC-002 on the delivery side: a reason
// with its push channel on, a recipient with at least one active device, and
// the push goes out. The row is feed-only=false on purpose: push does not need
// the feed to exist.
func TestPushRunner_SendsToAnActiveDevice(t *testing.T) {
	pool := newTestPool(t)
	f := newPushFixture(t, pool, "prsa").withDevice(t)
	id := f.owed(t, pushRow{feedChannel: false})

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("sends to this device = %d, want 1", len(got))
	}
	state, outcome, attempts := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — one row, one attempt", attempts)
	}
}

// TestPushRunner_ClaimsOnlyExplicitPending — the queue is what the row SAYS it
// is. A feed row that owes no push (push_state='none') must never be claimed,
// and the runner must not touch its lifecycle columns.
func TestPushRunner_ClaimsOnlyExplicitPending(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "prep").withDevice(t)

	// A feed-only row: visible in every reading surface, owed to no device.
	var feedOnly int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, reason, feed_channel, push_state)
VALUES ($1, $2, 'issue.status_changed', $3, 'delegated_status', true, 'none')
RETURNING id`, f.user, f.eventID, f.projectID).Scan(&feedOnly); err != nil {
		t.Fatalf("insert feed-only row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE id = $1`, feedOnly) })

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("a feed-only row produced %d pushes, want 0 — the channels are independent", len(got))
	}
	state, outcome, attempts := pushStateOf(t, pool, feedOnly)
	if state != "none" || outcome != nil || attempts != 0 {
		t.Errorf("feed-only row lifecycle = %s/%v/%d, want none/<nil>/0 — the runner must not touch it",
			state, outcome, attempts)
	}
}

// TestPushRunner_HonoursTheNextAttemptSchedule — a pending row whose next
// attempt is in the future is not claimed yet. Without this the back-off would
// be decorative: a failing row would be retried on every tick.
func TestPushRunner_HonoursTheNextAttemptSchedule(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "prns").withDevice(t)
	id := f.owed(t, pushRow{})

	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_next_attempt_at = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("postpone: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("a postponed row was sent %d times, want 0", len(got))
	}
	state, _, attempts := pushStateOf(t, pool, id)
	if state != "pending" || attempts != 0 {
		t.Errorf("postponed row = %s/%d attempts, want pending/0", state, attempts)
	}
}

// TestPushRunner_RereadsPreferencesBeforeSending — INV-7 at delivery time. The
// row was written while the channel was on; the recipient has switched it off
// since. An explicit refusal outranks a queued signal, and the row is retired
// rather than left pending forever.
func TestPushRunner_RereadsPreferencesBeforeSending(t *testing.T) {
	pool := newTestPool(t)
	f := newPushFixture(t, pool, "prpr").withDevice(t)
	id := f.owed(t, pushRow{})

	// The user changes their mind AFTER the row exists.
	setReasonPush(t, pool, f.user, "delegated_status", false)

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("push sent %d times after the channel was switched off, want 0 (INV-7)", len(got))
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeConfigurationNoSend)
	}
}

// TestPushRunner_RereadsProjectAccessBeforeSending — "no project push
// reveals data to someone who lost access before the actual send".
//
// The assertion is not only "no send": the payload carries the issue's title,
// and the gate sits BEFORE the payload is built, so there is no window in
// which the title exists in a message addressed to someone who may no longer
// see it.
func TestPushRunner_RereadsProjectAccessBeforeSending(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "prac").withDevice(t)
	id := f.owed(t, pushRow{})

	// Access is revoked between the row being written and the send.
	if _, err := pool.Exec(ctx,
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.projectID, f.user); err != nil {
		t.Fatalf("revoke access: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Fatalf("push sent %d times to a recipient who lost access, want 0", len(got))
	}
	for _, c := range sender.calls() {
		if c.payload.IssueID == f.issueID {
			t.Errorf("the issue's payload was built for a revoked recipient: %+v", c.payload)
		}
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeAccessRevoked {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeAccessRevoked)
	}
}

// TestPushRunner_IsIndependentOfTheFeedAndOfNATS — the split, asserted from
// both sides on the same row.
//
// A push-only row (feed_channel=false) is delivered by a runner that has no
// NATS connection to hand; the NATS dispatcher, given the same row, does not
// see it at all — the live stream IS a feed, and a row that belongs to no feed
// does not belong in it.
func TestPushRunner_IsIndependentOfTheFeedAndOfNATS(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	_, nc := startEmbeddedNATS(t)

	f := newPushFixture(t, pool, "prin").withDevice(t)
	id := f.owed(t, pushRow{feedChannel: false})

	// The NATS side first: a live dispatcher must leave the row alone.
	dispatcher := New(pool, nc, Options{BatchSize: 50})
	if _, err := dispatcher.tickNotifications(ctx); err != nil {
		t.Fatalf("tickNotifications: %v", err)
	}
	if at := notifPublishedAt(t, pool, id); at != nil {
		t.Errorf("published_at = %v — a push-only row must not enter the live stream", at)
	}
	if state, _, _ := pushStateOf(t, pool, id); state != "pending" {
		t.Errorf("push_state = %q after a NATS tick, want it untouched at pending", state)
	}

	// The push side second: the runner delivers it with no NATS anywhere.
	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("sends = %d, want 1 — push does not need the feed", len(got))
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeSuccess)
	}
}

// TestPushRunner_OneRowNeverPushesTwice — at-least-once is the contract for a
// crash; a healthy pass must not double-send. The second tick has nothing left
// to claim because the first finalised the row.
func TestPushRunner_OneRowNeverPushesTwice(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "prtw").withDevice(t)
	f.owed(t, pushRow{})

	sender := &fakePushSender{enabled: true}
	runner := newRunner(pool, sender)
	for range 3 {
		if _, err := runner.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Errorf("three ticks produced %d sends for one row, want 1", len(got))
	}
}

// TestPushRunner_MentionPushLeadsToTheTask — SC-015 on the runner: a mention
// in a COMMENT is recorded on the comment entity, and the payload still
// resolves to the issue it hangs on. Before this the mention push would find
// no issue and be dropped, which is precisely the gap US-4 opened when it gave
// mentions a push at all.
func TestPushRunner_MentionPushLeadsToTheTask(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	user := seedUser(t, pool)
	projectID := seedProject(t, pool, user)
	addProjectMember(t, pool, projectID, user)
	resetNotifications(t, pool, projectID)
	seedIssue(t, pool, projectID, user, "prmn-1", "Mentioned here")

	commentID := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, 'prmn-1', 'tester', $2, 'human', 'hi @you', now(), now())`, commentID, user); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentID) })

	eventID := seedEvent(t, pool, "comment.mention", "comment", commentID.String(), projectID)
	endpoint := "https://push.example.com/prmn"
	seedPushSubscription(t, pool, user, endpoint)
	setReasonPush(t, pool, user, "mention", true)

	insertPushNotification(t, pool, user, eventID, "comment.mention", projectID,
		pushRow{reason: "mention", feedChannel: true})

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := callsTo(sender, endpoint)
	if len(got) != 1 {
		t.Fatalf("mention pushes = %d, want 1", len(got))
	}
	if got[0].payload.IssueID != "prmn-1" {
		t.Errorf("payload issueId = %q, want the parent issue prmn-1", got[0].payload.IssueID)
	}
	if want := "https://app.example.com/issues/prmn-1"; got[0].payload.URL != want {
		t.Errorf("payload url = %q, want %q — the push must lead to the task", got[0].payload.URL, want)
	}
	if got[0].payload.Body != "prmn-1 · Mentioned here" {
		t.Errorf("payload body = %q, want the issue id and title", got[0].payload.Body)
	}
}

// TestPushRunner_UnknownReasonIsNotSent — a row whose reason this build does
// not know cannot be checked against any preference, so it is not sent. Fail
// closed: an unknown reason is a configuration gap, not a licence.
func TestPushRunner_UnknownReasonIsNotSent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "prun").withDevice(t)
	id := f.owed(t, pushRow{})

	// The CHECK constraint pins the reason column to the registry, so the
	// unknown-reason case is produced by clearing it — the same shape a
	// pre-registry historical row has.
	if _, err := pool.Exec(ctx, `UPDATE notifications SET reason = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("clear reason: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("a row with no reason was pushed %d times, want 0", len(got))
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeConfigurationNoSend)
	}
}

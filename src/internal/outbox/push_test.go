package outbox

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/push"
)

// ----------------------------- Fixtures -----------------------------

// fakePushSender captures Send calls for assertions. enabled controls
// Enabled(); sendErr (when set) is returned from every Send so a test can
// drive the transport-failure branch of the lifecycle.
type fakePushSender struct {
	mu      sync.Mutex
	enabled bool
	sendErr error
	sent    []sentPush
}

type sentPush struct {
	endpoint string
	payload  pushPayload
	// raw is the payload EXACTLY as it went to the sender. The decoded form
	// above is the convenient one, but two assertions need the bytes: the
	// digest's length budget (sk-303.14 / T016, coverage-matrix row 67 — the promise is
	// about what the provider receives, and a re-marshal is a different
	// string) and the leak scan for a revoked project (row 64 — a title
	// must be absent from the WHOLE payload, not only from the field the test
	// remembered to look at).
	raw []byte
}

func (f *fakePushSender) Enabled() bool { return f.enabled }

func (f *fakePushSender) Send(_ context.Context, sub push.Subscription, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p pushPayload
	_ = json.Unmarshal(payload, &p)
	raw := make([]byte, len(payload))
	copy(raw, payload)
	f.sent = append(f.sent, sentPush{endpoint: sub.Endpoint, payload: p, raw: raw})
	return f.sendErr
}

func (f *fakePushSender) calls() []sentPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentPush, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakePushSender) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// resetNotifications marks THIS project's pending notifications published so a
// test only sees the rows it inserts. Scoped by project_id so a concurrent
// package's not-yet-dispatched notifications are never touched.
func resetNotifications(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE notifications SET published_at = now() WHERE published_at IS NULL AND project_id = $1`,
		projectID)
	if err != nil {
		t.Fatalf("reset notifications: %v", err)
	}
}

// seedIssue inserts a minimal issues row (id + title) so issueForEvent can
// resolve the payload. Registers cleanup.
func seedIssue(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, creator uuid.UUID, id, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, version, title, type, priority, status,
                    source, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, 1, $3, 'task', 3, 'open', 'human', $4, now(), now(), '{}'::jsonb)`,
		id, projectID, title, creator)
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, id)
	})
}

// pushRow is the shape a test needs a notification row to be in for the push
// runner to consider it: a reason, and an explicit pending push state with a
// deadline. Feed membership is independent and is set by the caller.
type pushRow struct {
	reason      string
	feedChannel bool
	deadline    time.Duration
}

// insertPushNotification appends a notification row owed to the push runner.
// Returns the new id.
func insertPushNotification(
	t *testing.T, pool *pgxpool.Pool,
	userID uuid.UUID, eventID int64, evtType string, projectID uuid.UUID, row pushRow,
) int64 {
	t.Helper()
	if row.deadline == 0 {
		row.deadline = time.Hour
	}
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO notifications (
    user_id, event_id, type, project_id, reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at, published_at)
VALUES ($1, $2, $3, $4, $5, $6,
        'pending', now(), now() + ($7::bigint * interval '1 second'), NULL)
RETURNING id`,
		userID, eventID, evtType, projectID, row.reason, row.feedChannel,
		int64(row.deadline/time.Second)).Scan(&id)
	if err != nil {
		t.Fatalf("insert push notification: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE id = $1`, id)
	})
	return id
}

// pushStateOf reads back the lifecycle columns of one notification row.
func pushStateOf(t *testing.T, pool *pgxpool.Pool, id int64) (state string, outcome *string, attempts int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT push_state, push_outcome, push_attempts FROM notifications WHERE id = $1`, id).
		Scan(&state, &outcome, &attempts); err != nil {
		t.Fatalf("read push state: %v", err)
	}
	return state, outcome, attempts
}

// seedPushSubscription inserts a push_subscriptions row for userID with a
// public https endpoint (passes ValidateEndpoint, though the fake sender never
// validates). Registers cleanup.
func seedPushSubscription(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, endpoint string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, created_at, last_seen_at)
VALUES ($1, $2, 'p256dh-fake', 'auth-fake', now(), now())`,
		userID, endpoint)
	if err != nil {
		t.Fatalf("seed push subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	})
}

// setEventNewData writes an event's new_data column (jsonb). Used to seed the
// status string a delegated-status push reads back.
func setEventNewData(t *testing.T, pool *pgxpool.Pool, eventID int64, newData string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE events SET new_data = $2::jsonb WHERE id = $1`, eventID, newData)
	if err != nil {
		t.Fatalf("set event new_data: %v", err)
	}
}

// setEventOldDataAndPayload fills the two columns sk-489 taught the runner's
// query to read. Separate helper from setEventNewData on purpose: a test that
// only needs the status must not be forced to state an outcome it is not
// asserting about.
func setEventOldDataAndPayload(t *testing.T, pool *pgxpool.Pool, eventID int64, oldData, payload string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE events SET old_data = NULLIF($2,'')::jsonb, payload = NULLIF($3,'')::jsonb WHERE id = $1`,
		eventID, oldData, payload)
	if err != nil {
		t.Fatalf("set event old_data/payload: %v", err)
	}
}

// notifPublishedAt loads a notification row's published_at.
func notifPublishedAt(t *testing.T, pool *pgxpool.Pool, id int64) *time.Time {
	t.Helper()
	var at *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT published_at FROM notifications WHERE id = $1`, id).Scan(&at)
	if err != nil {
		t.Fatalf("read notif published_at: %v", err)
	}
	return at
}

// addProjectMember makes the recipient a member of the project, so the push
// runner's send-time access re-check passes. Without it the row would finalise
// as access_revoked — which is the correct behaviour, and the reason the
// helper exists rather than being implied.
func addProjectMember(t *testing.T, pool *pgxpool.Pool, projectID, userID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $2)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID)
	if err != nil {
		t.Fatalf("add project member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, userID)
	})
}

// seedEvent appends an events row (already published, so the NATS dispatcher
// ignores it) and returns its id.
func seedEvent(t *testing.T, pool *pgxpool.Pool, evtType, entityType, entityID string, projectID uuid.UUID) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_type, entity_type, entity_id,
                    project_id, payload, published_at)
VALUES (now(), $1, 'tester', 'human', $2, $3, $4, '{}'::jsonb, now())
RETURNING id`, evtType, entityType, entityID, projectID).Scan(&id)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, id)
	})
	return id
}

// setReasonPush writes the recipient's push preference for one reason.
func setReasonPush(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, reason string, enabled bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, false, $3)
ON CONFLICT (user_id, reason) DO UPDATE SET push_enabled = EXCLUDED.push_enabled`,
		userID, reason, enabled)
	if err != nil {
		t.Fatalf("set reason push pref: %v", err)
	}
}

// ----------------------------- Tests -----------------------------

// TestPushRunner_DeliversPendingRowWithoutNATS is the sk-303.13 / T010 smoke:
// the push runner is constructed with no NATS anything — there is nowhere to
// pass a connection — and it still drains a pending row end to end.
//
// That is the whole point of the split. Until this wave `cmd/api/main.go` built
// the dispatcher (and with it the push path) only when the NATS connection was
// live, so an empty NATS_URL or a failed connect silently disabled every web
// push in the deployment.
func TestPushRunner_DeliversPendingRowWithoutNATS(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	user := seedUser(t, pool)
	projectID := seedProject(t, pool, user)
	addProjectMember(t, pool, projectID, user)
	resetNotifications(t, pool, projectID)
	seedIssue(t, pool, projectID, user, "pshr-1", "Push me")
	eventID := seedEvent(t, pool, "issue.status_changed", "issue", "pshr-1", projectID)
	setEventNewData(t, pool, eventID, `"closed"`)
	seedPushSubscription(t, pool, user, "https://push.example.com/pshr-1")
	setReasonPush(t, pool, user, "delegated_status", true)

	id := insertPushNotification(t, pool, user, eventID, "issue.status_changed", projectID,
		pushRow{reason: "delegated_status", feedChannel: true})

	sender := &fakePushSender{enabled: true}
	runner := NewPushRunner(pool, sender, push.NewStore(pool), "https://app.example.com", PushOptions{})

	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The runner drains the whole queue, and the throwaway database carries
	// rows left by earlier packages — so the assertions are scoped to THIS
	// test's endpoint rather than to the size of the batch.
	var mine []sentPush
	for _, c := range sender.calls() {
		if c.endpoint == "https://push.example.com/pshr-1" {
			mine = append(mine, c)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("Send called %d times for this recipient, want exactly 1 (one row, one push)", len(mine))
	}
	if want := "✅ Closed · tester"; mine[0].payload.Title != want {
		t.Errorf("payload title = %q, want %q", mine[0].payload.Title, want)
	}
	if mine[0].payload.Category != "delegated_status" {
		t.Errorf("payload category = %q, want the reason key", mine[0].payload.Category)
	}
	if mine[0].payload.IssueID != "pshr-1" {
		t.Errorf("payload issueId = %q, want pshr-1", mine[0].payload.IssueID)
	}

	state, outcome, attempts := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("push state/outcome = %s/%v, want final/success", state, outcome)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	// The push runner is not the NATS publisher: it must not touch the
	// live-feed outcome column.
	if at := notifPublishedAt(t, pool, id); at != nil {
		t.Errorf("published_at = %v — the push runner must not stamp the NATS outcome", at)
	}
}

// TestPushRunner_DeliversOutcomeInTheStatusPush — the second half of row 45
// of the sk-489 matrix, and without it the first proves nothing.
//
// ⚠ WHY THE buildPushPayload TABLE IS NOT ENOUGH. It calls the builder on an
// eventIssue ASSEMBLED BY HAND, and is therefore green for any runner query —
// including a query that does not read the new columns at all. Here the event
// rows are written to the database, the runner picks them up with ITS OWN query
// (push.go#issueForEvent), and what reached the device is checked.
//
// ⚠ WHY TWO PUSHES HERE, NOT ONE. The outcome travels by two different roads:
// the title of the status row (reason delegated_status, the payload of the closing
// event) and the title of the outcome-change row (reason outcome_changed, old_data
// and new_data). They read DIFFERENT columns, and a query that forgot one of them
// would leave half of the assertion green.
//
// ⚠ DEVIATION FROM THE LETTER OF ROW 45, NAMED EXPLICITLY (review A/5, finding F-5).
// The matrix requires running the runner "over the rows of these events written
// by the Store.UpdateStatus writer". Here the rows are SEEDED (seedEvent +
// setEventNewData + setEventOldDataAndPayload), so the letter of row 45
// is SPLIT between two tests, and this one is only half.
//
// What THIS test proves: the runner's query (push.go#issueForEvent) reads
// the new columns and carries them to the device. This is exactly what the
// buildPushPayload table does not prove, and that is why the test exists.
//
// What it does NOT prove: that the WRITER puts into those columns what the runner
// expects. The second half lives with the writers —
// notifications/outcome_fanout_test.go runs Store.Close and
// Store.UpdateStatus and reads THE SAME `closeOutcome` key from events.payload
// via hydrateNotificationDTO, while old_data / new_data are written by the same
// json.Marshal of the outcome string that outcomeChangedHead parses here.
// A later review closed the chain directly as well, with a temporary end-to-end probe
// (Store.UpdateStatus → PushRunner.Tick → titles on the device): there is no
// product divergence. The deviation is recorded here rather than fixed, precisely
// because seeding gives this test cases the writer never creates: an empty and a
// corrupted new_data of the neighbouring table — and it was on those that the
// loss of the outcome in the title was found.
func TestPushRunner_DeliversOutcomeInTheStatusPush(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	user := seedUser(t, pool)
	projectID := seedProject(t, pool, user)
	addProjectMember(t, pool, projectID, user)
	resetNotifications(t, pool, projectID)
	seedIssue(t, pool, projectID, user, "psho-1", "Will not do")
	seedPushSubscription(t, pool, user, "https://push.example.com/psho-1")
	setReasonPush(t, pool, user, "delegated_status", true)
	setReasonPush(t, pool, user, "outcome_changed", true)

	// Status row: the outcome lives in payload, the status in new_data.
	statusEvent := seedEvent(t, pool, "issue.status_changed", "issue", "psho-1", projectID)
	setEventNewData(t, pool, statusEvent, `"closed"`)
	setEventOldDataAndPayload(t, pool, statusEvent, "", `{"closeOutcome":"wont_do"}`)
	statusRow := insertPushNotification(t, pool, user, statusEvent, "issue.status_changed", projectID,
		pushRow{reason: "delegated_status", feedChannel: true})

	// Outcome-change row: the pair lives in old_data / new_data.
	outcomeEvent := seedEvent(t, pool, "issue.outcome_changed", "issue", "psho-1", projectID)
	setEventNewData(t, pool, outcomeEvent, `"wont_do"`)
	setEventOldDataAndPayload(t, pool, outcomeEvent, `"done"`,
		`{"closeOutcome":"wont_do","previousCloseOutcome":"done"}`)
	outcomeRow := insertPushNotification(t, pool, user, outcomeEvent, "issue.outcome_changed", projectID,
		pushRow{reason: "outcome_changed", feedChannel: true})

	sender := &fakePushSender{enabled: true}
	runner := NewPushRunner(pool, sender, push.NewStore(pool), "https://app.example.com", PushOptions{})
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var titles []string
	for _, c := range sender.calls() {
		if c.endpoint == "https://push.example.com/psho-1" {
			titles = append(titles, c.payload.Title)
		}
	}
	if len(titles) != 2 {
		t.Fatalf("Send was called %d time(s) for this recipient, expected 2: %v", len(titles), titles)
	}
	got := map[string]bool{}
	for _, tl := range titles {
		got[tl] = true
	}
	for _, want := range []string{
		"✅ Closed · won't do · tester",
		"🔀 Outcome: done → won't do · tester",
	} {
		if !got[want] {
			t.Errorf("title %q is not among the delivered ones: %v", want, titles)
		}
	}

	for name, id := range map[string]int64{"status": statusRow, "outcome change": outcomeRow} {
		state, outcome, _ := pushStateOf(t, pool, id)
		if state != "final" || outcome == nil || *outcome != outcomeSuccess {
			t.Errorf("row %s: state/outcome = %s/%v, expected final/success", name, state, outcome)
		}
	}
}

// TestPushRunner_SenderDisabledLeavesNoPendingBacklog — "a disabled sender does not
// leave an eternal pending". A deployment with no VAPID keys still receives
// push-eligible rows from the write path; somebody has to retire them.
func TestPushRunner_SenderDisabledLeavesNoPendingBacklog(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	user := seedUser(t, pool)
	projectID := seedProject(t, pool, user)
	addProjectMember(t, pool, projectID, user)
	resetNotifications(t, pool, projectID)
	seedIssue(t, pool, projectID, user, "pshd-1", "No keys here")
	eventID := seedEvent(t, pool, "issue.status_changed", "issue", "pshd-1", projectID)
	setReasonPush(t, pool, user, "delegated_status", true)

	id := insertPushNotification(t, pool, user, eventID, "issue.status_changed", projectID,
		pushRow{reason: "delegated_status"})

	sender := &fakePushSender{enabled: false}
	runner := NewPushRunner(pool, sender, push.NewStore(pool), "", PushOptions{})
	if _, err := runner.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(sender.calls()) != 0 {
		t.Error("a disabled sender must not be called at all")
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
		t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeConfigurationNoSend)
	}
}

// buildPushPayload renders "<emoji> <what> · <who>" as the title and
// "<id> · <title>" as the body, for every reason and every known status.
// Pure function — no DB.
func TestBuildPushPayload(t *testing.T) {
	t.Parallel()
	const base = "https://app.example.com"
	cases := []struct {
		name    string
		reason  string
		newData string
		// oldData / payload — sk-489. An empty string means SQL NULL: exactly
		// what sits in old event rows and in rows written
		// by a pod before the rollout — and the title must render as before.
		oldData   string
		payload   string
		actor     string
		actorType string
		wantTitle string
	}{
		// delegated_status — one case per known status.
		{"status open", "delegated_status", `"open"`, ``, ``, "bob", "human", "📋 Open · bob"},
		{"status in_progress", "delegated_status", `"in_progress"`, ``, ``, "bob", "human", "▶️ In progress · bob"},
		{"status deferred", "delegated_status", `"deferred"`, ``, ``, "bob", "human", "⏸️ Deferred · bob"},
		{"status closed", "delegated_status", `"closed"`, ``, ``, "bob", "human", "✅ Closed · bob"},
		{"status standing", "delegated_status", `"standing"`, ``, ``, "bob", "human", "♾️ Standing · bob"},

		// delegated_status degradations — the words degrade, the glyph never does.
		{"unknown status falls back to raw", "delegated_status", `"frobnicated"`, ``, ``, "bob", "human", "🔄 frobnicated · bob"},
		{"empty new_data falls back to neutral label", "delegated_status", ``, ``, ``, "bob", "human", "🔄 Changed · bob"},
		{"JSON empty string falls back to neutral label", "delegated_status", `""`, ``, ``, "bob", "human", "🔄 Changed · bob"},
		{"malformed new_data falls back to neutral label", "delegated_status", `{`, ``, ``, "bob", "human", "🔄 Changed · bob"},
		{"non-string new_data falls back to neutral label", "delegated_status", `{"status":"closed"}`, ``, ``, "bob", "human", "🔄 Changed · bob"},

		// ---- sk-489 / SC-011: the outcome in the title of the status push ----
		//
		// The first two rows are what the feature is for: a close WITHOUT WORK is visible from
		// the lock screen, not only after opening the issue.
		{"closed as wont_do names the outcome", "delegated_status", `"closed"`,
			``, `{"closeOutcome":"wont_do"}`, "bob", "human", "✅ Closed · won't do · bob"},
		{"closed as duplicate names the outcome", "delegated_status", `"closed"`,
			``, `{"closeOutcome":"duplicate"}`, "bob", "human", "✅ Closed · duplicate · bob"},
		// Three GUARDS of the previous text. Without them an implementation that appends
		// a tail to every close would be green: the value of the new row
		// is visible only against the fact that an ordinary close has not changed.
		{"closed as done keeps today's title", "delegated_status", `"closed"`,
			``, `{"closeOutcome":"done"}`, "bob", "human", "✅ Closed · bob"},
		{"closed without payload keeps today's title", "delegated_status", `"closed"`,
			``, ``, "bob", "human", "✅ Closed · bob"},
		{"closed with malformed payload keeps today's title", "delegated_status", `"closed"`,
			``, `{no`, "bob", "human", "✅ Closed · bob"},
		// An unknown outcome is printed as is (INV-2).
		{"unknown outcome is printed raw", "delegated_status", `"closed"`,
			``, `{"closeOutcome":"abandoned"}`, "bob", "human", "✅ Closed · abandoned · bob"},
		// Repeated close with an outcome change: the status title carries the NEW
		// outcome, not the old one — the payload contains both.
		{"repeat close names the new outcome", "delegated_status", `"closed"`,
			``, `{"closeOutcome":"duplicate","previousCloseOutcome":"done"}`, "bob", "human",
			"✅ Closed · duplicate · bob"},
		// INTERSECTION OF TWO DEGRADATIONS (rework after review A/5). Both halves of the
		// title are read from DIFFERENT columns and break independently;
		// the table above carries them separately, and before these three rows never
		// together. An early return on an unreadable new_data swallowed the tail with
		// the outcome, and a "won't do" close went out to the device as
		// "🔄 Changed": exactly the news SC-011 was
		// written for is lost. Reachable on rows written before the rollout or
		// corrupted — that is, exactly where it is hardest for the reader
		// to guess what they are not seeing.
		{"empty new_data still names the outcome", "delegated_status", ``,
			``, `{"closeOutcome":"wont_do"}`, "bob", "human", "🔄 Changed · won't do · bob"},
		{"malformed new_data still names the outcome", "delegated_status", `{`,
			``, `{"closeOutcome":"duplicate"}`, "bob", "human", "🔄 Changed · duplicate · bob"},
		// And the guard on the other side: a degraded title of a "done" close
		// gets no tail — otherwise the change would append a tail to every
		// close, and the previous two rows would prove nothing.
		{"empty new_data with done keeps the bare fallback", "delegated_status", ``,
			``, `{"closeOutcome":"done"}`, "bob", "human", "🔄 Changed · bob"},

		// ---- sk-489 / US-3: the reason outcome_changed ----
		{"outcome change is a pair", "outcome_changed", `"wont_do"`, `"done"`, ``,
			"bob", "human", "🔀 Outcome: done → won't do · bob"},
		{"outcome change between no-work outcomes", "outcome_changed", `"duplicate"`, `"wont_do"`, ``,
			"bob", "human", "🔀 Outcome: won't do → duplicate · bob"},
		{"outcome change with unknown values prints them raw", "outcome_changed", `"vanished"`, `"appeared"`, ``,
			"bob", "human", "🔀 Outcome: appeared → vanished · bob"},
		// Half of the pair is not printed: "Outcome: → duplicate" would claim about
		// the wrong side.
		{"outcome change without old_data drops the pair", "outcome_changed", `"duplicate"`, ``, ``,
			"bob", "human", "🔀 Outcome changed · bob"},
		{"outcome change with malformed data drops the pair", "outcome_changed", `{`, `{`, ``,
			"bob", "human", "🔀 Outcome changed · bob"},

		// The other reasons with a push channel.
		{"assignment", "assignment", ``, ``, ``, "bob", "human", "📥 Assigned to you · bob"},
		{"accepted", "accepted", ``, ``, ``, "bob", "human", "🤝 Accepted · bob"},
		{"reminder is system-actored — no actor tail", "reminder", ``, ``, ``, "system", "system", "🔔 Reminder"},
		// sk-303.13 / US-4: a mention finally has a push at all.
		{"mention", "mention", ``, ``, ``, "bob", "human", "💬 You were mentioned · bob"},

		// An agent actor is named like any human one.
		{"agent actor", "accepted", ``, ``, ``, "alice", "agent", "🤝 Accepted · alice"},

		// Actor-tail suppression: no dangling separator either way.
		{"empty actor drops the tail", "delegated_status", `"closed"`, ``, ``, "", "human", "✅ Closed"},
		{"system actor drops the tail", "delegated_status", `"closed"`, ``, ``, "system", "system", "✅ Closed"},

		// Unknown future reason names the app rather than lying.
		{"unknown reason", "some_future_reason", ``, ``, ``, "bob", "human", "Skein · bob"},
		{"unknown reason, system actor", "some_future_reason", ``, ``, ``, "system", "system", "Skein"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ei := eventIssue{
				issueID:   "dlg-1",
				title:     "My task",
				newData:   []byte(tc.newData),
				oldData:   []byte(tc.oldData),
				payload:   []byte(tc.payload),
				actor:     tc.actor,
				actorType: tc.actorType,
			}
			p := buildPushPayload(tc.reason, ei, base)
			if p.Title != tc.wantTitle {
				t.Errorf("title: got %q want %q", p.Title, tc.wantTitle)
			}
			// Body is the same shape for every reason: id anchors it.
			if want := "dlg-1 · My task"; p.Body != want {
				t.Errorf("body: got %q want %q", p.Body, want)
			}
			if p.Category != tc.reason {
				t.Errorf("category: got %q want %q (the payload now carries the reason key)", p.Category, tc.reason)
			}
			if p.IssueID != "dlg-1" {
				t.Errorf("issueId: got %q", p.IssueID)
			}
			if want := "https://app.example.com/issues/dlg-1"; p.URL != want {
				t.Errorf("url: got %q want %q", p.URL, want)
			}
		})
	}
}

// Every status the issues package accepts must have a push label. The list is
// read from issues.ValidStatuses() rather than re-typed here: a hand-copied
// list drifts in lockstep with the map it guards and would pass while the UI
// shows a raw English status — which is exactly how `standing` shipped
// unlabelled. Adding a status upstream now fails HERE until it gets a label.
func TestStatusLabelsCoverEveryValidStatus(t *testing.T) {
	t.Parallel()
	for _, s := range issues.ValidStatuses() {
		lbl, ok := statusLabels[s]
		if !ok {
			t.Errorf("statusLabels has no entry for status %q — its push title would render the raw English value", s)
			continue
		}
		if lbl.emoji == "" || lbl.label == "" {
			t.Errorf("statusLabels[%q] = %+v — both emoji and label are required", s, lbl)
		}
	}
}

// TestCloseOutcomeLabel_UnknownValuePrintsRaw: an outcome the push label map
// does not know is printed as is (INV-2), never dropped.
func TestCloseOutcomeLabel_UnknownValuePrintsRaw(t *testing.T) {
	t.Parallel()
	if got := closeOutcomeLabel("abandoned"); got != "abandoned" {
		t.Errorf("unknown outcome = %q, want it printed as is (INV-2)", got)
	}
}

// Every glyph in a push title must render in COLOUR. Unicode gives some
// codepoints (U+25B6 ▶, U+23F8 ⏸, U+267E ♾) text presentation by default —
// bare, they draw as a small monochrome glyph and stop being the scan anchor
// the title is built around. Such codepoints must carry U+FE0F. This asserts
// the rule structurally, so a new label can't quietly reintroduce a text-
// presentation glyph.
func TestStatusLabelEmojiRenderInColour(t *testing.T) {
	t.Parallel()
	// Codepoints with Emoji_Presentation=No (emoji-data.txt) — these are the
	// ones that need the selector. Extend when adding a label that uses one.
	needsSelector := map[rune]bool{'▶': true, '⏸': true, '♾': true}
	const vs16 = '️'

	labels := map[string]string{}
	for s, lbl := range statusLabels {
		labels[s] = lbl.emoji
	}
	labels["<unknown-status fallback>"] = statusUnknownEmoji

	for name, emoji := range labels {
		runes := []rune(emoji)
		if len(runes) == 0 {
			t.Errorf("%s: empty emoji", name)
			continue
		}
		if needsSelector[runes[0]] && (len(runes) < 2 || runes[1] != vs16) {
			t.Errorf("%s: emoji %q (U+%04X) has text presentation by default and needs a trailing U+FE0F",
				name, emoji, runes[0])
		}
	}
}

// With no appBaseURL configured the url stays relative for the SW to resolve
// against its own origin.
func TestBuildPushPayload_RelativeURLWithoutBase(t *testing.T) {
	t.Parallel()
	p := buildPushPayload("assignment", eventIssue{issueID: "dlg-2", title: "T", actor: "bob", actorType: "human"}, "")
	if want := "/issues/dlg-2"; p.URL != want {
		t.Errorf("url: got %q want %q", p.URL, want)
	}
}

// pushBackoff grows and is capped — a bounded delay, not an unbounded one.
func TestPushBackoff_GrowsAndIsCapped(t *testing.T) {
	t.Parallel()
	if got := pushBackoff(1); got != pushRetryBaseDelay {
		t.Errorf("first retry delay = %v, want %v", got, pushRetryBaseDelay)
	}
	if pushBackoff(3) <= pushBackoff(1) {
		t.Error("back-off must grow with attempts")
	}
	if got := pushBackoff(50); got != pushRetryMaxDelay {
		t.Errorf("back-off at attempt 50 = %v, want the cap %v", got, pushRetryMaxDelay)
	}
}

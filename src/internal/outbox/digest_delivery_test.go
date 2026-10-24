package outbox

// digest_delivery_test.go — sk-303.14, task T016.
//
// Integration class of the second payload kind. Owner of coverage-matrix
// rows 21, 62–68, 70, 71, 112, 113 (row 105 is the unit half, in
// digest_text_test.go).
//
// What is checked here and why against a REAL database, not mocks: the
// whole point of the per-element re-check is that between selection and
// send the issue's state in the database has changed. A mock of that
// re-check checks a mock. So every case below changes a row in the
// database BETWEEN seeding the payload and the runner's tick — exactly as
// it happens in life.
//
// Run: `./scripts/integration-test.sh -p 1 ./internal/outbox/...` from the
// workspace root. Explicit `-p 1` is required: the harness injects it ONLY
// when there are no arguments at all, and a narrowed scope replaces the
// default wholesale — without the flag the migrations package rolls the
// schema in parallel with other tests and they go red on serialisation,
// not on their own assertion.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/notifications"
	"skein-api/internal/push"
	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// digestFixture — one recipient with a device, one project visible to them
// and an issue counter so identifiers inside a case do not repeat.
type digestFixture struct {
	pool      *pgxpool.Pool
	user      uuid.UUID
	username  string
	other     uuid.UUID
	otherName string
	projectID uuid.UUID
	endpoint  string
	key       string
	seq       int
	// titles — the title of each seeded issue, NOT derived from its
	// identifier. The independence is needed by the lost-access assertion:
	// a title glued from the id "did not leak" together with it, and a
	// title check would become a repeat of the identifier check.
	titles map[string]string
}

func newDigestFixture(t *testing.T, pool *pgxpool.Pool, key string) *digestFixture {
	t.Helper()
	user, uname := testsupport.SeedUser(t, pool, "user")
	other, oname := testsupport.SeedUser(t, pool, "user")
	projectID, _ := testsupport.SeedProject(t, pool, user, "dg"+key)
	addProjectMember(t, pool, projectID, user)

	f := &digestFixture{
		pool: pool, user: user, username: uname,
		other: other, otherName: oname,
		projectID: projectID,
		endpoint:  "https://push.example.com/digest-" + key,
		key:       key,
		titles:    map[string]string{},
	}
	seedPushSubscription(t, pool, user, f.endpoint)
	return f
}

// nextIssueID yields an identifier unique inside the case. Length is set
// by the caller: an issue identifier in the system is varchar(1000), and
// that is not theory, it is the upper bound the list must fit into.
func (f *digestFixture) nextIssueID(pad int) string {
	f.seq++
	base := fmt.Sprintf("dg%s-%03d", f.key, f.seq)
	if pad > len(base) {
		base += strings.Repeat("x", pad-len(base))
	}
	return base
}

// nextTitle yields a title not derived from the issue identifier.
func (f *digestFixture) nextTitle(prefix string) string {
	return prefix + " " + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
}

// unacceptedIn seeds an issue that belongs to the UNACCEPTED set for the
// recipient: not closed, assignee is the recipient, no acceptance.
func (f *digestFixture) unacceptedIn(t *testing.T, projectID uuid.UUID, pad int) string {
	t.Helper()
	id := f.nextIssueID(pad)
	title := f.nextTitle("Issue")
	f.titles[id] = title
	_, err := f.pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, version, title, type, priority, status,
                    source, created_by, assignee, accepted_at,
                    created_at, updated_at, metadata)
VALUES ($1, $2, 1, $3, 'task', 3, 'open', 'human', $4, $5, NULL,
        now(), now(), '{}'::jsonb)`,
		id, projectID, title, f.other, f.username)
	if err != nil {
		t.Fatalf("seed unaccepted issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, id)
	})
	return id
}

func (f *digestFixture) unaccepted(t *testing.T) string {
	return f.unacceptedIn(t, f.projectID, 0)
}

// unreviewedIn seeds an issue that belongs to the UNREVIEWED set for the
// recipient: closed, created by the recipient, an assignee is set and it
// is not the recipient, the flag is on, there is no review mark.
func (f *digestFixture) unreviewedIn(t *testing.T, projectID uuid.UUID, pad int) string {
	t.Helper()
	id := f.nextIssueID(pad)
	title := f.nextTitle("Result")
	f.titles[id] = title
	_, err := f.pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, version, title, type, priority, status,
                    source, created_by, assignee, review_required, reviewed_at,
                    created_at, updated_at, closed_at, metadata)
VALUES ($1, $2, 1, $3, 'task', 3, 'closed', 'human', $4, $5, true, NULL,
        now(), now(), now(), '{}'::jsonb)`,
		id, projectID, title, f.user, f.otherName)
	if err != nil {
		t.Fatalf("seed unreviewed issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, id)
	})
	return id
}

func (f *digestFixture) unreviewed(t *testing.T) string {
	return f.unreviewedIn(t, f.projectID, 0)
}

// digestRow writes a digest event and a push-only notification row for it
// — exactly the pair the nudge worker produces in production.
//
// project_id on both rows is NULL because a digest is addressed to a
// PERSON, not to a project, and spans many projects at once. That is why
// the tract's per-row access gate is idle for it and the re-check must be
// per-element.
func (f *digestFixture) digestRow(t *testing.T, cat review.Category, ids []string) (notifID, eventID int64) {
	t.Helper()
	evtType, ok := notifications.DigestEventTypeForCategory(cat)
	if !ok {
		t.Fatalf("no digest event type for category %q", cat)
	}
	reason, ok := notifications.DigestReasonForCategory(cat)
	if !ok {
		t.Fatalf("no digest reason for category %q", cat)
	}
	payload, err := notifications.MarshalDigestPayload(notifications.DigestPayload{IssueIDs: ids})
	if err != nil {
		t.Fatalf("marshal digest payload: %v", err)
	}

	ctx := context.Background()
	if err := f.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_type, entity_type, entity_id,
                    project_id, payload, published_at)
VALUES (now(), $1, 'system', 'system', 'user', $2, NULL, $3::jsonb, now())
RETURNING id`, evtType, f.user.String(), string(payload)).Scan(&eventID); err != nil {
		t.Fatalf("seed digest event: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID) })

	if err := f.pool.QueryRow(ctx, `
INSERT INTO notifications (
    user_id, event_id, type, project_id, reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at, published_at)
VALUES ($1, $2, $3, NULL, $4, false,
        'pending', now(), now() + interval '1 hour', NULL)
RETURNING id`, f.user, eventID, evtType, string(reason)).Scan(&notifID); err != nil {
		t.Fatalf("seed digest notification: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(ctx, `DELETE FROM notifications WHERE id = $1`, notifID) })
	return notifID, eventID
}

// tick runs the runner until the recipient's own due rows are processed —
// however many rows other tests left ahead of them in the shared queue —
// and returns the sender.
func (f *digestFixture) tick(t *testing.T) *fakePushSender {
	t.Helper()
	sender := &fakePushSender{enabled: true}
	testsupport.TickPushUntilSettled(t, f.pool, f.user, newRunner(f.pool, sender).Tick)
	return sender
}

// delivered takes the single payload delivered to the recipient's device
// and parses it. Fatal on any other count — "delivered zero" and
// "delivered twice" are different defects, and both must fail here rather
// than turn into an unreadable parse error below.
func delivered(t *testing.T, sender *fakePushSender, endpoint string) pushPayload {
	t.Helper()
	calls := callsTo(sender, endpoint)
	if len(calls) != 1 {
		t.Fatalf("sends to the recipient's device = %d, want exactly 1", len(calls))
	}
	return calls[0].payload
}

// deliveredRaw is the same single delivery, as the bytes that went out.
func deliveredRaw(t *testing.T, sender *fakePushSender, endpoint string) []byte {
	t.Helper()
	calls := callsTo(sender, endpoint)
	if len(calls) != 1 {
		t.Fatalf("sends to the recipient's device = %d, want exactly 1", len(calls))
	}
	return calls[0].raw
}

// listedIDs parses the list from a digest body: everything between " · "
// and the " and N more" tail. Also returns the remainder (-1 if there is
// no tail).
func listedIDs(t *testing.T, body string) (ids []string, rest int) {
	t.Helper()
	sep := strings.Index(body, " · ")
	if sep < 0 {
		return nil, -1
	}
	list := body[sep+len(" · "):]
	rest = -1
	if tail := strings.Index(list, digestRemainderSep); tail >= 0 {
		if _, err := fmt.Sscanf(list[tail:], digestRemainderSep+"%d more", &rest); err != nil {
			t.Fatalf("the remainder tail %q is not a number", list[tail:])
		}
		list = list[:tail]
	}
	return strings.Split(list, ", "), rest
}

// bodyCount reads the declared count from the body: "<subject>: <N>".
func bodyCount(t *testing.T, body string) int {
	t.Helper()
	colon := strings.Index(body, ": ")
	if colon < 0 {
		t.Fatalf("the digest body %q carries no count", body)
	}
	rest := body[colon+2:]
	var n int
	if _, err := fmt.Sscanf(rest, "%d", &n); err != nil {
		t.Fatalf("the digest body %q does not start its count with a number", body)
	}
	return n
}

// outcomeOf reads back the stored terminal outcome of a row.
func outcomeOf(t *testing.T, pool *pgxpool.Pool, id int64) (state string, outcome string) {
	t.Helper()
	var o *string
	if err := pool.QueryRow(context.Background(),
		`SELECT push_state, push_outcome FROM notifications WHERE id = $1`, id).
		Scan(&state, &o); err != nil {
		t.Fatalf("read push state: %v", err)
	}
	if o == nil {
		return state, ""
	}
	return state, *o
}

// ---------------------------------------------------------------------------
// Rows 113, 112, 66 — routes, list limit, deterministic order
// ---------------------------------------------------------------------------

// TestDigest_SeveralUnacceptedNameCountListAndInboxRoute — rows 113 and 112.
//
// Several UNACCEPTED: the count equals the number of unaccepted, the list
// is bounded by the limit, the remainder goes as a number, the route leads
// to the inbox — not to the card of some one of the seven issues, because
// no such one exists.
func TestDigest_SeveralUnacceptedNameCountListAndInboxRoute(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "rt1")

	ids := make([]string, 0, 7)
	for range 7 {
		ids = append(ids, f.unaccepted(t))
	}
	f.digestRow(t, review.CategoryUnaccepted, ids)

	p := delivered(t, f.tick(t), f.endpoint)

	if got := bodyCount(t, p.Body); got != 7 {
		t.Errorf("declared count = %d, want 7 — it must equal the number of unaccepted", got)
	}
	listed, rest := listedIDs(t, p.Body)
	if len(listed) != digestListLimit {
		t.Errorf("listed identifiers = %d, want %d (the list limit)", len(listed), digestListLimit)
	}
	if rest != 7-digestListLimit {
		t.Errorf("remainder = %d, want %d — the remainder must go as a number", rest, 7-digestListLimit)
	}
	if !strings.HasSuffix(p.URL, digestRouteInbox) {
		t.Errorf("route = %q, want suffix %q — several unaccepted lead to the inbox", p.URL, digestRouteInbox)
	}
	if p.IssueID != "" {
		t.Errorf("issueId = %q, want empty: there is no single issue a digest of seven points at", p.IssueID)
	}
}

// TestDigest_SeveralUnreviewedRouteToDelegatedWithSelection — the second
// half of row 113: several UNREVIEWED lead to the delegated surface with
// the selection APPLIED, not just to the surface.
//
// Two URL parameters, not one: the lens says which surface, the selection
// says which subset of it. Dropping the second would open the whole lens
// to the person instead of the results the notification is about.
func TestDigest_SeveralUnreviewedRouteToDelegatedWithSelection(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "rt2")

	ids := []string{f.unreviewed(t), f.unreviewed(t), f.unreviewed(t)}
	f.digestRow(t, review.CategoryUnreviewed, ids)

	p := delivered(t, f.tick(t), f.endpoint)

	if !strings.Contains(p.URL, "lens=delegated") {
		t.Errorf("route %q does not contain the delegated lens", p.URL)
	}
	if !strings.Contains(p.URL, "select=unreviewed") {
		t.Errorf("route %q does not carry the applied unreviewed selection — "+
			"the tap will open the whole lens instead of the results the notification is about", p.URL)
	}
}

// TestDigest_SingleIssueLeadsToItsCard — the third part of row 113: ONE
// issue leads to its card.
func TestDigest_SingleIssueLeadsToItsCard(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "rt3")

	id := f.unaccepted(t)
	f.digestRow(t, review.CategoryUnaccepted, []string{id})

	p := delivered(t, f.tick(t), f.endpoint)

	if want := "/issues/" + id; !strings.HasSuffix(p.URL, want) {
		t.Errorf("route = %q, want suffix %q — one issue leads to its card", p.URL, want)
	}
	if p.IssueID != id {
		t.Errorf("issueId = %q, want %q", p.IssueID, id)
	}
	if !strings.Contains(p.Body, id) {
		t.Errorf("body %q does not name the issue the notification is about", p.Body)
	}
}

// TestDigest_OrderIsDeterministicAcrossSegments — row 66.
//
// Stability ACROSS SEGMENTS, not inside one: two different notifications,
// formed in different segments over the same set of issues, must yield a
// byte-identical body. The second payload deliberately lists the same
// identifiers in REVERSE order — if order were set by the payload, not by
// the query, the bodies would diverge.
func TestDigest_OrderIsDeterministicAcrossSegments(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "ord")

	ids := make([]string, 0, 6)
	for range 6 {
		ids = append(ids, f.unaccepted(t))
	}
	reversed := make([]string, len(ids))
	for i, id := range ids {
		reversed[len(ids)-1-i] = id
	}

	f.digestRow(t, review.CategoryUnaccepted, ids)
	first := delivered(t, f.tick(t), f.endpoint)

	f2 := *f
	f2.endpoint = f.endpoint + "-b"
	seedPushSubscription(t, pool, f.user, f2.endpoint)
	f2.digestRow(t, review.CategoryUnaccepted, reversed)
	sender := f2.tick(t)
	second := delivered(t, sender, f2.endpoint)

	if first.Body != second.Body {
		t.Errorf("two segments over the same set of issues yielded different bodies:\n  1: %q\n  2: %q\n"+
			"order must be set by the query, not by the order of identifiers in the payload",
			first.Body, second.Body)
	}
}

// ---------------------------------------------------------------------------
// Row 68 — categories do not merge
// ---------------------------------------------------------------------------

// TestDigest_CategoriesDoNotMerge — row 68.
//
// One person has both unaccepted and unreviewed at the same time. That is
// TWO notifications with different titles and different routes, not one
// text with two sections: merging them would mean unsubscribing from one
// category silences the other, and that a tap leads where half the
// contents are missing.
func TestDigest_CategoriesDoNotMerge(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "cat")

	unacc := []string{f.unaccepted(t), f.unaccepted(t)}
	unrev := []string{f.unreviewed(t), f.unreviewed(t)}
	f.digestRow(t, review.CategoryUnaccepted, unacc)
	f.digestRow(t, review.CategoryUnreviewed, unrev)

	sender := f.tick(t)
	calls := callsTo(sender, f.endpoint)
	if len(calls) != 2 {
		t.Fatalf("notifications delivered = %d, want 2 — one per category", len(calls))
	}

	byCategory := map[string]pushPayload{}
	for _, c := range calls {
		byCategory[c.payload.Category] = c.payload
	}
	if len(byCategory) != 2 {
		t.Fatalf("distinct reasons among the delivered = %d, want 2 — the categories have merged", len(byCategory))
	}

	a := byCategory[string(notifications.ReasonKeyUnacceptedReminder)]
	b := byCategory[string(notifications.ReasonKeyReviewReminder)]
	if a.Title == b.Title {
		t.Errorf("category titles coincided (%q) — they are indistinguishable by title", a.Title)
	}
	if a.URL == b.URL {
		t.Errorf("category routes coincided (%q) — a tap leads to one place from two different notifications", a.URL)
	}
	for _, id := range unrev {
		if strings.Contains(a.Body, id) {
			t.Errorf("the unaccepted digest names the unreviewed result %s — the categories have merged", id)
		}
	}
	for _, id := range unacc {
		if strings.Contains(b.Body, id) {
			t.Errorf("the unreviewed digest names the unaccepted issue %s — the categories have merged", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Rows 62, 63 — a stale reminder and an emptied digest
// ---------------------------------------------------------------------------

// TestDigest_StaleElementIsDroppedAndCountRecomputed — row 62.
//
// An issue leaves the set BETWEEN selection and send. A stale reminder is
// not allowed by the product: the element is dropped, the count and the
// tail are recomputed. The snapshot from selection time is not used — if
// it were, the person would read "Issues awaiting acceptance: 5" after
// they had accepted two of them.
func TestDigest_StaleElementIsDroppedAndCountRecomputed(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "stl")

	ids := make([]string, 0, 5)
	for range 5 {
		ids = append(ids, f.unaccepted(t))
	}
	f.digestRow(t, review.CategoryUnaccepted, ids)

	// Between selection and send the person accepted two issues.
	gone := ids[:2]
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET accepted_at = now() WHERE id = ANY($1::text[])`, gone); err != nil {
		t.Fatalf("accept two issues: %v", err)
	}

	p := delivered(t, f.tick(t), f.endpoint)

	if got := bodyCount(t, p.Body); got != 3 {
		t.Errorf("count = %d, want 3 — it is counted from those that survived the re-check, not from the payload", got)
	}
	for _, id := range gone {
		if strings.Contains(p.Body, id) {
			t.Errorf("accepted issue %s stayed in the body — that is the stale reminder", id)
		}
	}
	listed, rest := listedIDs(t, p.Body)
	if len(listed) != 3 {
		t.Errorf("listed = %d, want 3", len(listed))
	}
	if rest != -1 {
		t.Errorf("the \"and %d more\" tail remained even though all three are listed — the tail must be recomputed", rest)
	}
}

// TestDigest_ReviewMarkDropsElement — the same row 62 from the second
// category's side: a review mark takes a result out of the set the same
// way acceptance takes an issue out.
func TestDigest_ReviewMarkDropsElement(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "rvm")

	ids := []string{f.unreviewed(t), f.unreviewed(t), f.unreviewed(t)}
	f.digestRow(t, review.CategoryUnreviewed, ids)

	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET reviewed_at = now(), reviewed_by = $2 WHERE id = $1`,
		ids[0], f.user); err != nil {
		t.Fatalf("mark reviewed: %v", err)
	}

	p := delivered(t, f.tick(t), f.endpoint)

	if got := bodyCount(t, p.Body); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	if strings.Contains(p.Body, ids[0]) {
		t.Errorf("reviewed result %s stayed in the body — \"you have 3 unreviewed\" after a review", ids[0])
	}
}

// TestDigest_EmptyAfterRecheckFinishesWithoutSend — row 63.
//
// Everything emptied — the notification is NOT sent and is recorded as an
// outcome. Empty copy ("Issues awaiting acceptance: 0") is worse than
// silence: it asserts an untruth about the person and teaches them not to
// trust notifications.
func TestDigest_EmptyAfterRecheckFinishesWithoutSend(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "emp")

	ids := []string{f.unaccepted(t), f.unaccepted(t)}
	id := func() int64 { n, _ := f.digestRow(t, review.CategoryUnaccepted, ids); return n }()

	before := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		string(notifications.ReasonKeyUnacceptedReminder), outcomeConfigurationNoSend, causeDigestEmpty))

	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET accepted_at = now() WHERE id = ANY($1::text[])`, ids); err != nil {
		t.Fatalf("accept every issue: %v", err)
	}

	sender := f.tick(t)
	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Fatalf("delivered %d notifications, want 0 — an emptied digest is not sent", len(got))
	}
	state, outcome := outcomeOf(t, pool, id)
	if state != "final" {
		t.Errorf("push_state = %q, want final — the row must finish, not stay in the queue", state)
	}
	if outcome != outcomeConfigurationNoSend {
		t.Errorf("push_outcome = %q, want %q", outcome, outcomeConfigurationNoSend)
	}
	after := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		string(notifications.ReasonKeyUnacceptedReminder), outcomeConfigurationNoSend, causeDigestEmpty))
	if after-before != 1 {
		t.Errorf("outcome increment with cause %q = %v, want 1 — otherwise an emptied digest "+
			"is indistinguishable from a configuration refusal", causeDigestEmpty, after-before)
	}
}

// TestDigest_OversizePayloadIsFinalisedLoudlyNotBlamedOnTheTransport —
// the last fitting frontier, on the tract as a whole.
//
// The unit half of the fork lives in digest_text_test.go; what is checked
// here is the CONSEQUENCE that is only visible on the tract: an unfit
// payload does not go to the sender, the row is finished with a knowing
// refusal and its own sub-cause and does NOT go into retries. Were it to
// go there it would exhaust attempts and be recorded as a transport
// failure, sending incident triage to a provider nobody bothered. That is
// the false attribution the budget comment names as a risk by name.
//
// Degeneracy of the data is deliberately put in the URL, not in the
// identifiers: the tap URL is not shortened by anything in the budget, so
// this is the only way to reach the last step without bypassing list
// fitting.
func TestDigest_OversizePayloadIsFinalisedLoudlyNotBlamedOnTheTransport(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "dgov")
	ids := []string{f.unaccepted(t), f.unaccepted(t), f.unaccepted(t)}
	id, _ := f.digestRow(t, review.CategoryUnaccepted, ids)

	before := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		string(notifications.ReasonKeyUnacceptedReminder), outcomeConfigurationNoSend, causeDigestOversize))

	// The URL base is longer than the budget itself: even the count-only
	// form does not fit.
	sender := &fakePushSender{enabled: true}
	base := "https://" + strings.Repeat("a", digestPayloadBudgetBytes) + ".example.com"
	runner := NewPushRunner(pool, sender, push.NewStore(pool), base, PushOptions{})
	testsupport.TickPushUntilSettled(t, pool, f.user, runner.Tick)

	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Fatalf("sent %d payloads, want 0 — something that cannot fit must not "+
			"reach the sender", len(got))
	}
	state, outcome := outcomeOf(t, pool, id)
	if state != "final" {
		t.Errorf("push_state = %q, want final — the row went into retries and will be recorded as a transport failure", state)
	}
	if outcome != outcomeConfigurationNoSend {
		t.Errorf("push_outcome = %q, want %q — \"we knowingly did not send\", not a provider failure",
			outcome, outcomeConfigurationNoSend)
	}
	after := testutil.ToFloat64(pushOutcomesTotal.WithLabelValues(
		string(notifications.ReasonKeyUnacceptedReminder), outcomeConfigurationNoSend, causeDigestOversize))
	if after-before != 1 {
		t.Errorf("outcome increment with cause %q = %v, want 1 — degenerate data must be "+
			"tellable apart from a knowing refusal to send", causeDigestOversize, after-before)
	}
}

// ---------------------------------------------------------------------------
// Row 64 — lost access (INV-14, SC-021)
// ---------------------------------------------------------------------------

// TestDigest_RevokedProjectHidesIdAndTitleAndCount — row 64.
//
// The recipient is a member of project A and is NOT a member of project B.
// Issues from B are revealed by neither identifier nor title and are not
// counted. Checked against the whole payload, not only the body: the
// title, the URL and any other field is as much a leak channel as the
// body.
func TestDigest_RevokedProjectHidesIdAndTitleAndCount(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "acc")

	// Project B: the recipient does NOT see it — only the owner makes
	// them a member, and here the owner is another person.
	foreignID, _ := testsupport.SeedProject(t, pool, f.other, "dgfor"+f.key)

	mine := []string{f.unaccepted(t), f.unaccepted(t)}
	hidden := f.unacceptedIn(t, foreignID, 0)
	hiddenTitle := f.titles[hidden]

	f.digestRow(t, review.CategoryUnaccepted, append(append([]string{}, mine...), hidden))

	sender := f.tick(t)
	p := delivered(t, sender, f.endpoint)
	raw := string(deliveredRaw(t, sender, f.endpoint))
	if got := bodyCount(t, p.Body); got != len(mine) {
		t.Errorf("count = %d, want %d — an inaccessible issue is not counted", got, len(mine))
	}
	if strings.Contains(raw, hidden) {
		t.Errorf("the payload reveals the identifier of the inaccessible issue %s", hidden)
	}
	if strings.Contains(raw, hiddenTitle) {
		t.Errorf("the payload reveals the title of the inaccessible issue (%q)", hiddenTitle)
	}
}

// TestDigest_SoleInaccessibleElementLeaksNeitherIdNorTitle — the second
// half of row 64, and it does NOT follow from the first.
//
// In a digest of several issues the body carries only identifiers, so the
// case above only proves "not revealed by identifier": nobody there has a
// title, and the title assertion in it is empty. A title enters the body
// exactly when ONE element remains after the re-check — and that is the
// only case in which "nor by title" is checkable.
//
// Here the payload consists of one issue whose project the recipient has
// no access to. Correct behaviour: send nothing. Behaviour without the
// re-check: send "identifier · title" of someone else's issue.
func TestDigest_SoleInaccessibleElementLeaksNeitherIdNorTitle(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "sol")

	foreignID, _ := testsupport.SeedProject(t, pool, f.other, "dgsol"+f.key)
	hidden := f.unacceptedIn(t, foreignID, 0)
	hiddenTitle := f.titles[hidden]

	id, _ := f.digestRow(t, review.CategoryUnaccepted, []string{hidden})

	sender := f.tick(t)
	calls := callsTo(sender, f.endpoint)
	for _, c := range calls {
		raw := string(c.raw)
		if strings.Contains(raw, hidden) {
			t.Errorf("sent the identifier of the inaccessible issue %s", hidden)
		}
		if strings.Contains(raw, hiddenTitle) {
			t.Errorf("sent the TITLE of the inaccessible issue (%q)", hiddenTitle)
		}
	}
	if len(calls) != 0 {
		t.Errorf("delivered %d notifications, want 0 — no accessible issue remained", len(calls))
	}
	if state, outcome := outcomeOf(t, pool, id); state != "final" || outcome != outcomeConfigurationNoSend {
		t.Errorf("state/outcome = %s/%s, want final/%s", state, outcome, outcomeConfigurationNoSend)
	}
}

// TestDigest_UnreviewedRevokedProjectIsHidden — the same row 64 for the
// second category. It takes its OWN path: the visibility horizon sits
// inside the unreviewed fragment, it is not added by the tract — so a
// green first case proves nothing about the second.
func TestDigest_UnreviewedRevokedProjectIsHidden(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "acr")

	foreignID, _ := testsupport.SeedProject(t, pool, f.other, "dgfr"+f.key)
	// The issue in the foreign project was created by the RECIPIENT
	// (otherwise it is not in their set by authorship, and the case would
	// be checking the wrong thing). They have no visibility of the
	// project.
	mine := []string{f.unreviewed(t), f.unreviewed(t)}
	hidden := f.unreviewedIn(t, foreignID, 0)
	hiddenTitle := f.titles[hidden]

	f.digestRow(t, review.CategoryUnreviewed, append(append([]string{}, mine...), hidden))

	sender := f.tick(t)
	p := delivered(t, sender, f.endpoint)
	raw := string(deliveredRaw(t, sender, f.endpoint))
	if got := bodyCount(t, p.Body); got != len(mine) {
		t.Errorf("count = %d, want %d", got, len(mine))
	}
	if strings.Contains(raw, hidden) {
		t.Errorf("the payload reveals the identifier of an issue from an invisible project %s", hidden)
	}
	if strings.Contains(raw, hiddenTitle) {
		t.Errorf("the payload reveals the title of an issue from an invisible project (%q)", hiddenTitle)
	}
}

// ---------------------------------------------------------------------------
// Row 65 — fail-closed
// ---------------------------------------------------------------------------

// TestDigest_RecheckFailureLeavesRowLeasedAndRetries — row 65.
//
// A READ error on the re-check is not a reason to send "as-is". The row
// stays leased and is retried. Sending on an unread access check is the
// one thing this gate exists for.
//
// The failure is staged by renaming the table the per-element re-check
// reads: a transactional shadow cannot be slipped to the runner, it uses
// its own connection. The same trick, for the same reason, is already
// used in push_outcomes_test.go.
func TestDigest_RecheckFailureLeavesRowLeasedAndRetries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newDigestFixture(t, pool, "fcl")

	ids := []string{f.unaccepted(t), f.unaccepted(t)}
	id, _ := f.digestRow(t, review.CategoryUnaccepted, ids)

	if _, err := pool.Exec(ctx,
		`ALTER TABLE project_members RENAME TO pm_hidden_digest_test`); err != nil {
		t.Fatalf("hide project_members: %v", err)
	}
	restore := func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE IF EXISTS pm_hidden_digest_test RENAME TO project_members`)
	}
	t.Cleanup(restore)

	sender := f.tick(t)
	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Fatalf("delivered %d notifications on an unread re-check, want 0", len(got))
	}
	state, outcome := outcomeOf(t, pool, id)
	if state != "leased" {
		t.Errorf("push_state = %q, want leased — fail-closed leaves the row leased, "+
			"it does not finish it with an outcome", state)
	}
	if outcome != "" {
		t.Errorf("push_outcome = %q, want empty — a deferred row must not have an outcome", outcome)
	}

	// The cause of the failure is gone — the lease expires, the row comes
	// back and goes out.
	restore()
	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET push_leased_until = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	sender2 := f.tick(t)
	if got := callsTo(sender2, f.endpoint); len(got) != 1 {
		t.Fatalf("after lifting the failure, delivered %d, want 1 — the row must be retried", len(got))
	}
	if state, outcome := outcomeOf(t, pool, id); state != "final" || outcome != outcomeSuccess {
		t.Errorf("after the retry state/outcome = %s/%s, want final/%s", state, outcome, outcomeSuccess)
	}
}

// ---------------------------------------------------------------------------
// Row 67 — length budget and list integrity
// ---------------------------------------------------------------------------

// TestDigest_FullListOfMaximalIdentifiersFitsTheBudget — row 67.
//
// An issue identifier in the system is varchar(1000). Five such
// identifiers are five kilobytes, which is well over a web-push record:
// without fitting the message would not go out AT ALL and would be
// recorded as a transport failure, sending incident triage to a provider
// nobody bothered.
//
// Both halves of the requirement are checked: the payload fits the budget
// AND the list is not cut in the middle — every shown identifier equals a
// whole seeded one, not its prefix.
func TestDigest_FullListOfMaximalIdentifiersFitsTheBudget(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "bdg")

	const maxIssueIDLen = 1000
	ids := make([]string, 0, 7)
	for range 7 {
		ids = append(ids, f.unacceptedIn(t, f.projectID, maxIssueIDLen))
	}
	f.digestRow(t, review.CategoryUnaccepted, ids)

	calls := callsTo(f.tick(t), f.endpoint)
	if len(calls) != 1 {
		t.Fatalf("delivered %d, want 1", len(calls))
	}
	raw := calls[0].raw

	if len(raw) > push.MaxPayloadBytes {
		t.Errorf("payload %d bytes — over the web-push record ceiling (%d): "+
			"the provider will not accept it, and the outcome will lie about the transport", len(raw), push.MaxPayloadBytes)
	}
	if len(raw) > digestPayloadBudgetBytes {
		t.Errorf("payload %d bytes — over the budget %d", len(raw), digestPayloadBudgetBytes)
	}

	p := calls[0].payload
	if got := bodyCount(t, p.Body); got != 7 {
		t.Errorf("count = %d, want 7 — fitting shortens the list, not the count", got)
	}
	listed, rest := listedIDs(t, p.Body)
	if len(listed) == 0 {
		t.Fatalf("the list is empty: fitting ate everything, even though the budget holds several identifiers")
	}
	if len(listed) >= digestListLimit {
		t.Errorf("listed %d of %d — at maximum-length identifiers fitting MUST "+
			"have shortened the list, otherwise the case does not check the budget", len(listed), digestListLimit)
	}
	whole := map[string]bool{}
	for _, id := range ids {
		whole[id] = true
	}
	for _, shown := range listed {
		if !whole[shown] {
			t.Errorf("the list contains %q — that is not a whole identifier, "+
				"the list was cut in the middle", shown[:min(40, len(shown))]+"…")
		}
	}
	if want := 7 - len(listed); rest != want {
		t.Errorf("remainder = %d, want %d", rest, want)
	}
}

// ---------------------------------------------------------------------------
// Row 70 — a digest row never appears in any feed
// ---------------------------------------------------------------------------

// TestDigest_RowNeverAppearsInAnyFeed — row 70 (INV-7, SC-012).
//
// Checked by STORAGE STATE through the real feed readers, not by reading
// a column the test itself wrote: the tautology "put false — read false"
// does not close the row. The readers are the ones that serve the inbox
// and its counter.
//
// Retries do not create feed entries because otherwise the same fact
// would sit in the feed as many times as there were segments the person
// did not react to.
func TestDigest_RowNeverAppearsInAnyFeed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newDigestFixture(t, pool, "fed")

	ids := []string{f.unaccepted(t), f.unaccepted(t)}
	notifID, _ := f.digestRow(t, review.CategoryUnaccepted, ids)

	// The half of the assertion that is NOT tautological: a row with
	// feed_channel is written in production by the producer, and the
	// decision "the feed does not apply to this reason" lives in the
	// reason registry — the one the producer reads. The assertion "put
	// false — read false" is worth nothing on its own; together with the
	// registry the chain is closed.
	for _, key := range []notifications.ReasonKey{
		notifications.ReasonKeyUnacceptedReminder,
		notifications.ReasonKeyReviewReminder,
	} {
		spec, known := notifications.LookupReason(key)
		if !known {
			t.Fatalf("reason %s is absent from the registry", key)
		}
		if spec.Feed.Applicable {
			t.Errorf("reason %s is declared applicable to the feed channel — the producer will write "+
				"a feed row, and the same fact will sit in the feed as many times as "+
				"there were segments the person did not react to", key)
		}
	}

	store := notifications.NewStore(pool)
	countsBefore, err := store.Counts(ctx, f.user, nil)
	if err != nil {
		t.Fatalf("counts before: %v", err)
	}

	if got := callsTo(f.tick(t), f.endpoint); len(got) != 1 {
		t.Fatalf("delivered %d, want 1 — the case checks the feed AFTER a completed delivery", len(got))
	}

	for _, scope := range []string{"unread", "all", "archived"} {
		res, err := store.ListInbox(ctx, notifications.ListInboxArgs{
			UserID: f.user, Scope: scope, Limit: 100,
		})
		if err != nil {
			t.Fatalf("list inbox (%s): %v", scope, err)
		}
		for _, item := range res.Items {
			if item.ID == notifID {
				t.Errorf("the digest row is visible in the feed (scope=%s) — reminders do not create feed entries", scope)
			}
		}
	}

	countsAfter, err := store.Counts(ctx, f.user, nil)
	if err != nil {
		t.Fatalf("counts after: %v", err)
	}
	if countsAfter.Total != countsBefore.Total || countsAfter.Unread != countsBefore.Unread {
		t.Errorf("the feed counter moved from %+v to %+v — the digest landed in it", countsBefore, countsAfter)
	}
}

// ---------------------------------------------------------------------------
// Row 71 — the new terminal supersede outcome
// ---------------------------------------------------------------------------

// TestDigest_SupersededOutcomeIsStoredAndDistinct — row 71.
//
// Supersede must be tellable apart from the other "did not send" IN THE
// ROW'S STATE, not only in a metric label: a sub-cause inside an existing
// outcome is indistinguishable in the database, so the outcome set was
// extended by a migration.
//
// Three assertions, and none is tautological:
//   - the schema ACCEPTS the new value (without migration 000044 the write
//     would fail);
//   - the schema still REJECTS an invented value, i.e. the set stayed
//     closed and was not opened along the way;
//   - a row finished by supersede is not picked up by the runner again and
//     is read from storage as supersede, not as a configuration refusal.
func TestDigest_SupersededOutcomeIsStoredAndDistinct(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newDigestFixture(t, pool, "sup")

	ids := []string{f.unaccepted(t)}
	id, _ := f.digestRow(t, review.CategoryUnaccepted, ids)

	if _, err := pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'final', push_outcome = $2, push_finalized_at = now(),
       push_next_attempt_at = NULL, push_leased_until = NULL
 WHERE id = $1`, id, outcomeSuperseded); err != nil {
		t.Fatalf("the schema did not accept outcome %q: %v — the outcome set was not extended by the migration", outcomeSuperseded, err)
	}

	state, outcome := outcomeOf(t, pool, id)
	if state != "final" || outcome != outcomeSuperseded {
		t.Fatalf("state/outcome = %s/%s, want final/%s", state, outcome, outcomeSuperseded)
	}
	for _, other := range []string{outcomeConfigurationNoSend, outcomeAccessRevoked, outcomeTransportExhausted} {
		if outcome == other {
			t.Errorf("supersede is indistinguishable from %q in the row's state", other)
		}
	}

	// The set stayed CLOSED: the schema must reject an invented value.
	// Without this half the previous one would only prove that the
	// constraint was lifted.
	if _, err := pool.Exec(ctx,
		`UPDATE notifications SET push_outcome = 'displaced_by_a_fresher_one' WHERE id = $1`, id); err == nil {
		t.Errorf("the schema accepted an invented outcome — the outcome set is no longer closed")
	}

	sender := f.tick(t)
	if got := callsTo(sender, f.endpoint); len(got) != 0 {
		t.Errorf("a superseded row was delivered (%d sends) — a terminal outcome is not terminal", len(got))
	}
	if state, outcome := outcomeOf(t, pool, id); state != "final" || outcome != outcomeSuperseded {
		t.Errorf("after the tick state/outcome = %s/%s, want final/%s — the runner overwrote someone else's outcome",
			state, outcome, outcomeSuperseded)
	}
}

// ---------------------------------------------------------------------------
// Row 21 — unsubscribe by the same mechanism (INV-9)
// ---------------------------------------------------------------------------

// TestDigest_UnsubscribeSilencesOneCategoryOnly — row 21.
//
// Unsubscribing from a category is checked by THE SAME mechanism as for
// other notifications: the tract reads user_notification_reason_prefs by
// the row's reason — one query, one code, before it builds anything. A
// queue of its own would have required a second implementation of this
// check, which is exactly why it was rejected.
//
// The second half of the case is just as required: unsubscribing from ONE
// category does not silence the other. Categories do not merge here
// either.
func TestDigest_UnsubscribeSilencesOneCategoryOnly(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "uns")

	unacc := []string{f.unaccepted(t), f.unaccepted(t)}
	unrev := []string{f.unreviewed(t), f.unreviewed(t)}
	offID, _ := f.digestRow(t, review.CategoryUnaccepted, unacc)
	onID, _ := f.digestRow(t, review.CategoryUnreviewed, unrev)

	setReasonPush(t, pool, f.user, string(notifications.ReasonKeyUnacceptedReminder), false)

	sender := f.tick(t)
	calls := callsTo(sender, f.endpoint)
	if len(calls) != 1 {
		t.Fatalf("delivered %d, want 1 — exactly one of the two categories is silenced", len(calls))
	}
	p := calls[0].payload
	if p.Category != string(notifications.ReasonKeyReviewReminder) {
		t.Errorf("delivered category %q, want %q — the wrong category was silenced",
			p.Category, notifications.ReasonKeyReviewReminder)
	}

	if state, outcome := outcomeOf(t, pool, offID); state != "final" || outcome != outcomeConfigurationNoSend {
		t.Errorf("row of the unsubscribed category: state/outcome = %s/%s, want final/%s",
			state, outcome, outcomeConfigurationNoSend)
	}
	if state, outcome := outcomeOf(t, pool, onID); state != "final" || outcome != outcomeSuccess {
		t.Errorf("row of the NOT-unsubscribed category: state/outcome = %s/%s, want final/%s — "+
			"unsubscribing from one category silenced the other", state, outcome, outcomeSuccess)
	}
}

// TestDigest_LostAccessDropsFromTheNextDigest — sk-674.18 matrix row 32a
// (SC-003, the digest). The recipient is a member of a second project B and
// one of its issues is in the digest set alongside two of the recipient's
// own. While the recipient sees B, the issue is delivered (the positive
// control: the access check does not drop what is visible). Then the
// recipient loses B; the NEXT digest of the same set carries neither the
// issue's identifier, nor its title, nor a link to it, and does not count
// it — the recipient's own issues stay.
func TestDigest_LostAccessDropsFromTheNextDigest(t *testing.T) {
	pool := newTestPool(t)
	f := newDigestFixture(t, pool, "lst")

	sharedID, _ := testsupport.SeedProject(t, pool, f.other, "dglst"+f.key)
	addProjectMember(t, pool, sharedID, f.user)

	mine := []string{f.unaccepted(t), f.unaccepted(t)}
	shared := f.unacceptedIn(t, sharedID, 0)
	sharedTitle := f.titles[shared]
	set := append(append([]string{}, mine...), shared)

	f.digestRow(t, review.CategoryUnaccepted, set)
	before := f.tick(t)
	if got := bodyCount(t, delivered(t, before, f.endpoint).Body); got != len(set) {
		t.Fatalf("while the recipient sees B: count = %d, want %d", got, len(set))
	}
	if !strings.Contains(string(deliveredRaw(t, before, f.endpoint)), shared) {
		t.Fatalf("while the recipient sees B the digest does not carry %s — the case would prove nothing", shared)
	}

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, sharedID, f.user); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}

	f.digestRow(t, review.CategoryUnaccepted, set)
	after := f.tick(t)
	p := delivered(t, after, f.endpoint)
	raw := string(deliveredRaw(t, after, f.endpoint))
	if got := bodyCount(t, p.Body); got != len(mine) {
		t.Errorf("after losing B: count = %d, want %d — the lost issue is not counted", got, len(mine))
	}
	if strings.Contains(raw, shared) {
		t.Errorf("after losing B the digest still carries the identifier (or a link with it) of %s", shared)
	}
	if strings.Contains(raw, sharedTitle) {
		t.Errorf("after losing B the digest still carries the title of %s (%q)", shared, sharedTitle)
	}
	for _, id := range mine {
		if !strings.Contains(raw, id) {
			t.Errorf("the recipient's own %s dropped from the digest", id)
		}
	}
}

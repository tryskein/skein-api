package issues

// optout_autowatch_test.go — sk-622.21, T011, matrix rows No. 24 and No. 24a.
//
// The automatic subscription paths of the issues store — assignment
// (PatchIssue), reopen (UpdateStatus from closed), creation of a subtask
// with an assignee — against an opt-out (US-1, INV-2) and, until the
// reconciliation, an old-version tombstone (K3, INV-16); the close path
// (US-2: automatic subscriptions stripped without an opt-out and without
// clearing cards) and the reopen order (US-6: subscriptions come back BEFORE
// the reopen is fanned out, so the returning author's notification is
// carded). Every block is checked against the pair (user, issue): the same
// paths subscribe a neighbouring pair in the same fixture, or a block would
// be indistinguishable from "the path subscribes nobody".
//
// Expected values are the US-1 / US-2 / US-6 rules. Skips without
// TEST_DATABASE_URL.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
)

type optAutoFix struct {
	pool      *pgxpool.Pool
	store     *Store
	notif     *notifications.Store
	owner     uuid.UUID
	ownerName string
	pid       uuid.UUID
	prefix    string
}

func newOptAutoFix(t *testing.T) *optAutoFix {
	t.Helper()
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	return &optAutoFix{
		pool: pool, store: NewStore(pool), notif: notifications.NewStore(pool),
		owner: owner, ownerName: readUsername(t, pool, owner), pid: pid, prefix: prefix,
	}
}

func (f *optAutoFix) member(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	u := seedUser(t, f.pool)
	addProjectMember(t, f.pool, f.pid, u, f.owner)
	return u, readUsername(t, f.pool, u)
}

func (f *optAutoFix) ownerActor() Actor { return HumanActor(f.owner, f.ownerName) }

func (f *optAutoFix) create(t *testing.T, in CreateIssueInput) *IssueDetail {
	t.Helper()
	if in.Type == "" {
		in.Type, in.Priority = "task", 2
	}
	if in.Title == "" {
		in.Title = "opt-out autowatch"
	}
	d, err := f.store.CreateIssue(context.Background(), f.pid, f.prefix, "counter", in, f.owner, f.ownerActor())
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return d
}

func (f *optAutoFix) version(t *testing.T, id string) int {
	t.Helper()
	var v int
	if err := f.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

func (f *optAutoFix) assign(t *testing.T, id, username string, actor Actor) {
	t.Helper()
	if _, err := f.store.PatchIssue(context.Background(), id,
		PatchIssueInput{ExpectedVersion: f.version(t, id), Assignee: &username}, actor); err != nil {
		t.Fatalf("assign %s to %s: %v", id, username, err)
	}
}

func (f *optAutoFix) close(t *testing.T, id string, actor Actor) {
	t.Helper()
	if _, err := f.store.Close(context.Background(), CloseInput{
		ID: id, ExpectedVersion: f.version(t, id), Reason: "done", Actor: actor,
	}); err != nil {
		t.Fatalf("Close %s: %v", id, err)
	}
}

func (f *optAutoFix) reopen(t *testing.T, id string, actor Actor) {
	t.Helper()
	if _, err := f.store.UpdateStatus(context.Background(), id,
		UpdateStatusInput{ExpectedVersion: f.version(t, id), Status: StatusOpen}, actor); err != nil {
		t.Fatalf("reopen %s: %v", id, err)
	}
}

func (f *optAutoFix) optedOut(t *testing.T, id string, u uuid.UUID) bool {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, id, u).Scan(&n); err != nil {
		t.Fatalf("count opt-outs: %v", err)
	}
	return n > 0
}

// subscribedBy reports whether u has a LIVE subscription on id carrying
// reason.
func (f *optAutoFix) subscribedBy(t *testing.T, id string, u uuid.UUID, reason string) bool {
	t.Helper()
	rs := watcherReasons(t, f.pool, id, u)
	return !hasReason(rs, "unwatched") && hasReason(rs, reason)
}

// TestOptOutAutoWatch_ExplicitUnwatchBlocksTheAutomaticPaths — No. 24: after
// an explicit unwatch of X, U is not subscribed to X by being assigned, by
// a reopen, or by a subtask assigned to U; on the same paths U is
// subscribed to the neighbouring Y and to the subtask, and V to X.
func TestOptOutAutoWatch_ExplicitUnwatchBlocksTheAutomaticPaths(t *testing.T) {
	f := newOptAutoFix(t)
	ctx := context.Background()
	u, uName := f.member(t)
	v, vName := f.member(t)
	x := f.create(t, CreateIssueInput{})
	y := f.create(t, CreateIssueInput{})

	if err := f.notif.AddIssueWatch(ctx, x.ID, u, false); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := f.notif.RemoveIssueWatch(ctx, x.ID, u); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	if !f.optedOut(t, x.ID, u) {
		t.Fatalf("precondition: opt-out (U, X) recorded")
	}

	// Assignment.
	f.assign(t, x.ID, uName, f.ownerActor())
	if n := countWatcherRows(t, f.pool, x.ID, u); n != 0 {
		t.Errorf("assigning U to X subscribed U despite the opt-out: %v", watcherReasons(t, f.pool, x.ID, u))
	}
	f.assign(t, y.ID, uName, f.ownerActor())
	if !f.subscribedBy(t, y.ID, u, "assignee") {
		t.Errorf("control: assigning U to Y did not subscribe U (reasons %v)", watcherReasons(t, f.pool, y.ID, u))
	}

	// Reopen with U as the assignee (U has no prefs row: unwatch on close is on).
	f.close(t, x.ID, f.ownerActor())
	f.reopen(t, x.ID, f.ownerActor())
	if n := countWatcherRows(t, f.pool, x.ID, u); n != 0 {
		t.Errorf("reopening X subscribed U despite the opt-out: %v", watcherReasons(t, f.pool, x.ID, u))
	}

	// A subtask assigned to U: its own issue, so U is subscribed to it.
	sub := f.create(t, CreateIssueInput{ParentID: &x.ID, Assignee: &uName})
	if !f.subscribedBy(t, sub.ID, u, "assignee") {
		t.Errorf("control: the subtask assigned to U did not subscribe U (reasons %v)", watcherReasons(t, f.pool, sub.ID, u))
	}
	if n := countWatcherRows(t, f.pool, x.ID, u); n != 0 {
		t.Errorf("creating a subtask assigned to U subscribed U to X")
	}

	// Another user on the same issue and the same paths.
	f.assign(t, x.ID, vName, f.ownerActor())
	if !f.subscribedBy(t, x.ID, v, "assignee") {
		t.Errorf("control: assigning V to X did not subscribe V (reasons %v)", watcherReasons(t, f.pool, x.ID, v))
	}
	f.close(t, x.ID, f.ownerActor())
	f.reopen(t, x.ID, f.ownerActor())
	if !f.subscribedBy(t, x.ID, v, "assignee") {
		t.Errorf("control: reopening X did not give V the automatic subscription back (reasons %v)",
			watcherReasons(t, f.pool, x.ID, v))
	}
	if n := countWatcherRows(t, f.pool, x.ID, u); n != 0 {
		t.Errorf("the second reopen subscribed U")
	}
}

// TestOptOutAutoWatch_StateSweepThenReopenResubscribes — No. 24: "unsubscribe from
// closed" records no opt-out, so a reopen gives the automatic
// subscription back. U keeps the subscription through the close (setting
// off), sweeps it, turns the setting on, and the reopen returns it.
func TestOptOutAutoWatch_StateSweepThenReopenResubscribes(t *testing.T) {
	f := newOptAutoFix(t)
	ctx := context.Background()
	u, uName := f.member(t)
	x := f.create(t, CreateIssueInput{Assignee: &uName})
	if !f.subscribedBy(t, x.ID, u, "assignee") {
		t.Fatalf("precondition: U subscribed as the assignee")
	}
	setUnwatchOnClose(t, f.pool, u, false)
	f.close(t, x.ID, f.ownerActor())

	res, err := f.notif.BulkUnwatch(ctx, u, notifications.BulkUnwatchInput{State: notifications.WatchingStateClosed})
	if err != nil {
		t.Fatalf("BulkUnwatch: %v", err)
	}
	if res.Unwatched != 1 || countWatcherRows(t, f.pool, x.ID, u) != 0 || f.optedOut(t, x.ID, u) {
		t.Fatalf("precondition: the sweep removed the subscription without an opt-out (answer %+v)", res)
	}

	setUnwatchOnClose(t, f.pool, u, true)
	f.reopen(t, x.ID, f.ownerActor())
	if !f.subscribedBy(t, x.ID, u, "assignee") {
		t.Errorf("reopen after \"from closed\" did not give the automatic subscription back (reasons %v)",
			watcherReasons(t, f.pool, x.ID, u))
	}
}

// TestOptOutAutoWatch_CloseStripsWithoutOptOutAndReopenIsCarded — No. 24, US-2,
// US-6 "closed" / "reopened": another member closes the author's issue —
// the author's automatic subscription is taken away AFTER the close
// notification was written with card X (no opt-out, the card stays); a
// manual subscription survives the close; the reopen returns the author's
// subscription BEFORE the fan-out, so the author's reopen notification is
// carded X.
func TestOptOutAutoWatch_CloseStripsWithoutOptOutAndReopenIsCarded(t *testing.T) {
	f := newOptAutoFix(t)
	ctx := context.Background()
	actor, actorName := f.member(t)
	keeper, _ := f.member(t)
	x := f.create(t, CreateIssueInput{})
	if !f.subscribedBy(t, x.ID, f.owner, "creator") {
		t.Fatalf("precondition: the author is subscribed as creator")
	}
	if err := f.notif.AddIssueWatch(ctx, x.ID, keeper, false); err != nil {
		t.Fatalf("manual watch: %v", err)
	}
	other := HumanActor(actor, actorName)

	f.close(t, x.ID, other)
	if n := countWatcherRows(t, f.pool, x.ID, f.owner); n != 0 {
		t.Errorf("the author's automatic subscription survived the close: %v", watcherReasons(t, f.pool, x.ID, f.owner))
	}
	if f.optedOut(t, x.ID, f.owner) {
		t.Errorf("the auto-unsubscribe on close wrote an opt-out")
	}
	if !f.subscribedBy(t, x.ID, keeper, "manual") {
		t.Errorf("a manual subscription did not survive the close")
	}
	if card := f.cardOfLatest(t, x.ID, f.owner, EventIssueStatusChanged); card != x.ID {
		t.Errorf("the author's close notification card = %q, want %q (kept, not cleared)", card, x.ID)
	}

	f.reopen(t, x.ID, other)
	if !f.subscribedBy(t, x.ID, f.owner, "creator") {
		t.Errorf("reopen did not give the author the subscription back: %v", watcherReasons(t, f.pool, x.ID, f.owner))
	}
	if card := f.cardOfLatest(t, x.ID, f.owner, EventIssueStatusChanged); card != x.ID {
		t.Errorf("the author's reopen notification card = %q, want %q — the subscription came back after the fan-out",
			card, x.ID)
	}
}

// cardOfLatest is the card of u's notification of the newest evType event on
// issueID ("" when u has no row for it or the row has no card).
func (f *optAutoFix) cardOfLatest(t *testing.T, issueID string, u uuid.UUID, evType string) string {
	t.Helper()
	var card *string
	err := f.pool.QueryRow(context.Background(), `
SELECT n.card_issue_id
  FROM events e
  LEFT JOIN notifications n ON n.event_id = e.id AND n.user_id = $3
 WHERE e.entity_id = $1 AND e.type = $2
 ORDER BY e.id DESC LIMIT 1`, issueID, evType, u).Scan(&card)
	if err != nil {
		t.Fatalf("read latest %s card: %v", evType, err)
	}
	if card == nil {
		return ""
	}
	return *card
}

// TestOptOutAutoWatch_DelegationDeclineUnwatches — No. 24: declining a
// delegation deletes the subscription, records the opt-out and clears the
// decliner's card of the issue; a later assignment does not re-subscribe.
func TestOptOutAutoWatch_DelegationDeclineUnwatches(t *testing.T) {
	f := newOptAutoFix(t)
	ctx := context.Background()
	x := f.create(t, CreateIssueInput{})
	if !f.subscribedBy(t, x.ID, f.owner, "creator") {
		t.Fatalf("precondition: the author is subscribed")
	}
	var eid int64
	if err := f.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', 'human', '{}'::jsonb)
RETURNING id`, x.ID, f.pid).Scan(&eid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, reason, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, 'issue.status_changed', $3, 'delegated_status', true, $4, true)`,
		f.owner, eid, f.pid, x.ID); err != nil {
		t.Fatalf("seed card: %v", err)
	}

	if _, err := f.store.DismissDelegated(ctx, DelegationActionInput{ID: x.ID, Actor: f.ownerActor()}); err != nil {
		t.Fatalf("DismissDelegated: %v", err)
	}
	if n := countWatcherRows(t, f.pool, x.ID, f.owner); n != 0 {
		t.Errorf("subscription row left after the decline: %v", watcherReasons(t, f.pool, x.ID, f.owner))
	}
	if !f.optedOut(t, x.ID, f.owner) {
		t.Errorf("the decline recorded no opt-out")
	}
	var cards int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND card_issue_id = $2`, f.owner, x.ID).Scan(&cards); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	if cards != 0 {
		t.Errorf("the decliner still has %d notifications carded to the issue", cards)
	}
	f.assign(t, x.ID, f.ownerName, f.ownerActor())
	if n := countWatcherRows(t, f.pool, x.ID, f.owner); n != 0 {
		t.Errorf("assignment after the decline re-subscribed the author")
	}
}

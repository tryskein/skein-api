package notifications_test

// sk-622.21, T022 — the Watching badge and column read the card each
// notification was given when it was written, inside the Watching window
// (US-5, US-6, INV-5, INV-8, D1). Matrix rows No. 41 and No. 42.
//
// Every expected number is a literal derived from the US-5 definitions over
// the corpus described next to it, never read back from the code under test:
//
//   - badge = unread, non-archived notifications with a card, in the window;
//   - cards = live subscriptions ∪ cards of non-archived notifications in the
//     window; a historical card has empty reasons and no subscription time;
//   - totals: subscriptions_total = live subscriptions; open/closed_total =
//     cards of both kinds by task state; closed_auto_total = closed cards
//     without a manual subscription, historical included; unread_total =
//     the count of the unread rows of the reader's notification set in the
//     window (sk-1007 D1: rows of no project or of a project he sees now) —
//     the badge's count;
//   - order: latest notification, then id; cards without notifications last;
//   - the in-card timeline: the last 5 within 7 days (R10).
//
// Tests skip when TEST_DATABASE_URL is unset (testsupport.NewTestPool).

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// wrEnv is one project, its owner (who acts in every event) and a reader U
// whose Watching is under test.
type wrEnv struct {
	pool    *pgxpool.Pool
	store   *notifications.Store
	owner   uuid.UUID
	reader  uuid.UUID
	project uuid.UUID
	prefix  string
	n       int
}

func newWREnv(t *testing.T) *wrEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	reader, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "wread")
	testsupport.AddProjectMember(t, pool, pid, reader, "member")
	return &wrEnv{pool: pool, store: notifications.NewStore(pool), owner: owner, reader: reader,
		project: pid, prefix: prefix}
}

// issue seeds the next counter-shaped issue in the given status.
func (e *wrEnv) issue(t *testing.T, status string) string {
	t.Helper()
	e.n++
	return testsupport.SeedIssue(t, e.pool, e.project, e.owner,
		e.prefix+"-"+strconv.Itoa(e.n), "watching read", status, 2)
}

// sub writes a live subscription of the reader with the given reasons. A
// fixture, not the subject: the subscription paths have their own tests.
func (e *wrEnv) sub(t *testing.T, issueID string, scope string, reasons ...string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope) VALUES ($1, $2, $3, $4)`,
		issueID, e.reader, reasons, scope); err != nil {
		t.Fatalf("subscribe %s: %v", issueID, err)
	}
}

// notif is one notification row of the reader, as the fan-out of R2 writes
// it: card is the card the US-4 rules give it ("" = none); age is how long
// ago it was written.
type notif struct {
	issueID  string // the task the event is about
	card     string
	ntype    string
	age      time.Duration
	read     bool
	archived bool
	// project is the project of the event and of the row; zero = the env's
	// project. noProject writes the row with no project (the event keeps
	// the env's project).
	project   uuid.UUID
	noProject bool
}

// seed writes an event on n.issueID and the reader's notification row.
func (e *wrEnv) seed(t *testing.T, n notif) int64 {
	t.Helper()
	ctx := context.Background()
	ntype := n.ntype
	if ntype == "" {
		ntype = "issue.status_changed"
	}
	project := e.project
	if n.project != uuid.Nil {
		project = n.project
	}
	var rowProject *uuid.UUID
	if !n.noProject {
		rowProject = &project
	}
	var eid int64
	if err := e.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now() - $5::interval, $1, 'issue', $2, $3, 'tester', $4, 'human', '{}'::jsonb)
RETURNING id`, ntype, n.issueID, project, e.owner, n.age).Scan(&eid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var card *string
	if n.card != "" {
		card = &n.card
	}
	var id int64
	if err := e.pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel,
                           card_issue_id, card_resolved, created_at, read_at, archived_at)
VALUES ($1, $2, $3, $4, true, $5, true, now() - $6::interval,
        CASE WHEN $7 THEN now() END, CASE WHEN $8 THEN now() END)
RETURNING id`, e.reader, eid, ntype, rowProject, card, n.age, n.read, n.archived).Scan(&id); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return id
}

func (e *wrEnv) feed(t *testing.T, state notifications.WatchingState) notifications.WatchingFeedResult {
	t.Helper()
	res, err := e.store.ListWatchingFeed(context.Background(), e.reader, state)
	if err != nil {
		t.Fatalf("ListWatchingFeed(%s): %v", state, err)
	}
	return res
}

func (e *wrEnv) badge(t *testing.T) int {
	t.Helper()
	n, err := e.store.CountWatchingUnread(context.Background(), e.reader)
	if err != nil {
		t.Fatalf("CountWatchingUnread: %v", err)
	}
	return n
}

func cardIDs(rows []notifications.WatchingBase) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.IssueID)
	}
	return out
}

func cardByID(t *testing.T, rows []notifications.WatchingBase, id string) notifications.WatchingBase {
	t.Helper()
	for _, r := range rows {
		if r.IssueID == id {
			return r
		}
	}
	t.Fatalf("card %s not in the column %v", id, cardIDs(rows))
	return notifications.WatchingBase{}
}

// TestWatchingRead_CorpusDefinitions — No. 41. One corpus with every class the
// read has to tell apart; the window is the store default (21 days).
//
//	A  open,   manual         2 unread (1h, 2h), 1 read (3h), 1 unread 30d old,
//	                          1 archived unread, 1 unread mention without a card
//	P  open,   manual subtree 1 unread on P (6h), 1 unread on child P.1 carded
//	                          to P (5h), 6 read on P (4d … 4d+5m)
//	B  closed, creator        1 unread (10h)
//	H  closed, historical     1 unread (20h), 1 read (21h), 1 read 8d old
//	O  open,   historical?    1 unread 30d old only — outside the window, no card
//	C  open,   manual         silent
//	D  closed, manual         silent
//
// Badge: A 2 + P 2 + B 1 + H 1 = 6.
func TestWatchingRead_CorpusDefinitions(t *testing.T) {
	e := newWREnv(t)
	a := e.issue(t, "open")
	p := e.issue(t, "open")
	b := e.issue(t, "closed")
	h := e.issue(t, "closed")
	o := e.issue(t, "open")
	c := e.issue(t, "open")
	d := e.issue(t, "closed")
	child := testsupport.SeedIssue(t, e.pool, e.project, e.owner, p+".1", "child", "open", 2)
	testsupport.SeedDep(t, e.pool, child, p, "parent-child", e.owner)

	e.sub(t, a, "issue", "manual")
	e.sub(t, p, "subtree", "manual")
	e.sub(t, b, "issue", "creator")
	e.sub(t, c, "issue", "manual")
	e.sub(t, d, "issue", "manual")

	const day = 24 * time.Hour
	e.seed(t, notif{issueID: a, card: a, age: time.Hour})
	e.seed(t, notif{issueID: a, card: a, age: 2 * time.Hour})
	e.seed(t, notif{issueID: a, card: a, age: 3 * time.Hour, read: true})
	e.seed(t, notif{issueID: a, card: a, age: 30 * day})
	e.seed(t, notif{issueID: a, card: a, age: 30 * time.Minute, archived: true})
	e.seed(t, notif{issueID: a, ntype: "issue.mention", age: 10 * time.Minute})
	e.seed(t, notif{issueID: p, card: p, age: 6 * time.Hour})
	e.seed(t, notif{issueID: child, card: p, age: 5 * time.Hour})
	for i := 0; i < 6; i++ {
		e.seed(t, notif{issueID: p, card: p, age: 4*day + time.Duration(i)*time.Minute, read: true})
	}
	e.seed(t, notif{issueID: b, card: b, age: 10 * time.Hour})
	e.seed(t, notif{issueID: h, card: h, age: 20 * time.Hour})
	e.seed(t, notif{issueID: h, card: h, age: 21 * time.Hour, read: true})
	e.seed(t, notif{issueID: h, card: h, age: 8 * day, read: true})
	e.seed(t, notif{issueID: o, card: o, age: 30 * day})

	if got := e.badge(t); got != 6 {
		t.Errorf("badge = %d, want 6 (A 2 + P 2 + B 1 + H 1)", got)
	}

	all := e.feed(t, notifications.WatchingStateAll)
	if all.UnreadTotal != 6 {
		t.Errorf("unreadTotal = %d, want 6 — equal to the badge (INV-5)", all.UnreadTotal)
	}
	if got, want := cardIDs(all.Rows), []string{a, p, b, h, c, d}; !reflect.DeepEqual(got, want) {
		t.Errorf("cards = %v, want %v (latest notification first, silent last by id; O has nothing in the window)", got, want)
	}
	if all.SubscriptionsTotal != 5 || all.OpenTotal != 3 || all.ClosedTotal != 3 ||
		all.ClosedAutoTotal != 2 || all.StateTotal != 6 || all.Truncated {
		t.Errorf("totals = subs %d open %d closed %d closedAuto %d state %d truncated %v; "+
			"want subs 5 (A P B C D), open 3 (A P C), closed 3 (B H D), closedAuto 2 (B H), state 6, not truncated",
			all.SubscriptionsTotal, all.OpenTotal, all.ClosedTotal, all.ClosedAutoTotal, all.StateTotal, all.Truncated)
	}
	for id, want := range map[string]int{a: 2, p: 2, b: 1, h: 1, c: 0, d: 0} {
		if got := cardByID(t, all.Rows, id).UnreadEventsCount; got != want {
			t.Errorf("card %s unread = %d, want %d", id, got, want)
		}
	}
	hist := cardByID(t, all.Rows, h)
	if len(hist.Reasons) != 0 || hist.Since != nil {
		t.Errorf("historical card %s: reasons %v since %v, want empty reasons and no since", h, hist.Reasons, hist.Since)
	}
	if hist.LastActivity == nil {
		t.Errorf("historical card %s has no last activity", h)
	}
	subd := cardByID(t, all.Rows, a)
	if !reflect.DeepEqual(subd.Reasons, []string{"manual"}) || subd.Since == nil {
		t.Errorf("subscribed card %s: reasons %v since %v, want [manual] and a since", a, subd.Reasons, subd.Since)
	}
	if silent := cardByID(t, all.Rows, c); silent.LastActivity != nil {
		t.Errorf("silent subscription %s has last activity %v, want none", c, silent.LastActivity)
	}

	closed := e.feed(t, notifications.WatchingStateClosed)
	if got, want := cardIDs(closed.Rows), []string{b, h, d}; !reflect.DeepEqual(got, want) {
		t.Errorf("closed cards = %v, want %v", got, want)
	}
	if closed.StateTotal != 3 || closed.UnreadTotal != 6 || closed.Truncated {
		t.Errorf("closed: stateTotal %d unreadTotal %d truncated %v; want 3, 6 (unfiltered), false",
			closed.StateTotal, closed.UnreadTotal, closed.Truncated)
	}
	open := e.feed(t, notifications.WatchingStateOpen)
	if got, want := cardIDs(open.Rows), []string{a, p, c}; !reflect.DeepEqual(got, want) {
		t.Errorf("open cards = %v, want %v", got, want)
	}
	if open.StateTotal != 3 {
		t.Errorf("open stateTotal = %d, want 3", open.StateTotal)
	}

	// The in-card timeline: the last 5 within 7 days, read or unread (R10).
	recent, err := e.store.HydrateWatchingRecentEvents(context.Background(), e.reader, []string{a, p, h, c})
	if err != nil {
		t.Fatalf("HydrateWatchingRecentEvents: %v", err)
	}
	for id, want := range map[string]int{a: 3, p: 5, h: 2, c: 0} {
		if got := len(recent[id]); got != want {
			t.Errorf("card %s timeline = %d rows, want %d", id, got, want)
		}
	}
}

// TestWatchingRead_TruncationFollowsTheFilter — No. 41, "truncation by filter":
// with more closed cards than the cap, the closed filter is truncated and
// the open one is not.
func TestWatchingRead_TruncationFollowsTheFilter(t *testing.T) {
	e := newWREnv(t)
	open := e.issue(t, "open")
	e.sub(t, open, "issue", "manual")
	for i := 0; i < notifications.WatchingFeedLimit+1; i++ {
		e.sub(t, e.issue(t, "closed"), "issue", "creator")
	}

	closed := e.feed(t, notifications.WatchingStateClosed)
	if len(closed.Rows) != notifications.WatchingFeedLimit || closed.StateTotal != notifications.WatchingFeedLimit+1 || !closed.Truncated {
		t.Errorf("closed: %d rows, stateTotal %d, truncated %v; want %d, %d, true",
			len(closed.Rows), closed.StateTotal, closed.Truncated,
			notifications.WatchingFeedLimit, notifications.WatchingFeedLimit+1)
	}
	op := e.feed(t, notifications.WatchingStateOpen)
	if len(op.Rows) != 1 || op.StateTotal != 1 || op.Truncated {
		t.Errorf("open: %d rows, stateTotal %d, truncated %v; want 1, 1, false", len(op.Rows), op.StateTotal, op.Truncated)
	}
}

// TestWatchingRead_SubscriptionShowsOnlyLaterNotifications — No. 42, row
// "subscribed": a notification written before the subscription (by another
// reason, so without a card) does not join the card when the reader
// subscribes; one written after it does.
func TestWatchingRead_SubscriptionShowsOnlyLaterNotifications(t *testing.T) {
	e := newCardEnv(t)
	reader := e.member(t)
	task := e.issue(t)
	ctx := context.Background()

	// Before: the reader is notified about the task without watching it
	// (another reason), so the row was written without a card.
	if _, err := e.pool.Exec(ctx, `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    VALUES (now(), 'issue.status_changed', 'issue', $3, $2, 'tester', $4, 'human', '{}'::jsonb)
    RETURNING id
)
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_resolved)
SELECT $1, ev.id, 'issue.status_changed', $2, true, true FROM ev`,
		reader, e.project, task, e.owner); err != nil {
		t.Fatalf("seed earlier notification: %v", err)
	}
	e.watch(t, task, reader, false)

	store := notifications.NewStore(e.pool)
	if n, err := store.CountWatchingUnread(ctx, reader); err != nil || n != 0 {
		t.Fatalf("badge after subscribing = %d, %v; want 0 — the earlier notification stays off the card", n, err)
	}
	res, err := store.ListWatchingFeed(ctx, reader, notifications.WatchingStateAll)
	if err != nil {
		t.Fatalf("ListWatchingFeed: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].IssueID != task || res.Rows[0].UnreadEventsCount != 0 {
		t.Fatalf("column after subscribing = %+v, want one silent card %s", res.Rows, task)
	}

	statusEvent(t, e.pool, task, e.project, e.owner, notifications.FanOutInput{})
	if n, err := store.CountWatchingUnread(ctx, reader); err != nil || n != 1 {
		t.Errorf("badge after a later event = %d, %v; want 1", n, err)
	}
	res, err = store.ListWatchingFeed(ctx, reader, notifications.WatchingStateAll)
	if err != nil {
		t.Fatalf("ListWatchingFeed: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].UnreadEventsCount != 1 || res.UnreadTotal != 1 {
		t.Errorf("column after a later event = %+v (unreadTotal %d), want the card with 1 unread", res.Rows, res.UnreadTotal)
	}
}

// TestWatchingRead_ExplicitUnwatchLeavesNoCard — No. 42, row "unsubscribed": the
// explicit unwatch of a subscribed card takes the card out of the column and
// the badge; it does not turn into a historical card.
func TestWatchingRead_ExplicitUnwatchLeavesNoCard(t *testing.T) {
	e := newCardEnv(t)
	reader := e.member(t)
	task := e.issue(t)
	ctx := context.Background()
	e.watch(t, task, reader, false)
	statusEvent(t, e.pool, task, e.project, e.owner, notifications.FanOutInput{})

	store := notifications.NewStore(e.pool)
	if n, _ := store.CountWatchingUnread(ctx, reader); n != 1 {
		t.Fatalf("precondition: badge = %d, want 1", n)
	}
	if err := store.RemoveIssueWatch(ctx, task, reader); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}
	if n, err := store.CountWatchingUnread(ctx, reader); err != nil || n != 0 {
		t.Errorf("badge after unwatch = %d, %v; want 0", n, err)
	}
	res, err := store.ListWatchingFeed(ctx, reader, notifications.WatchingStateAll)
	if err != nil {
		t.Fatalf("ListWatchingFeed: %v", err)
	}
	if len(res.Rows) != 0 || res.ClosedTotal+res.OpenTotal != 0 {
		t.Errorf("column after unwatch = %v, want no card", cardIDs(res.Rows))
	}
}

// closedHistorical builds the US-6 "closed" situation through the product:
// the owner creates a task assigned to the reader (an automatic `assignee`
// subscription), then closes it. The reader is notified of the close on the
// card, and the automatic unwatch on close (on by default, US-2) drops the
// subscription — the card becomes historical.
func closedHistorical(t *testing.T) (pool *pgxpool.Pool, reader uuid.UUID, task string) {
	t.Helper()
	pool = testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	reader, readerName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "wrhist")
	testsupport.AddProjectMember(t, pool, pid, reader, "member")
	store := issues.NewStore(pool)
	act := issues.HumanActor(owner, ownerName)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		issues.CreateIssueInput{Title: "historical", Type: "task", Priority: 2, Assignee: &readerName},
		owner, act)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := store.Close(t.Context(), issues.CloseInput{
		ID: dto.ID, ExpectedVersion: dto.Version, Reason: "done", Actor: act,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return pool, reader, dto.ID
}

// optOuts counts the reader's opt-outs on the task.
func optOuts(t *testing.T, pool *pgxpool.Pool, task string, reader uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, task, reader).Scan(&n); err != nil {
		t.Fatalf("count opt-outs: %v", err)
	}
	return n
}

// TestWatchingRead_HistoricalCardLifecycle — No. 42, rows "closed", "remove",
// "unsubscribe from closed": after the automatic unwatch on close the closing
// notification keeps the card visible as a historical one (empty reasons,
// no since, counted in the badge); removing it — by selection or by the
// closed sweep — takes it away for good, across re-reads, with no opt-out.
func TestWatchingRead_HistoricalCardLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   func(task string) notifications.BulkUnwatchInput
	}{
		{"selection", func(task string) notifications.BulkUnwatchInput {
			return notifications.BulkUnwatchInput{IssueIDs: []string{task}}
		}},
		{"closed sweep", func(string) notifications.BulkUnwatchInput {
			return notifications.BulkUnwatchInput{State: notifications.WatchingStateClosed}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, reader, task := closedHistorical(t)
			ctx := context.Background()
			store := notifications.NewStore(pool)

			var subs int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
				task, reader).Scan(&subs); err != nil {
				t.Fatalf("count subscriptions: %v", err)
			}
			if subs != 0 {
				t.Fatalf("precondition: the automatic subscription survived the close (%d rows)", subs)
			}
			res, err := store.ListWatchingFeed(ctx, reader, notifications.WatchingStateClosed)
			if err != nil {
				t.Fatalf("ListWatchingFeed: %v", err)
			}
			if len(res.Rows) != 1 || res.Rows[0].IssueID != task {
				t.Fatalf("after close: closed cards = %v, want the historical card %s", cardIDs(res.Rows), task)
			}
			card := res.Rows[0]
			if len(card.Reasons) != 0 || card.Since != nil || card.UnreadEventsCount < 1 {
				t.Errorf("historical card = %+v, want empty reasons, no since, the closing notification unread", card)
			}
			if res.SubscriptionsTotal != 0 || res.ClosedAutoTotal != 1 {
				t.Errorf("after close: subscriptionsTotal %d closedAutoTotal %d, want 0 and 1", res.SubscriptionsTotal, res.ClosedAutoTotal)
			}
			badge, err := store.CountWatchingUnread(ctx, reader)
			if err != nil {
				t.Fatalf("CountWatchingUnread: %v", err)
			}
			if badge != card.UnreadEventsCount || badge != res.UnreadTotal {
				t.Errorf("badge %d, card unread %d, unreadTotal %d — must be equal", badge, card.UnreadEventsCount, res.UnreadTotal)
			}

			out, err := store.BulkUnwatch(ctx, reader, tc.in(task))
			if err != nil {
				t.Fatalf("BulkUnwatch: %v", err)
			}
			if out.HistoricalRemoved != 1 || out.Unwatched != 0 {
				t.Errorf("BulkUnwatch = %+v, want historicalRemoved 1, unwatched 0", out)
			}
			for i := 0; i < 2; i++ { // the card does not come back on a re-read
				res, err = store.ListWatchingFeed(ctx, reader, notifications.WatchingStateAll)
				if err != nil {
					t.Fatalf("ListWatchingFeed: %v", err)
				}
				if len(res.Rows) != 0 || res.UnreadTotal != 0 {
					t.Errorf("read %d after removal: cards %v unreadTotal %d, want none", i+1, cardIDs(res.Rows), res.UnreadTotal)
				}
				if n, _ := store.CountWatchingUnread(ctx, reader); n != 0 {
					t.Errorf("read %d after removal: badge %d, want 0", i+1, n)
				}
			}
			if n := optOuts(t, pool, task, reader); n != 0 {
				t.Errorf("opt-outs after removing a historical card = %d, want 0 (R3)", n)
			}
		})
	}
}

// ----------------- sk-1007 US-1: the column over the notification set -----------------
//
// sk-1007 D1 / K3: every number of the column, the timeline and whether a
// historical card exists come from the reader's notification set — his rows
// of no project or of a project he sees now. The expected numbers below are
// literals of the corpus each test describes (spec US-1, INV-3).

// wrProject is a second project of a wrEnv: the owner's, the reader a member
// (visibility members, so removing the membership hides it).
type wrProject struct {
	id     uuid.UUID
	prefix string
	n      int
}

func (e *wrEnv) otherProject(t *testing.T, name string) *wrProject {
	t.Helper()
	id, prefix := testsupport.SeedProject(t, e.pool, e.owner, name)
	testsupport.AddProjectMember(t, e.pool, id, e.reader, "member")
	return &wrProject{id: id, prefix: prefix}
}

func (e *wrEnv) issueIn(t *testing.T, q *wrProject, status string) string {
	t.Helper()
	q.n++
	return testsupport.SeedIssue(t, e.pool, q.id, e.owner, q.prefix+"-"+strconv.Itoa(q.n), "watching read", status, 2)
}

// loseAccess removes the reader from q: q is a project he does not see now.
func (e *wrEnv) loseAccess(t *testing.T, q *wrProject) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, q.id, e.reader); err != nil {
		t.Fatalf("remove the reader from %s: %v", q.prefix, err)
	}
}

func (e *wrEnv) createdAt(t *testing.T, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT created_at FROM notifications WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read created_at of %d: %v", id, err)
	}
	return at
}

// timeline returns the notification ids of the card's in-card timeline, in
// the order it gives them.
func (e *wrEnv) timeline(t *testing.T, card string) []int64 {
	t.Helper()
	recent, err := e.store.HydrateWatchingRecentEvents(context.Background(), e.reader, []string{card})
	if err != nil {
		t.Fatalf("HydrateWatchingRecentEvents: %v", err)
	}
	out := []int64{}
	for _, n := range recent[card] {
		out = append(out, n.ID)
	}
	return out
}

// TestWatchingRead_WindowIsTheNotificationSet — sk-1007 No. 6, US-1
// "Current". A (project P) is watched with its subtree; its descendant B
// lies in Q and the fan-out carded B's events to A, one of them newer than
// every row of P. While the reader sees Q its rows count; after he is
// removed from Q, A's unread count, UnreadTotal, A's last activity and A's
// timeline hold the rows of P only.
//
//	P on A: unread 2h, read 3h        Q (B) on A: unread 1h, unread 4h
func TestWatchingRead_WindowIsTheNotificationSet(t *testing.T) {
	e := newWREnv(t)
	q := e.otherProject(t, "wrq")
	a := e.issue(t, "open")
	b := e.issueIn(t, q, "open")
	testsupport.SeedDep(t, e.pool, b, a, "parent-child", e.owner)
	e.sub(t, a, "subtree", "manual")

	pUnread := e.seed(t, notif{issueID: a, card: a, age: 2 * time.Hour})
	pRead := e.seed(t, notif{issueID: a, card: a, age: 3 * time.Hour, read: true})
	qNewest := e.seed(t, notif{issueID: b, card: a, project: q.id, age: time.Hour})
	qOlder := e.seed(t, notif{issueID: b, card: a, project: q.id, age: 4 * time.Hour})

	// Positive control: Q is visible, its rows are on the card.
	before := e.feed(t, notifications.WatchingStateAll)
	card := cardByID(t, before.Rows, a)
	if card.UnreadEventsCount != 3 || before.UnreadTotal != 3 {
		t.Fatalf("with access to Q: card unread %d, unreadTotal %d; want 3 and 3 (P 1 + Q 2) — the fixture proves nothing",
			card.UnreadEventsCount, before.UnreadTotal)
	}
	if card.LastActivity == nil || !card.LastActivity.Equal(e.createdAt(t, qNewest)) {
		t.Fatalf("with access to Q: last activity %v, want the newest row of Q", card.LastActivity)
	}
	if got, want := e.timeline(t, a), []int64{qNewest, pUnread, pRead, qOlder}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with access to Q: timeline %v, want %v", got, want)
	}

	e.loseAccess(t, q)

	after := e.feed(t, notifications.WatchingStateAll)
	card = cardByID(t, after.Rows, a)
	if card.UnreadEventsCount != 1 {
		t.Errorf("card %s unread = %d, want 1 (the unread row of P only)", a, card.UnreadEventsCount)
	}
	if after.UnreadTotal != 1 {
		t.Errorf("unreadTotal = %d, want 1 (the unread row of P only)", after.UnreadTotal)
	}
	if card.LastActivity == nil || !card.LastActivity.Equal(e.createdAt(t, pUnread)) {
		t.Errorf("card %s last activity = %v, want the time of the latest row of P (%v)",
			a, card.LastActivity, e.createdAt(t, pUnread))
	}
	if got, want := e.timeline(t, a), []int64{pUnread, pRead}; !reflect.DeepEqual(got, want) {
		t.Errorf("card %s timeline = %v, want %v (no row of Q)", a, got, want)
	}
}

// TestWatchingRead_HistoricalCardNeedsARowOfTheSet — sk-1007 No. 7, US-1
// "existence of a historical card". P-5 is a closed task of P the reader
// sees, without a subscription; the only rows carded to it are of Q. While
// Q is visible P-5 is a historical card; after the reader is removed from Q
// there is no card and no total counts one.
func TestWatchingRead_HistoricalCardNeedsARowOfTheSet(t *testing.T) {
	e := newWREnv(t)
	q := e.otherProject(t, "wrq")
	p5 := e.issue(t, "closed")
	qi := e.issueIn(t, q, "open")
	e.seed(t, notif{issueID: qi, card: p5, project: q.id, age: time.Hour})

	before := e.feed(t, notifications.WatchingStateAll)
	if got := cardIDs(before.Rows); !reflect.DeepEqual(got, []string{p5}) ||
		before.ClosedTotal != 1 || before.ClosedAutoTotal != 1 || before.StateTotal != 1 || before.OpenTotal != 0 {
		t.Fatalf("with access to Q: cards %v, closed %d closedAuto %d state %d open %d; want [%s], 1, 1, 1, 0 — the fixture proves nothing",
			got, before.ClosedTotal, before.ClosedAutoTotal, before.StateTotal, before.OpenTotal, p5)
	}

	e.loseAccess(t, q)

	for _, state := range []notifications.WatchingState{notifications.WatchingStateAll, notifications.WatchingStateClosed} {
		after := e.feed(t, state)
		if len(after.Rows) != 0 {
			t.Errorf("%s: cards %v, want none — no row of the set carries P-5", state, cardIDs(after.Rows))
		}
		if after.StateTotal != 0 || after.OpenTotal != 0 || after.ClosedTotal != 0 ||
			after.ClosedAutoTotal != 0 || after.UnreadTotal != 0 {
			t.Errorf("%s: state %d open %d closed %d closedAuto %d unread %d; want all 0",
				state, after.StateTotal, after.OpenTotal, after.ClosedTotal, after.ClosedAutoTotal, after.UnreadTotal)
		}
	}
}

// TestWatchingRead_UnreadTotalCountsRowsOfAHiddenCard — sk-1007 No. 8,
// INV-3, SC-001, K3 "ancestor in a project the reader cannot see". H-1
// (project H) is watched with its subtree; an event of its descendant P-9
// (project P, visible) is carded to H-1. After the reader is removed from
// H there is no card H-1 and no subscription is counted, but the row of P
// is in the reader's set: UnreadTotal counts it and equals the badge.
func TestWatchingRead_UnreadTotalCountsRowsOfAHiddenCard(t *testing.T) {
	e := newWREnv(t)
	h := e.otherProject(t, "wrh")
	h1 := e.issueIn(t, h, "open")
	p9 := e.issue(t, "open")
	testsupport.SeedDep(t, e.pool, p9, h1, "parent-child", e.owner)
	e.sub(t, h1, "subtree", "manual")
	e.seed(t, notif{issueID: p9, card: h1, age: time.Hour})

	before := e.feed(t, notifications.WatchingStateAll)
	if got := cardIDs(before.Rows); !reflect.DeepEqual(got, []string{h1}) || before.SubscriptionsTotal != 1 || before.UnreadTotal != 1 {
		t.Fatalf("with access to H: cards %v, subscriptions %d, unreadTotal %d; want [%s], 1, 1 — the fixture proves nothing",
			got, before.SubscriptionsTotal, before.UnreadTotal, h1)
	}

	e.loseAccess(t, h)

	after := e.feed(t, notifications.WatchingStateAll)
	if len(after.Rows) != 0 {
		t.Errorf("cards = %v, want none — H-1 is a task the reader does not see", cardIDs(after.Rows))
	}
	if after.SubscriptionsTotal != 0 || after.OpenTotal != 0 || after.StateTotal != 0 {
		t.Errorf("subscriptions %d open %d state %d, want 0 — the card of H-1 is not shown",
			after.SubscriptionsTotal, after.OpenTotal, after.StateTotal)
	}
	if after.UnreadTotal != 1 {
		t.Errorf("unreadTotal = %d, want 1 — the unread row of P-9 is in the reader's set", after.UnreadTotal)
	}
	if badge := e.badge(t); after.UnreadTotal != badge {
		t.Errorf("unreadTotal %d != badge %d (INV-3)", after.UnreadTotal, badge)
	}
}

// TestWatchingRead_RowWithoutProjectCounts — sk-1007 No. 9, US-1 "a row of
// no project" (Unchanged): on a visible card, an unread row of no project
// counts in the card, in UnreadTotal and in the badge, beside a row of P.
func TestWatchingRead_RowWithoutProjectCounts(t *testing.T) {
	e := newWREnv(t)
	a := e.issue(t, "open")
	e.sub(t, a, "issue", "manual")
	e.seed(t, notif{issueID: a, card: a, age: time.Hour, noProject: true})
	e.seed(t, notif{issueID: a, card: a, age: 2 * time.Hour})

	res := e.feed(t, notifications.WatchingStateAll)
	if got := cardByID(t, res.Rows, a).UnreadEventsCount; got != 2 {
		t.Errorf("card %s unread = %d, want 2 (the row of no project and the row of P)", a, got)
	}
	if res.UnreadTotal != 2 {
		t.Errorf("unreadTotal = %d, want 2", res.UnreadTotal)
	}
	if badge := e.badge(t); badge != 2 {
		t.Errorf("badge = %d, want 2", badge)
	}
}

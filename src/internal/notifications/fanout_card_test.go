package notifications_test

// sk-622.21, T012 — the Watching card computed at write time (US-4, INV-4),
// the subtree by the canonical hierarchy (US-3, INV-3) and a fan-out failure
// that does not take the mutation with it (edge 8). Matrix rows No. 12, No. 13, No. 16.
//
// External test package on purpose: the direct writers that must NOT give a
// card (reminders, digests) and the real edge/reparent path (dependencies)
// all import notifications, so an in-package test could not reach them.
//
// Every expected card is a literal taken from the US-4 rules — exact
// subscription → the event's issue; else the NEAREST ancestor with a subtree
// subscription; else none — never read back from the code under test.
//
// Tests skip when TEST_DATABASE_URL is unset (testsupport.NewTestPool).

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/notifications"
	"skein-api/internal/nudges"
	"skein-api/internal/reminders"
	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// cardEnv is one project with its owner, who acts in every event (so the
// self-action exclusion never touches a recipient under test).
type cardEnv struct {
	pool      *pgxpool.Pool
	owner     uuid.UUID
	ownerName string
	project   uuid.UUID
	prefix    string
	n         int
	notif     *notifications.Store
	deps      *dependencies.Store
}

func newCardEnv(t *testing.T) *cardEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "fcard")
	return &cardEnv{
		pool: pool, owner: owner, ownerName: ownerName,
		project: pid, prefix: prefix,
		notif: notifications.NewStore(pool),
		deps:  dependencies.NewStore(pool),
	}
}

// issue seeds the next counter-shaped issue of the project.
func (e *cardEnv) issue(t *testing.T) string {
	t.Helper()
	e.n++
	return testsupport.SeedIssue(t, e.pool, e.project, e.owner,
		e.prefix+"-"+strconv.Itoa(e.n), "fanout card", "open", 2)
}

// member seeds a user who belongs to the env's project.
func (e *cardEnv) member(t *testing.T) uuid.UUID {
	t.Helper()
	u, _ := testsupport.SeedUser(t, e.pool, "user")
	testsupport.AddProjectMember(t, e.pool, e.project, u, "member")
	return u
}

// link makes child a child of parent through the product's edge path
// (dependencies.Store#Create), so the canonical-parent rule is the real one.
func link(t *testing.T, deps *dependencies.Store, child, parent string, actor uuid.UUID, actorName string) {
	t.Helper()
	if _, err := deps.Create(context.Background(), child,
		dependencies.CreateDepInput{ToID: parent, Type: "parent-child"},
		dependencies.CallerAccess{UserID: actor, Role: "admin"},
		dependencies.HumanActor(actor, actorName)); err != nil {
		t.Fatalf("link %s → %s: %v", child, parent, err)
	}
}

// watch is the public manual subscription (issue scope or subtree).
func (e *cardEnv) watch(t *testing.T, issueID string, u uuid.UUID, subtree bool) {
	t.Helper()
	if err := e.notif.AddIssueWatch(context.Background(), issueID, u, subtree); err != nil {
		t.Fatalf("watch %s (subtree=%v): %v", issueID, subtree, err)
	}
}

// insertEvent writes one events row inside tx and returns its id.
func insertEvent(t *testing.T, tx pgx.Tx, evtType, entityType, entityID string, projectID, actor uuid.UUID) int64 {
	t.Helper()
	var id int64
	if err := tx.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, $2, $3, $4, 'tester', $5, 'human', '{}'::jsonb)
RETURNING id`, evtType, entityType, entityID, projectID, actor).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

// statusEvent records an issue.status_changed on issueID in projectID and
// fans it out through the product's FanOut, in one committed transaction.
// in carries the role context; its event fields are filled here.
func statusEvent(t *testing.T, pool *pgxpool.Pool, issueID string, projectID, actor uuid.UUID,
	in notifications.FanOutInput) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	eid := insertEvent(t, tx, "issue.status_changed", "issue", issueID, projectID, actor)
	in.EventID = eid
	in.EventType = "issue.status_changed"
	in.EntityType = "issue"
	in.IssueID = issueID
	in.ProjectID = projectID
	in.ActorUserID = actor
	in.ActorType = "human"
	if _, err := notifications.FanOut(ctx, tx, in); err != nil {
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return eid
}

// notifRow is what the tests read back about one recipient's row.
type notifRow struct {
	exists   bool
	card     *string
	resolved bool
}

func rowFor(t *testing.T, pool *pgxpool.Pool, u uuid.UUID, eid int64) notifRow {
	t.Helper()
	var r notifRow
	err := pool.QueryRow(context.Background(), `
SELECT card_issue_id, card_resolved FROM notifications WHERE user_id = $1 AND event_id = $2`,
		u, eid).Scan(&r.card, &r.resolved)
	if err == pgx.ErrNoRows {
		return r
	}
	if err != nil {
		t.Fatalf("read notification of %s for event %d: %v", u, eid, err)
	}
	r.exists = true
	return r
}

func cardString(c *string) string {
	if c == nil {
		return "<none>"
	}
	return *c
}

func setReasonChannels(t *testing.T, pool *pgxpool.Pool, u uuid.UUID, key notifications.ReasonKey, feed, push bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, reason) DO UPDATE
   SET feed_enabled = EXCLUDED.feed_enabled, push_enabled = EXCLUDED.push_enabled`,
		u, string(key), feed, push); err != nil {
		t.Fatalf("set reason %s for %s: %v", key, u, err)
	}
}

// TestFanOutCard_PerRecipientRules — No. 12: the three US-4 rules, exact beats
// an ancestor, the nearest of two ancestors wins, and recipients that came
// by role, by a project subscription or by a filter get no card. All in ONE
// fan-out of ONE event, because the card is per recipient: a card computed
// once per event would give every row the same value and fail here.
//
// Hierarchy: T is a child of B, B is a child of A.
func TestFanOutCard_PerRecipientRules(t *testing.T) {
	e := newCardEnv(t)
	ctx := context.Background()

	a, b, tk := e.issue(t), e.issue(t), e.issue(t)
	link(t, e.deps, b, a, e.owner, e.ownerName)
	link(t, e.deps, tk, b, e.owner, e.ownerName)

	exact := e.member(t)     // issue-scope subscription on T
	exactAndA := e.member(t) // on T AND subtree on A — exact wins
	nearB := e.member(t)     // subtree on A AND on B — B is nearer
	farA := e.member(t)      // subtree on A only
	role := e.member(t)      // the creator in the role context, no subscription
	project := e.member(t)   // project watcher
	filter := e.member(t)    // custom (filter) watcher on the project

	e.watch(t, tk, exact, false)
	e.watch(t, tk, exactAndA, false)
	e.watch(t, a, exactAndA, true)
	e.watch(t, a, nearB, true)
	e.watch(t, b, nearB, true)
	e.watch(t, a, farA, true)
	if err := e.notif.AddProjectWatch(ctx, e.project, project); err != nil {
		t.Fatalf("project watch: %v", err)
	}
	if _, err := e.notif.AddCustomWatcher(ctx, filter, notifications.CreateCustomWatcherInput{
		Name:   "fanout card filter",
		Filter: notifications.CustomWatcherFilter{Projects: []string{e.project.String()}},
	}); err != nil {
		t.Fatalf("custom watcher: %v", err)
	}
	// The role recipient's row must exist for its "no card" to mean
	// anything: the delegated-status push is switched on explicitly.
	setReasonChannels(t, e.pool, role, notifications.ReasonKeyDelegatedStatus, true, true)

	eid := statusEvent(t, e.pool, tk, e.project, e.owner, notifications.FanOutInput{
		CreatorUserID:  role,
		AssigneeUserID: exact,
	})

	cases := []struct {
		name string
		user uuid.UUID
		want *string
	}{
		{"exact subscription → the event's issue", exact, &tk},
		{"exact beats a subtree ancestor", exactAndA, &tk},
		{"two subtree ancestors → the nearest", nearB, &b},
		{"one subtree ancestor → that ancestor", farA, &a},
		{"role recipient → no card", role, nil},
		{"project watcher → no card", project, nil},
		{"filter watcher → no card", filter, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := rowFor(t, e.pool, c.user, eid)
			if !r.exists {
				t.Fatalf("no notification row — the case proves nothing without one")
			}
			if cardString(r.card) != cardString(c.want) {
				t.Errorf("card = %s, want %s", cardString(r.card), cardString(c.want))
			}
			if !r.resolved {
				t.Errorf("card_resolved = false on a row this version wrote")
			}
		})
	}
}

// TestFanOutCard_DirectWritersGiveNoCard — No. 12: notifications not written by
// the fan-out — a mention, a reminder, a digest — never get a card, even for
// a recipient subscribed to the very issue, and are marked resolved.
func TestFanOutCard_DirectWritersGiveNoCard(t *testing.T) {
	e := newCardEnv(t)
	ctx := context.Background()
	tk := e.issue(t)
	u := e.member(t)
	e.watch(t, tk, u, false)

	t.Run("mention", func(t *testing.T) {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		eid := insertEvent(t, tx, "issue.mention", "issue", tk, e.project, e.owner)
		if err := notifications.WriteMentionNotification(ctx, tx, eid, u, "issue.mention", e.project); err != nil {
			t.Fatalf("WriteMentionNotification: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		r := rowFor(t, e.pool, u, eid)
		if !r.exists {
			t.Fatalf("no mention row")
		}
		if r.card != nil || !r.resolved {
			t.Errorf("mention: card = %s, resolved = %v; want <none>, true", cardString(r.card), r.resolved)
		}
	})

	t.Run("reminder", func(t *testing.T) {
		setReasonChannels(t, e.pool, u, notifications.ReasonKeyReminder, false, true)
		rid := testsupport.SeedReminder(t, e.pool, tk, u)
		if _, err := e.pool.Exec(ctx,
			`UPDATE reminders SET remind_at = now() - interval '1 minute' WHERE id = $1`, rid); err != nil {
			t.Fatalf("make reminder due: %v", err)
		}
		if _, err := reminders.NewStore(e.pool).ClaimAndNotifyDue(ctx, 1000,
			reminders.Actor{Username: "system", Role: "system"}); err != nil {
			t.Fatalf("ClaimAndNotifyDue: %v", err)
		}
		assertOnlyCardless(t, e.pool, u, string(notifications.ReasonKeyReminder))
	})

	t.Run("digest", func(t *testing.T) {
		res, err := nudges.NewStore(e.pool).Form(ctx, review.CategoryUnaccepted,
			nudges.Candidate{UserID: u, IssueIDs: []string{tk}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("Form: %v", err)
		}
		if !res.Formed {
			t.Fatalf("digest not formed — the case proves nothing without a row")
		}
		assertOnlyCardless(t, e.pool, u, res.Reason)
	})
}

// assertOnlyCardless asserts u has at least one row of reason and none of
// them carries a card or is unresolved.
func assertOnlyCardless(t *testing.T, pool *pgxpool.Pool, u uuid.UUID, reason string) {
	t.Helper()
	var total, carded, unresolved int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*),
       count(*) FILTER (WHERE card_issue_id IS NOT NULL),
       count(*) FILTER (WHERE NOT card_resolved)
  FROM notifications WHERE user_id = $1 AND reason = $2`, u, reason).
		Scan(&total, &carded, &unresolved); err != nil {
		t.Fatalf("read %s rows: %v", reason, err)
	}
	if total == 0 {
		t.Fatalf("no %s row — the case proves nothing without one", reason)
	}
	if carded != 0 || unresolved != 0 {
		t.Errorf("%s rows: %d with a card, %d unresolved; want 0 and 0", reason, carded, unresolved)
	}
}

// TestFanOutCard_ReparentMovesOnlyNewEvents — No. 13, US-6 "move": T moves
// from under A to under B. A's subtree subscriber gets no NEW events of T,
// B's gets them with card B, and the row written before the move keeps
// card A.
func TestFanOutCard_ReparentMovesOnlyNewEvents(t *testing.T) {
	e := newCardEnv(t)
	ctx := context.Background()
	a, b, tk := e.issue(t), e.issue(t), e.issue(t)
	link(t, e.deps, tk, a, e.owner, e.ownerName)

	subA, subB := e.member(t), e.member(t)
	e.watch(t, a, subA, true)
	e.watch(t, b, subB, true)

	before := statusEvent(t, e.pool, tk, e.project, e.owner, notifications.FanOutInput{})
	if r := rowFor(t, e.pool, subA, before); !r.exists || cardString(r.card) != a {
		t.Fatalf("before the move: A's subscriber row exists=%v card=%s, want a row with card %s",
			r.exists, cardString(r.card), a)
	}
	if r := rowFor(t, e.pool, subB, before); r.exists {
		t.Fatalf("before the move: B's subscriber got an event of a task not under B")
	}

	var version int
	if err := e.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, tk).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, err := e.deps.Reparent(ctx, tk, b, version,
		dependencies.CallerAccess{UserID: e.owner, Role: "admin"},
		dependencies.HumanActor(e.owner, e.ownerName)); err != nil {
		t.Fatalf("Reparent: %v", err)
	}

	after := statusEvent(t, e.pool, tk, e.project, e.owner, notifications.FanOutInput{})
	if r := rowFor(t, e.pool, subA, after); r.exists {
		t.Errorf("after the move: A's subtree subscriber still got a new event of T (card %s)", cardString(r.card))
	}
	if r := rowFor(t, e.pool, subB, after); !r.exists || cardString(r.card) != b {
		t.Errorf("after the move: B's subscriber row exists=%v card=%s, want a row with card %s",
			r.exists, cardString(r.card), b)
	}
	if r := rowFor(t, e.pool, subA, before); !r.exists || cardString(r.card) != a {
		t.Errorf("the row written before the move: exists=%v card=%s, want card %s kept",
			r.exists, cardString(r.card), a)
	}
}

// TestFanOutCard_IdPrefixWithoutEdgeIsNotADescendant — No. 13: an issue whose id
// starts with A's id and a dot but has no parent-child edge to A is not in
// A's subtree.
func TestFanOutCard_IdPrefixWithoutEdgeIsNotADescendant(t *testing.T) {
	e := newCardEnv(t)
	a := e.issue(t)
	lookalike := testsupport.SeedIssue(t, e.pool, e.project, e.owner, a+".1", "prefix only", "open", 2)

	subA := e.member(t)
	e.watch(t, a, subA, true)

	eid := statusEvent(t, e.pool, lookalike, e.project, e.owner, notifications.FanOutInput{})
	if r := rowFor(t, e.pool, subA, eid); r.exists {
		t.Errorf("subtree subscriber of %s got an event of %s, which has no edge to it (card %s)",
			a, lookalike, cardString(r.card))
	}
}

// TestFanOutCard_CrossProjectChainChecksTheEventProject — No. 13: the chain may
// cross projects; the recipient's right is decided by the EVENT's project.
// A is in P1, T (its child) in P2; both subscribers watch A with subtree,
// only one of them is a member of P2.
func TestFanOutCard_CrossProjectChainChecksTheEventProject(t *testing.T) {
	e := newCardEnv(t)
	a := e.issue(t)
	p2, prefix2 := testsupport.SeedProject(t, e.pool, e.owner, "fcardx")
	tk := testsupport.SeedIssue(t, e.pool, p2, e.owner, prefix2+"-1", "cross project child", "open", 2)
	link(t, e.deps, tk, a, e.owner, e.ownerName)

	inBoth := e.member(t)
	testsupport.AddProjectMember(t, e.pool, p2, inBoth, "member")
	onlyP1 := e.member(t)
	e.watch(t, a, inBoth, true)
	e.watch(t, a, onlyP1, true)

	eid := statusEvent(t, e.pool, tk, p2, e.owner, notifications.FanOutInput{})
	if r := rowFor(t, e.pool, inBoth, eid); !r.exists || cardString(r.card) != a {
		t.Errorf("member of the event's project: row exists=%v card=%s, want card %s",
			r.exists, cardString(r.card), a)
	}
	if r := rowFor(t, e.pool, onlyP1, eid); r.exists {
		t.Errorf("non-member of the event's project got the event (card %s)", cardString(r.card))
	}
}

// TestFanOutCard_FanOutFailureKeepsTheMutation — No. 16, edge 8: the card is
// part of the fan-out, and a fan-out that cannot finish leaves no rows while
// the mutation and its event commit and no error reaches the caller.
//
// The failure is a preferences read that cannot run (a temporary table
// shadows user_notification_reason_prefs in this transaction) — the fan-out
// failure the product isolates behind its savepoint. The row-16 variant with
// a lock_timeout on the INSERT is not usable: a failed INSERT is, by the
// fan-out's contract, an error the parent must see
// (fanout_matrix_test.go#TestFanOutMatrix_OtherDBErrorsStayErrors), and the
// spec's edge 8 asks for "no worse than today", not for swallowing it.
// The recipient has an exact subscription, so the card computation really
// runs inside the failing fan-out.
func TestFanOutCard_FanOutFailureKeepsTheMutation(t *testing.T) {
	e := newCardEnv(t)
	ctx := context.Background()
	tk := e.issue(t)
	u := e.member(t)
	e.watch(t, tk, u, false)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE issues SET title = 'mutation survived' WHERE id = $1`, tk); err != nil {
		t.Fatalf("mutation: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`CREATE TEMPORARY TABLE user_notification_reason_prefs (shadow int) ON COMMIT DROP`); err != nil {
		t.Fatalf("shadow prefs: %v", err)
	}
	eid := insertEvent(t, tx, "issue.status_changed", "issue", tk, e.project, e.owner)
	if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
		EventID: eid, EventType: "issue.status_changed", EntityType: "issue",
		IssueID: tk, ProjectID: e.project, ActorUserID: e.owner, ActorType: "human",
	}); err != nil {
		t.Fatalf("a failed fan-out surfaced to the caller: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the mutation's transaction did not commit: %v", err)
	}

	var title string
	var events, rows int
	if err := e.pool.QueryRow(ctx, `
SELECT (SELECT title FROM issues WHERE id = $1),
       (SELECT count(*) FROM events WHERE id = $2),
       (SELECT count(*) FROM notifications WHERE event_id = $2)`, tk, eid).
		Scan(&title, &events, &rows); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if title != "mutation survived" {
		t.Errorf("issue title = %q — the mutation did not commit", title)
	}
	if events != 1 {
		t.Errorf("events rows for the event = %d, want 1", events)
	}
	if rows != 0 {
		t.Errorf("notifications for the event = %d, want 0", rows)
	}
}

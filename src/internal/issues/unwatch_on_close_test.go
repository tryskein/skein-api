package issues

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
)

// unwatch_on_close_test.go — sk-551.10, item 5; sk-622.21 US-2 (T011, row
// No. 25). Expected values are the owner's rule: with the personal setting ON
// — the default, and what a person WITHOUT a prefs row gets — closing an
// issue removes the person's automatic subscriptions (creator, assignee)
// and keeps a manual one; with it explicitly OFF nothing changes; the
// removal writes no opt-out — reopening brings the automatic subscription
// back, unless the person explicitly unwatched the issue (opt-out).

func setUnwatchOnClose(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, on bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, unwatch_on_close, updated_at)
VALUES ($1, true, $2, now())
ON CONFLICT (user_id) DO UPDATE SET unwatch_on_close = EXCLUDED.unwatch_on_close`, userID, on); err != nil {
		t.Fatalf("set unwatch_on_close: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, userID)
	})
}

func addManualWatch(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual'], now())
ON CONFLICT (issue_id, user_id) DO UPDATE
   SET reasons = array_append(array_remove(issue_watchers.reasons, 'manual'), 'manual')`,
		issueID, userID); err != nil {
		t.Fatalf("add manual watch: %v", err)
	}
}

func sortedReasons(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) []string {
	t.Helper()
	rs := watcherReasons(t, pool, issueID, userID)
	slices.Sort(rs)
	return rs
}

func TestUnwatchOnClose_StripsAutoKeepsManualAndComesBackOnReopen(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool) // setting ON, creator of X
	ownerName := readUsername(t, pool, owner)
	worker := seedUser(t, pool) // setting ON, assignee of X AND a manual watcher of X
	workerName := usernameFor(t, pool, worker)
	keeper := seedUser(t, pool) // setting explicitly OFF, creator of Y
	keeperName := usernameFor(t, pool, keeper)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, worker, owner)
	addProjectMember(t, pool, pid, keeper, owner)
	setUnwatchOnClose(t, pool, owner, true)
	setUnwatchOnClose(t, pool, worker, true)
	setUnwatchOnClose(t, pool, keeper, false)

	store := NewStore(pool)
	x, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "x", Type: "task", Priority: 2, Assignee: ptrStr(workerName)},
		owner, HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue(x): %v", err)
	}
	addManualWatch(t, pool, x.ID, worker)
	y, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "y", Type: "task", Priority: 2},
		keeper, HumanActor(keeper, keeperName))
	if err != nil {
		t.Fatalf("CreateIssue(y): %v", err)
	}

	// Close X through Close (the MCP / dedicated close path).
	closedX, err := store.Close(ctx, CloseInput{
		ID: x.ID, ExpectedVersion: x.Version, Reason: "done",
		Actor: HumanActor(owner, ownerName),
	})
	if err != nil {
		t.Fatalf("Close(x): %v", err)
	}
	if n := countWatcherRows(t, pool, x.ID, owner); n != 0 {
		t.Errorf("creator with the setting on: %d subscription rows after close, want 0 (reasons %v)",
			n, watcherReasons(t, pool, x.ID, owner))
	}
	if got := sortedReasons(t, pool, x.ID, worker); !slices.Equal(got, []string{"manual"}) {
		t.Errorf("assignee+manual with the setting on: reasons %v after close, want [manual]", got)
	}

	// Close Y through UpdateStatus (the REST transition path); its creator
	// has the setting off.
	if _, err := store.UpdateStatus(ctx, y.ID,
		UpdateStatusInput{ExpectedVersion: y.Version, Status: StatusClosed},
		HumanActor(keeper, keeperName)); err != nil {
		t.Fatalf("UpdateStatus(y → closed): %v", err)
	}
	if !hasReason(watcherReasons(t, pool, y.ID, keeper), "creator") {
		t.Errorf("creator with the setting off lost the subscription on close: %v",
			watcherReasons(t, pool, y.ID, keeper))
	}

	// Reopen X: not sticky — the automatic subscriptions come back.
	if _, err := store.UpdateStatus(ctx, x.ID,
		UpdateStatusInput{ExpectedVersion: closedX.Version, Status: StatusOpen},
		HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("UpdateStatus(x → open): %v", err)
	}
	if !hasReason(watcherReasons(t, pool, x.ID, owner), "creator") {
		t.Errorf("creator not re-subscribed on reopen: %v", watcherReasons(t, pool, x.ID, owner))
	}
	if got := sortedReasons(t, pool, x.ID, worker); !slices.Equal(got, []string{"assignee", "manual"}) {
		t.Errorf("assignee after reopen: reasons %v, want [assignee manual]", got)
	}
}

// TestUnwatchOnClose_ReopenRespectsOptOut — the setting must not override a
// «don't watch»: a person who explicitly unwatched the issue (subscription
// deleted, opt-out recorded — sk-622.21 US-1) is not re-subscribed by a
// reopen.
func TestUnwatchOnClose_ReopenRespectsOptOut(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	setUnwatchOnClose(t, pool, owner, true)

	store := NewStore(pool)
	x, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "x", Type: "task", Priority: 2},
		owner, HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := notifications.NewStore(pool).RemoveIssueWatch(ctx, x.ID, owner); err != nil {
		t.Fatalf("explicit unwatch: %v", err)
	}
	closed, err := store.Close(ctx, CloseInput{
		ID: x.ID, ExpectedVersion: x.Version, Reason: "done", Actor: HumanActor(owner, ownerName),
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.UpdateStatus(ctx, x.ID,
		UpdateStatusInput{ExpectedVersion: closed.Version, Status: StatusOpen},
		HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := countWatcherRows(t, pool, x.ID, owner); n != 0 {
		t.Errorf("reopen re-subscribed a person who explicitly unwatched: reasons %v",
			watcherReasons(t, pool, x.ID, owner))
	}
	var optouts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, x.ID, owner).Scan(&optouts); err != nil {
		t.Fatalf("count opt-outs: %v", err)
	}
	if optouts != 1 {
		t.Errorf("opt-outs after close+reopen = %d, want 1 (kept)", optouts)
	}
}

// TestUnwatchOnClose_NoPrefsRowMeansOn — sk-622.21 US-2: a person who never
// saved preferences has the auto-unsubscribe ON. Closing their issue through
// either closing path (Close and the status transition) removes their
// automatic subscription, and writes no opt-out.
func TestUnwatchOnClose_NoPrefsRowMeansOn(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool) // no user_notification_prefs row
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	var prefsRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_notification_prefs WHERE user_id = $1`, owner).Scan(&prefsRows); err != nil {
		t.Fatalf("count prefs rows: %v", err)
	}
	if prefsRows != 0 {
		t.Fatalf("precondition: the person has %d prefs rows, want none", prefsRows)
	}

	store := NewStore(pool)
	for _, path := range []string{"Close", "UpdateStatus"} {
		x, err := store.CreateIssue(ctx, pid, prefix, "counter",
			CreateIssueInput{Title: "no prefs row " + path, Type: "task", Priority: 2},
			owner, HumanActor(owner, ownerName))
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if !hasReason(watcherReasons(t, pool, x.ID, owner), "creator") {
			t.Fatalf("precondition: the author is subscribed as creator")
		}
		if path == "Close" {
			_, err = store.Close(ctx, CloseInput{ID: x.ID, ExpectedVersion: x.Version, Reason: "done",
				Actor: HumanActor(owner, ownerName)})
		} else {
			_, err = store.UpdateStatus(ctx, x.ID, UpdateStatusInput{ExpectedVersion: x.Version, Status: StatusClosed},
				HumanActor(owner, ownerName))
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if n := countWatcherRows(t, pool, x.ID, owner); n != 0 {
			t.Errorf("%s: a person without a prefs row kept the automatic subscription: %v",
				path, watcherReasons(t, pool, x.ID, owner))
		}
		var optouts int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, x.ID, owner).Scan(&optouts); err != nil {
			t.Fatalf("count opt-outs: %v", err)
		}
		if optouts != 0 {
			t.Errorf("%s: the auto-unsubscribe wrote an opt-out", path)
		}
	}
}

// TestUnwatchOnClose_AssignOnClosedIssueAddsNoAutoReason — sk-622.21,
// decisions R23 / R23′ (found in review).
// Reference: the US-2 acceptance read as an invariant of the state — «on
// closed issues there are no subscriptions made only of automatic reasons
// for users with the setting on» — held at commit; R23′: an assignment on an
// already closed issue goes the way of the close — the new assignee gets the
// feed row of the assignment with its Watching card, and after the commit a
// person with the setting on has no automatic reason left; a manual
// subscription and the setting off behave as before. The subject is the
// production edit path (PatchIssue).
//
// Counterexamples it catches: the edit leaving the new assignee subscribed
// with reason `assignee` regardless of the issue's status (the code before
// R23); the edit not subscribing at all, so the assignee gets no feed row
// (the first R23 fix, before R23′).
func TestUnwatchOnClose_AssignOnClosedIssueAddsNoAutoReason(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)

	type assignCase struct {
		name        string
		settingOn   *bool // nil = no prefs row (reads as on)
		manualWatch bool
		want        []string // reasons after the edit; nil = no row
	}
	on, off := true, false
	cases := []assignCase{
		{name: "no prefs row, not watching", settingOn: nil, want: nil},
		{name: "setting on, manual watch", settingOn: &on, manualWatch: true, want: []string{"manual"}},
		{name: "setting off", settingOn: &off, want: []string{"assignee"}},
	}

	store := NewStore(pool)

	type fixture struct {
		c       assignCase
		user    uuid.UUID
		issueID string
	}
	fixtures := make([]fixture, 0, len(cases))
	for _, c := range cases {
		u := seedUser(t, pool)
		uName := usernameFor(t, pool, u)
		addProjectMember(t, pool, pid, u, owner)
		if c.settingOn != nil {
			setUnwatchOnClose(t, pool, u, *c.settingOn)
		}
		x, err := store.CreateIssue(ctx, pid, prefix, "counter",
			CreateIssueInput{Title: "assign on closed: " + c.name, Type: "task", Priority: 2},
			owner, HumanActor(owner, ownerName))
		if err != nil {
			t.Fatalf("%s: CreateIssue: %v", c.name, err)
		}
		if c.manualWatch {
			addManualWatch(t, pool, x.ID, u)
		}
		closed, err := store.Close(ctx, CloseInput{ID: x.ID, ExpectedVersion: x.Version, Reason: "done",
			Actor: HumanActor(owner, ownerName)})
		if err != nil {
			t.Fatalf("%s: Close: %v", c.name, err)
		}
		if _, err := store.PatchIssue(ctx, x.ID,
			PatchIssueInput{ExpectedVersion: closed.Version, Assignee: ptrStr(uName)},
			HumanActor(owner, ownerName)); err != nil {
			t.Fatalf("%s: PatchIssue(assignee) on the closed issue: %v", c.name, err)
		}
		fixtures = append(fixtures, fixture{c: c, user: u, issueID: x.ID})
	}

	check := func(when string) {
		t.Helper()
		for _, f := range fixtures {
			got := sortedReasons(t, pool, f.issueID, f.user)
			if f.c.want == nil {
				if n := countWatcherRows(t, pool, f.issueID, f.user); n != 0 {
					t.Errorf("%s / %s: subscription %v on the closed issue, want none", when, f.c.name, got)
				}
				continue
			}
			if !slices.Equal(got, f.c.want) {
				t.Errorf("%s / %s: reasons %v, want %v", when, f.c.name, got, f.c.want)
			}
		}
	}
	check("right after the edit")

	// Every new assignee gets the assignment as a feed row carded with the
	// issue — the last notification the automatic subscription brings.
	for _, f := range fixtures {
		var rows int
		if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1
   AND e.type = 'issue.updated'
   AND e.entity_type = 'issue'
   AND e.entity_id = $2
   AND n.reason = $3
   AND n.feed_channel
   AND n.card_issue_id = $2`, f.user, f.issueID, string(notifications.ReasonKeyAssignment)).Scan(&rows); err != nil {
			t.Fatalf("count assignment feed rows: %v", err)
		}
		if rows != 1 {
			t.Errorf("%s: %d «Issue assignment» feed rows carded with the issue, want 1", f.c.name, rows)
		}
	}
}

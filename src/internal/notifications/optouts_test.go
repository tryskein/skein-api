package notifications

// optouts_test.go — sk-622.21, T010, matrix rows No. 18 and No. 18a.
//
// The model under test (US-1, US-6, INV-1, INV-2, D2, R3, K3):
//
//   - an explicit unwatch of an issue the user is subscribed to deletes the
//     subscription, records the opt-out (U, X) and clears card X on U's
//     notifications — nobody else's, no other card;
//   - "remove" of a historical card (no subscription) clears the card and
//     records NO opt-out;
//   - a manual watch deletes the opt-out;
//   - the bulk sweep by state deletes WITHOUT an opt-out and clears the cards
//     of every touched closed card, historical ones included; the bulk
//     selection by id is handled per card kind; the answer counts stopped /
//     kept manual / historical removed;
//   - the opt-out — and, until the reconciliation, an old-version tombstone —
//     blocks the AUTOMATIC subscription for exactly the pair (user, issue);
//     the fan-out and the reads do not read it;
//   - issue_watchers never holds an 'unwatched' row this version wrote.
//
// Expected values are literals from those rules. Tests skip without
// TEST_DATABASE_URL.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// optFixture is one project, its owner (the actor of every event) and a
// counter for issue ids.
type optFixture struct {
	pool    *pgxpool.Pool
	store   *Store
	owner   uuid.UUID
	project uuid.UUID
	tag     string
	n       int
}

func newOptFixture(t *testing.T, tag string) *optFixture {
	t.Helper()
	pool := newTestPool(t)
	owner := seedTestUser(t, pool, "user")
	return &optFixture{
		pool: pool, store: NewStore(pool), owner: owner,
		project: seedTestProject(t, pool, owner),
		tag:     tag + uuid.Must(uuid.NewRandom()).String()[:6],
	}
}

func (f *optFixture) issue(t *testing.T) string {
	t.Helper()
	f.n++
	id := f.tag + "-" + string(rune('a'+f.n-1))
	seedTestIssue(t, f.pool, f.project, f.owner, id, 2)
	return id
}

func (f *optFixture) member(t *testing.T) uuid.UUID {
	t.Helper()
	u := seedTestUser(t, f.pool, "user")
	addMember(t, f.pool, f.project, u, f.owner)
	return u
}

func (f *optFixture) close(t *testing.T, issueID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = '' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("close %s: %v", issueID, err)
	}
}

// event fans an issue event on issueID out (actor: the owner) and returns
// its id.
func (f *optFixture) event(t *testing.T, issueID string) int64 {
	t.Helper()
	return fanoutIssueEvent(t, f.pool, issueID, f.project, f.owner, "human")
}

// autoAdd is the automatic subscription primitive, as the issue paths call
// it, in its own transaction.
func autoAdd(t *testing.T, pool *pgxpool.Pool, issueID string, u uuid.UUID, reason, scope string) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return AddIssueWatcherScoped(ctx, tx, issueID, u, reason, scope)
	})
	if err != nil {
		t.Fatalf("AddIssueWatcherScoped(%s, %s, %s): %v", issueID, reason, scope, err)
	}
}

// watcherRow reads (exists, reasons, scope) of the pair's subscription row.
func watcherRow(t *testing.T, pool *pgxpool.Pool, issueID string, u uuid.UUID) (bool, []string, string) {
	t.Helper()
	var (
		reasons []string
		scope   string
	)
	err := pool.QueryRow(context.Background(),
		`SELECT reasons, scope FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, u).
		Scan(&reasons, &scope)
	if err == pgx.ErrNoRows {
		return false, nil, ""
	}
	if err != nil {
		t.Fatalf("read watcher row (%s, %s): %v", issueID, u, err)
	}
	return true, reasons, scope
}

func hasOptOut(t *testing.T, pool *pgxpool.Pool, issueID string, u uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, issueID, u).
		Scan(&n); err != nil {
		t.Fatalf("read opt-out: %v", err)
	}
	return n == 1
}

// carded counts u's notifications whose Watching card is issueID.
func carded(t *testing.T, pool *pgxpool.Pool, u uuid.UUID, issueID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND card_issue_id = $2`, u, issueID).
		Scan(&n); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	return n
}

// noTombstones asserts none of the users has an 'unwatched' subscription row.
func noTombstones(t *testing.T, pool *pgxpool.Pool, users ...uuid.UUID) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watchers WHERE user_id = ANY($1::uuid[]) AND 'unwatched' = ANY(reasons)`,
		users).Scan(&n); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if n != 0 {
		t.Errorf("issue_watchers holds %d 'unwatched' rows — a tombstone was written (INV-1)", n)
	}
}

// TestOptOuts_ExplicitUnwatchDeletesRecordsAndClearsOnlyThatCard — No. 18: the
// subscribed branch of the single unwatch.
func TestOptOuts_ExplicitUnwatchDeletesRecordsAndClearsOnlyThatCard(t *testing.T) {
	f := newOptFixture(t, "ooex")
	ctx := context.Background()
	x, y := f.issue(t), f.issue(t)
	u, v := f.member(t), f.member(t)
	watchIssue(t, f.pool, x, u, "manual")
	watchIssue(t, f.pool, y, u, "manual")
	watchIssue(t, f.pool, x, v, "manual")
	f.event(t, x)
	f.event(t, y)
	if carded(t, f.pool, u, x) != 1 || carded(t, f.pool, u, y) != 1 || carded(t, f.pool, v, x) != 1 {
		t.Fatalf("precondition: each subscriber has one carded row")
	}

	if err := f.store.RemoveIssueWatch(ctx, x, u); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}

	if exists, reasons, _ := watcherRow(t, f.pool, x, u); exists {
		t.Errorf("subscription (U, X) still there with reasons %v — want the row deleted", reasons)
	}
	if !hasOptOut(t, f.pool, x, u) {
		t.Errorf("no opt-out (U, X) after an explicit unwatch of a subscription")
	}
	if n := carded(t, f.pool, u, x); n != 0 {
		t.Errorf("U still has %d notifications carded X", n)
	}
	if n := carded(t, f.pool, u, y); n != 1 {
		t.Errorf("U's card Y: %d rows, want 1 — the unwatch of X cleared another card", n)
	}
	if n := carded(t, f.pool, v, x); n != 1 {
		t.Errorf("V's card X: %d rows, want 1 — the unwatch of U cleared somebody else's card", n)
	}
	if hasOptOut(t, f.pool, x, v) || hasOptOut(t, f.pool, y, u) {
		t.Errorf("an opt-out appeared for a pair that was not unwatched")
	}

	// Idempotent: a second unwatch finds nothing and keeps one opt-out.
	if err := f.store.RemoveIssueWatch(ctx, x, u); err != nil {
		t.Fatalf("second RemoveIssueWatch: %v", err)
	}
	if !hasOptOut(t, f.pool, x, u) {
		t.Errorf("the opt-out vanished on a repeated unwatch")
	}
	noTombstones(t, f.pool, u, v)
}

// TestOptOuts_RemoveHistoricalCardWritesNoOptOut — No. 18, R3: a historical
// card (the automatic subscription was taken away on close, the close
// notification kept its card) removed by the single unwatch and by the
// selection branch of the bulk unwatch ("remove" in the product): cards
// cleared, no opt-out.
func TestOptOuts_RemoveHistoricalCardWritesNoOptOut(t *testing.T) {
	f := newOptFixture(t, "oohi")
	ctx := context.Background()

	for _, remove := range []struct {
		name string
		fn   func(t *testing.T, issueID string, u uuid.UUID) error
	}{
		{"single unwatch", func(_ *testing.T, issueID string, u uuid.UUID) error {
			return f.store.RemoveIssueWatch(ctx, issueID, u)
		}},
		{"bulk selection", func(t *testing.T, issueID string, u uuid.UUID) error {
			res, err := f.store.BulkUnwatch(ctx, u, BulkUnwatchInput{IssueIDs: []string{issueID}})
			if err == nil && res != (BulkUnwatchResult{HistoricalRemoved: 1}) {
				t.Errorf("BulkUnwatch answered %+v, want {HistoricalRemoved:1}", res)
			}
			return err
		}},
	} {
		t.Run(remove.name, func(t *testing.T) {
			x := f.issue(t)
			u := f.member(t)
			autoAdd(t, f.pool, x, u, ReasonAssignee, ScopeIssue)
			f.event(t, x)
			f.close(t, x)
			// The real auto-unsubscribe on close: no opt-out, card kept.
			if err := pgx.BeginTxFunc(ctx, f.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				_, err := StripAutoWatchersOnClose(ctx, tx, x)
				return err
			}); err != nil {
				t.Fatalf("StripAutoWatchersOnClose: %v", err)
			}
			if exists, _, _ := watcherRow(t, f.pool, x, u); exists || carded(t, f.pool, u, x) != 1 {
				t.Fatalf("precondition: X must be a historical card of U (no subscription, one carded row)")
			}

			if err := remove.fn(t, x, u); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if n := carded(t, f.pool, u, x); n != 0 {
				t.Errorf("historical card X still on %d rows", n)
			}
			if hasOptOut(t, f.pool, x, u) {
				t.Errorf("an opt-out was written for a historical card (R3 forbids it)")
			}
			noTombstones(t, f.pool, u)
		})
	}
}

// TestOptOuts_ManualWatchRemovesOptOut — No. 18: watching by hand undoes the
// refusal.
func TestOptOuts_ManualWatchRemovesOptOut(t *testing.T) {
	f := newOptFixture(t, "oomw")
	ctx := context.Background()
	x := f.issue(t)
	u := f.member(t)
	watchIssue(t, f.pool, x, u, "manual")
	if err := f.store.RemoveIssueWatch(ctx, x, u); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}
	if !hasOptOut(t, f.pool, x, u) {
		t.Fatalf("precondition: opt-out recorded")
	}

	if err := f.store.AddIssueWatch(ctx, x, u, false); err != nil {
		t.Fatalf("AddIssueWatch: %v", err)
	}
	if hasOptOut(t, f.pool, x, u) {
		t.Errorf("the opt-out survived a manual watch")
	}
	if exists, reasons, _ := watcherRow(t, f.pool, x, u); !exists || len(reasons) != 1 || reasons[0] != ReasonManual {
		t.Errorf("subscription after the manual watch: exists=%v reasons=%v, want [manual]", exists, reasons)
	}
}

// TestOptOuts_BulkByStateDeletesWithoutOptOutAndClearsClosedCards — No. 18:
// "unsubscribe from closed".
func TestOptOuts_BulkByStateDeletesWithoutOptOutAndClearsClosedCards(t *testing.T) {
	f := newOptFixture(t, "oobs")
	ctx := context.Background()
	closedAuto, closedManual, historical, open := f.issue(t), f.issue(t), f.issue(t), f.issue(t)
	u, v := f.member(t), f.member(t)

	autoAdd(t, f.pool, closedAuto, u, ReasonCreator, ScopeIssue)
	watchIssue(t, f.pool, closedManual, u, "manual")
	autoAdd(t, f.pool, historical, u, ReasonAssignee, ScopeIssue)
	autoAdd(t, f.pool, open, u, ReasonAssignee, ScopeIssue)
	watchIssue(t, f.pool, closedAuto, v, "manual")
	for _, id := range []string{closedAuto, closedManual, historical, open} {
		f.event(t, id)
	}
	// historical: subscription gone without an opt-out, card kept.
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, historical, u); err != nil {
		t.Fatalf("make historical: %v", err)
	}
	for _, id := range []string{closedAuto, closedManual, historical} {
		f.close(t, id)
	}

	res, err := f.store.BulkUnwatch(ctx, u, BulkUnwatchInput{State: WatchingStateClosed})
	if err != nil {
		t.Fatalf("BulkUnwatch: %v", err)
	}
	if res != (BulkUnwatchResult{Unwatched: 1, KeptManual: 1, HistoricalRemoved: 1}) {
		t.Errorf("answer = %+v, want {Unwatched:1 KeptManual:1 HistoricalRemoved:1}", res)
	}

	if exists, _, _ := watcherRow(t, f.pool, closedAuto, u); exists {
		t.Errorf("the automatic subscription on a closed task survived the sweep")
	}
	if hasOptOut(t, f.pool, closedAuto, u) || hasOptOut(t, f.pool, historical, u) {
		t.Errorf("the sweep by state wrote an opt-out")
	}
	if n := carded(t, f.pool, u, closedAuto); n != 0 {
		t.Errorf("card of the swept closed subscription still on %d rows", n)
	}
	if n := carded(t, f.pool, u, historical); n != 0 {
		t.Errorf("historical closed card still on %d rows", n)
	}
	if exists, _, _ := watcherRow(t, f.pool, closedManual, u); !exists || carded(t, f.pool, u, closedManual) != 1 {
		t.Errorf("the kept manual subscription or its card was touched")
	}
	if exists, _, _ := watcherRow(t, f.pool, open, u); !exists || carded(t, f.pool, u, open) != 1 {
		t.Errorf("an open task's subscription or card was touched by the closed sweep")
	}
	if n := carded(t, f.pool, v, closedAuto); n != 1 {
		t.Errorf("another user's card on the closed task: %d rows, want 1", n)
	}
	noTombstones(t, f.pool, u, v)
}

// TestOptOuts_BulkSelectionByCardKind — No. 18: "stop watching the selected"
// handles each id by its card kind.
func TestOptOuts_BulkSelectionByCardKind(t *testing.T) {
	f := newOptFixture(t, "oobi")
	ctx := context.Background()
	subscribed, historical, unrelated := f.issue(t), f.issue(t), f.issue(t)
	u := f.member(t)
	watchIssue(t, f.pool, subscribed, u, "manual")
	autoAdd(t, f.pool, historical, u, ReasonCreator, ScopeIssue)
	f.event(t, subscribed)
	f.event(t, historical)
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, historical, u); err != nil {
		t.Fatalf("make historical: %v", err)
	}

	res, err := f.store.BulkUnwatch(ctx, u, BulkUnwatchInput{IssueIDs: []string{subscribed, historical, unrelated}})
	if err != nil {
		t.Fatalf("BulkUnwatch: %v", err)
	}
	if res != (BulkUnwatchResult{Unwatched: 1, KeptManual: 0, HistoricalRemoved: 1}) {
		t.Errorf("answer = %+v, want {Unwatched:1 KeptManual:0 HistoricalRemoved:1}", res)
	}
	if exists, _, _ := watcherRow(t, f.pool, subscribed, u); exists {
		t.Errorf("selected manual subscription survived")
	}
	if !hasOptOut(t, f.pool, subscribed, u) {
		t.Errorf("selected subscription: no opt-out (an explicit selection is an explicit unwatch)")
	}
	if hasOptOut(t, f.pool, historical, u) || hasOptOut(t, f.pool, unrelated, u) {
		t.Errorf("an opt-out was written for a historical card or an unrelated id")
	}
	if carded(t, f.pool, u, subscribed) != 0 || carded(t, f.pool, u, historical) != 0 {
		t.Errorf("cards of the selection were not cleared")
	}
	noTombstones(t, f.pool, u)
}

// TestOptOuts_KeyIsThePair — No. 18, INV-2: an opt-out (U, X) recorded by an
// explicit unwatch blocks the automatic subscription of exactly that pair,
// for every automatic reason and scope; the neighbouring pairs (U, Y) and
// (V, X) are subscribed in the same fixture. The fan-out and the reads do
// not read the opt-out: U still gets X's events through a subtree
// subscription on X's parent, counted by the Watching badge.
func TestOptOuts_KeyIsThePair(t *testing.T) {
	f := newOptFixture(t, "oopk")
	ctx := context.Background()
	parent := f.issue(t)
	x := parent + ".1"
	seedChild(t, f.pool, f.project, f.owner, x, parent)
	y := f.issue(t)
	u, v := f.member(t), f.member(t)
	watchIssue(t, f.pool, x, u, "manual")
	if err := f.store.RemoveIssueWatch(ctx, x, u); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}

	for _, c := range []struct{ reason, scope string }{
		{ReasonAssignee, ScopeIssue}, {ReasonCreator, ScopeIssue},
		{ReasonAssignee, ScopeSubtree}, {ReasonCreator, ScopeSubtree},
	} {
		autoAdd(t, f.pool, x, u, c.reason, c.scope)
		if exists, reasons, _ := watcherRow(t, f.pool, x, u); exists {
			t.Errorf("auto %s/%s subscribed (U, X) despite the opt-out (reasons %v)", c.reason, c.scope, reasons)
		}
	}
	autoAdd(t, f.pool, y, u, ReasonAssignee, ScopeSubtree)
	if exists, _, _ := watcherRow(t, f.pool, y, u); !exists {
		t.Errorf("(U, Y) was not subscribed — the opt-out is read by user, not by pair")
	}
	autoAdd(t, f.pool, x, v, ReasonAssignee, ScopeSubtree)
	if exists, _, _ := watcherRow(t, f.pool, x, v); !exists {
		t.Errorf("(V, X) was not subscribed — the opt-out is read by issue, not by pair")
	}

	// Delivery and reads ignore the opt-out.
	watchSubtree(t, f.pool, parent, u, "manual")
	eid := f.event(t, x)
	var card *string
	if err := f.pool.QueryRow(ctx,
		`SELECT card_issue_id FROM notifications WHERE user_id = $1 AND event_id = $2`, u, eid).Scan(&card); err != nil {
		t.Fatalf("the fan-out read the opt-out: no row for U on X's event (%v)", err)
	}
	if card == nil || *card != parent {
		t.Errorf("card of X's event for U = %v, want %s", card, parent)
	}
	unread, err := f.store.CountWatchingUnread(ctx, u)
	if err != nil {
		t.Fatalf("CountWatchingUnread: %v", err)
	}
	if unread != 1 {
		t.Errorf("Watching badge of U = %d, want 1 — the read ignored a delivered row", unread)
	}
	noTombstones(t, f.pool, u, v)
}

// archiveCarded archives every notification of u carded issueID and returns
// how many it archived.
func archiveCarded(t *testing.T, pool *pgxpool.Pool, u uuid.UUID, issueID string) int64 {
	t.Helper()
	ct, err := pool.Exec(context.Background(),
		`UPDATE notifications SET archived_at = now() WHERE user_id = $1 AND card_issue_id = $2`, u, issueID)
	if err != nil {
		t.Fatalf("archive carded rows: %v", err)
	}
	return ct.RowsAffected()
}

// TestOptOuts_ClearsCardOnArchivedNotificationsToo — INV-6 / K6 (fix found
// in review): after an explicit unwatch or "remove"
// of card X, U has NO notification carded X — archived ones included. Each
// operation that clears cards is run on a card carried by one archived and
// one live notification. Red before the fix: the clearing statements were
// limited to archived_at IS NULL, so the archived row kept card X.
func TestOptOuts_ClearsCardOnArchivedNotificationsToo(t *testing.T) {
	f := newOptFixture(t, "ooar")
	ctx := context.Background()

	for _, op := range []struct {
		name string
		// prepare leaves X a card of u and returns nothing; run clears it.
		prepare func(t *testing.T, x string, u uuid.UUID)
		run     func(t *testing.T, x string, u uuid.UUID) error
	}{
		{"explicit unwatch",
			func(t *testing.T, x string, u uuid.UUID) { watchIssue(t, f.pool, x, u, "manual") },
			func(_ *testing.T, x string, u uuid.UUID) error { return f.store.RemoveIssueWatch(ctx, x, u) }},
		{"bulk selection of a subscription",
			func(t *testing.T, x string, u uuid.UUID) { watchIssue(t, f.pool, x, u, "manual") },
			func(_ *testing.T, x string, u uuid.UUID) error {
				_, err := f.store.BulkUnwatch(ctx, u, BulkUnwatchInput{IssueIDs: []string{x}})
				return err
			}},
		{"bulk selection of a historical card",
			func(t *testing.T, x string, u uuid.UUID) { autoAdd(t, f.pool, x, u, ReasonAssignee, ScopeIssue) },
			func(t *testing.T, x string, u uuid.UUID) error {
				if _, err := f.pool.Exec(ctx,
					`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, x, u); err != nil {
					t.Fatalf("make historical: %v", err)
				}
				_, err := f.store.BulkUnwatch(ctx, u, BulkUnwatchInput{IssueIDs: []string{x}})
				return err
			}},
		{"bulk sweep of closed",
			func(t *testing.T, x string, u uuid.UUID) { autoAdd(t, f.pool, x, u, ReasonAssignee, ScopeIssue) },
			func(t *testing.T, x string, u uuid.UUID) error {
				f.close(t, x)
				_, err := f.store.BulkUnwatch(ctx, u, BulkUnwatchInput{State: WatchingStateClosed})
				return err
			}},
	} {
		t.Run(op.name, func(t *testing.T) {
			x := f.issue(t)
			u := f.member(t)
			op.prepare(t, x, u)
			f.event(t, x)
			if n := archiveCarded(t, f.pool, u, x); n != 1 {
				t.Fatalf("precondition: one carded row to archive, archived %d", n)
			}
			f.event(t, x)
			if n := carded(t, f.pool, u, x); n != 2 {
				t.Fatalf("precondition: X carded on one archived and one live row, got %d rows", n)
			}

			if err := op.run(t, x, u); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}
			if n := carded(t, f.pool, u, x); n != 0 {
				t.Errorf("U still has %d notifications carded X after %s (INV-6: archived ones included)", n, op.name)
			}
		})
	}
}

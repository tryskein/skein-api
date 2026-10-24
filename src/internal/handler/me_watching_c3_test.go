package handler

// sk-303.13 / T015 — contract C3, carried onto the R2 read (sk-622.21,
// T022). The badge, the card counts and the card timelines are three views
// of ONE set — the caller's notifications by the Watching card each was
// given when it was written — and they have to agree about what a
// subscription covers (INV-4/INV-5), about how a subtree is matched (by
// parent-child edges, US-3), and about what the column owes the reader when
// it cannot show everything.
//
// Rows on the subtree are written by the product's fan-out
// (fanOutMeEvent), so the card is the one the delivery computes from the
// hierarchy; the other rows are seeded with the card the US-4 rules give
// them (seedMeCardNotification).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
)

// seedScopedWatcher inserts an issue_watchers row with an explicit scope
// ('issue' | 'subtree').
func seedScopedWatcher(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reasons []string, scope string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope, since)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (issue_id, user_id) DO UPDATE
SET reasons = EXCLUDED.reasons, scope = EXCLUDED.scope`,
		issueID, userID, reasons, scope); err != nil {
		t.Fatalf("seed scoped watcher: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, userID)
	})
}

// fanOutMeEvent records an issue.status_changed on issueID by actor and
// fans it out through notifications.FanOut, in one committed transaction —
// the path that computes each recipient's Watching card (US-4).
func fanOutMeEvent(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, issueID string, actor uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eid int64
	if err := tx.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id)
VALUES ('issue.status_changed', 'tester', $1, 'human', 'issue', $2, $3)
RETURNING id`, actor, issueID, projectID).Scan(&eid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
		EventID: eid, EventType: "issue.status_changed", EntityType: "issue",
		IssueID: issueID, ProjectID: projectID, ActorUserID: actor, ActorType: "human",
	}); err != nil {
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedMeEdge makes child a child of parent (a parent-child edge).
func seedMeEdge(t *testing.T, pool *pgxpool.Pool, child, parent string, creator uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, created_by) VALUES ($1, $2, 'parent-child', $3)`,
		child, parent, creator); err != nil {
		t.Fatalf("seed edge %s → %s: %v", child, parent, err)
	}
}

// watchingCard finds the card for one issue id in a /me/watching body.
func watchingCard(t *testing.T, body map[string]any, issueID string) map[string]any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing/not array; body=%v", body)
	}
	for _, raw := range items {
		row := raw.(map[string]any)
		item, ok := row["item"].(map[string]any)
		if !ok {
			continue
		}
		if item["id"] == issueID {
			return row
		}
	}
	t.Fatalf("no card for issue %q; body=%v", issueID, body)
	return nil
}

// TestWatching_SubtreeActivityLandsOnAncestorCard (US-6): a subtree
// subscription on an ancestor counts and shows the activity of its
// descendants — on the ANCESTOR's card, not on a card per descendant,
// because the ancestor's subscription is the one that exists and the one
// an unsubscribe would remove.
func TestWatching_SubtreeActivityLandsOnAncestorCard(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	epic := prefix + "-1"
	child := prefix + "-1.2"
	grandchild := prefix + "-1.2.3"
	for _, id := range []string{epic, child, grandchild} {
		seedMeIssue(t, pool, pid, other, id, "", nil)
	}
	seedMeEdge(t, pool, child, epic, other)
	seedMeEdge(t, pool, grandchild, child, other)
	seedScopedWatcher(t, pool, epic, user, []string{"manual"}, "subtree")

	// One event on the child, one on the grandchild. Neither issue has a
	// subscription of its own; the fan-out cards both to the epic.
	for _, id := range []string{child, grandchild} {
		fanOutMeEvent(t, pool, pid, id, other)
	}

	res, body := callWatching(t, h, user)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d; body=%v", res.StatusCode, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1 — the tab lists SUBSCRIPTIONS, so descendant activity must not spawn cards; body=%v",
			len(items), body)
	}
	card := watchingCard(t, body, epic)
	if got := int(card["unreadEventsCount"].(float64)); got != 2 {
		t.Errorf("ancestor card unreadEventsCount = %d, want 2 (child + grandchild)", got)
	}
	if got := len(card["recentEvents"].([]any)); got != 2 {
		t.Errorf("ancestor card recentEvents = %d rows, want 2 — the timeline reads the same card as the count", got)
	}
	if got := int(body["unreadTotal"].(float64)); got != 2 {
		t.Errorf("unreadTotal = %d, want 2", got)
	}

	// The badge must agree — it is the same predicate (INV-5).
	_, counts := callCounts(t, newMeWatchingHandler(pool), user)
	if got := mustInt(t, counts, "watching"); got != 2 {
		t.Errorf("header watching badge = %d, want 2 — the badge ignored the subtree subscription the delivery honoured", got)
	}
}

// TestWatching_ExactAndSubtreeCountOnce (INV-4): a task covered by BOTH
// an exact subscription and an ancestor's subtree subscription is counted
// once, and the per-card counts still sum to the badge.
func TestWatching_ExactAndSubtreeCountOnce(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	epic := prefix + "-1"
	child := prefix + "-1.2"
	seedMeIssue(t, pool, pid, other, epic, "", nil)
	seedMeIssue(t, pool, pid, other, child, "", nil)
	seedMeEdge(t, pool, child, epic, other)

	seedScopedWatcher(t, pool, epic, user, []string{"manual"}, "subtree")
	seedScopedWatcher(t, pool, child, user, []string{"manual"}, "issue")

	fanOutMeEvent(t, pool, pid, child, other)

	_, body := callWatching(t, h, user)
	if got := int(body["unreadTotal"].(float64)); got != 1 {
		t.Fatalf("unreadTotal = %d, want 1 — one notification matched two subscriptions and was counted twice; body=%v", got, body)
	}

	sum := 0
	for _, raw := range body["items"].([]any) {
		sum += int(raw.(map[string]any)["unreadEventsCount"].(float64))
	}
	if sum != 1 {
		t.Errorf("sum of per-card counts = %d, want 1 — the cards must PARTITION the set", sum)
	}
	// Most specific wins: the exact subscription owns the row.
	if got := int(watchingCard(t, body, child)["unreadEventsCount"].(float64)); got != 1 {
		t.Errorf("child card count = %d, want 1 (exact subscription is the most specific match)", got)
	}
	if got := int(watchingCard(t, body, epic)["unreadEventsCount"].(float64)); got != 0 {
		t.Errorf("epic card count = %d, want 0 (the row is attributed to the exact subscription)", got)
	}

	_, counts := callCounts(t, h, user)
	if got := mustInt(t, counts, "watching"); got != 1 {
		t.Errorf("header watching badge = %d, want 1", got)
	}
}

// TestWatching_SubtreePredicateIsDelimiterSafe (US-3, US-6): the subtree
// is the hierarchy of parent-child edges, not an id prefix. A subtree watch
// on "…-a_b" covers its real descendant "…-a_b.1" (an edge), and neither a
// task that only shares the id prefix without an edge ("…-a_b.2") nor the
// neighbouring branch of a LIKE-lookalike ("…-aXb.1") lands on its card.
func TestWatching_SubtreePredicateIsDelimiterSafe(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	watched := prefix + "-a_b"
	ownChild := prefix + "-a_b.1"
	prefixOnly := prefix + "-a_b.2"
	neighbour := prefix + "-aXb"
	neighbourChild := prefix + "-aXb.1"
	for _, id := range []string{watched, ownChild, prefixOnly, neighbour, neighbourChild} {
		seedMeIssue(t, pool, pid, other, id, "", nil)
	}
	seedMeEdge(t, pool, ownChild, watched, other)
	seedMeEdge(t, pool, neighbourChild, neighbour, other)
	seedScopedWatcher(t, pool, watched, user, []string{"manual"}, "subtree")

	for _, id := range []string{ownChild, prefixOnly, neighbourChild} {
		fanOutMeEvent(t, pool, pid, id, other)
	}

	_, body := callWatching(t, h, user)
	if got := int(body["unreadTotal"].(float64)); got != 1 {
		t.Fatalf("unreadTotal = %d, want 1 — a task without an edge (id prefix or LIKE lookalike) landed on the card; body=%v", got, body)
	}
	if got := int(watchingCard(t, body, watched)["unreadEventsCount"].(float64)); got != 1 {
		t.Errorf("card count = %d, want 1 (own descendant only)", got)
	}
	if got := len(body["items"].([]any)); got != 1 {
		t.Errorf("items len = %d, want 1", got)
	}
}

// TestWatching_TotalsAndTruncation (US-5 / INV-5): past the card cap the
// column reports its own incompleteness AND still publishes the complete
// unread total, so badge and column never diverge silently.
func TestWatching_TotalsAndTruncation(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	// One more subscription than the card cap, each with one unread row.
	n := notifications.WatchingFeedLimit + 5
	for i := range n {
		id := prefix + "-" + itoa(i+1)
		seedMeIssue(t, pool, pid, other, id, "", nil)
		seedScopedWatcher(t, pool, id, user, []string{"manual"}, "issue")
		ev := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", id, other)
		seedMeCardNotification(t, pool, user, ev, "issue.status_changed", pid, id)
	}

	_, body := callWatching(t, h, user)
	items := body["items"].([]any)
	if len(items) != notifications.WatchingFeedLimit {
		t.Fatalf("items len = %d, want the cap %d", len(items), notifications.WatchingFeedLimit)
	}
	if truncated, _ := body["truncated"].(bool); !truncated {
		t.Errorf("truncated = false; the column showed %d of %d subscriptions and must say so", len(items), n)
	}
	if got := int(body["subscriptionsTotal"].(float64)); got != n {
		t.Errorf("subscriptionsTotal = %d, want %d", got, n)
	}
	if got := int(body["unreadTotal"].(float64)); got != n {
		t.Errorf("unreadTotal = %d, want %d — the total must NOT be capped with the cards", got, n)
	}

	sum := 0
	for _, raw := range items {
		sum += int(raw.(map[string]any)["unreadEventsCount"].(float64))
	}
	if sum >= n {
		t.Errorf("sum of shown cards = %d; the fixture is meant to exercise the truncated case (< %d)", sum, n)
	}

	_, counts := callCounts(t, h, user)
	if got := mustInt(t, counts, "watching"); got != n {
		t.Errorf("header watching badge = %d, want %d (uncapped, equal to unreadTotal)", got, n)
	}
}

// TestWatching_PushOnlyAndSilentSubscriptions: a push-only row is on no
// card and in no total (INV-11); a subscription with no matching activity
// is STILL returned, so "subscriptions exist, but there were no events" stays
// distinguishable from "there are no subscriptions at all" (US-5).
func TestWatching_PushOnlyAndSilentSubscriptions(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	silent := prefix + "-1"
	noisy := prefix + "-2"
	seedMeIssue(t, pool, pid, other, silent, "", nil)
	seedMeIssue(t, pool, pid, other, noisy, "", nil)
	seedScopedWatcher(t, pool, silent, user, []string{"manual"}, "issue")
	seedScopedWatcher(t, pool, noisy, user, []string{"manual"}, "issue")

	evPush := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", noisy, other)
	seedMePushOnlyNotification(t, pool, user, evPush, "issue.status_changed", pid)

	_, body := callWatching(t, h, user)
	if got := int(body["unreadTotal"].(float64)); got != 0 {
		t.Errorf("unreadTotal = %d, want 0 — the only row is push-only (INV-11); body=%v", got, body)
	}
	if got := len(body["items"].([]any)); got != 2 {
		t.Fatalf("items len = %d, want 2 — silent subscriptions must still be listed", got)
	}
	if got := len(watchingCard(t, body, noisy)["recentEvents"].([]any)); got != 0 {
		t.Errorf("recentEvents = %d rows, want 0 — a push-only row is on no timeline", got)
	}

	_, counts := callCounts(t, h, user)
	if got := mustInt(t, counts, "watching"); got != 0 {
		t.Errorf("header watching badge = %d, want 0", got)
	}
}

// itoa is strconv.Itoa without the import churn in this file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

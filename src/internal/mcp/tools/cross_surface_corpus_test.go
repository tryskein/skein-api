package tools

// sk-303.13 / T016 — cross-surface corpus (coverage matrix, rows 28-31).
//
// One fixture, one expected id set, checked on every surface that reads
// notification rows. The point is not that each surface works: each has its
// own tests for that. The point is that they all answer with the SAME SET,
// because the failure this whole wave exists to remove is a row that one
// surface shows and another hides (INV-2/INV-3/INV-11).
//
// Why this file lives in internal/mcp/tools. The set has to be asserted
// through the agent tool AND through the human REST/me surfaces in one
// fixture; the tool handlers here are unexported, and this package can
// import internal/handler while handler cannot import this one. So the
// corpus is built here and the REST handlers are driven from it.
//
// Two surfaces are asserted elsewhere, and deliberately not re-asserted with
// a weaker copy here:
//
//   - The LIVE STREAM. internal/outbox's
//     TestPushLifecycle_LiveNATSCarriesFeedRowsOnly proves it FROM THE WIRE —
//     what actually arrives on skein.user.{id}.notification — which is
//     strictly stronger than anything reachable from this package, since
//     tickNotifications is unexported. A copy here could only re-read a
//     column.
//   - The AGENT MONITOR (GET /agent/v1/notifications/poll). It is not a
//     notification-reading surface at all: since sk-164 it is an event tail
//     over a token's scope and never touches the notifications table, so
//     there is no row for it to include or exclude. The "agent notification
//     reading tool" of INV-11 is skein_notifications(inbox),
//     which IS in the corpus below.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
)

// corpus is the fixture: one caller, one project, four issues and eight
// notification rows spanning every class the read side has to tell apart.
type corpus struct {
	pool    *pgxpool.Pool
	caller  uuid.UUID
	actor   uuid.UUID
	project uuid.UUID
	prefix  string

	issueA string // watched exactly
	issueB string // NOT watched
	epic   string // watched with scope=subtree
	child  string // descendant of epic, not watched itself

	feedIssueA   int64 // feed row on A
	feedCommentA int64 // feed row on a comment under A
	feedChild    int64 // feed row on the epic's descendant
	feedIssueB   int64 // feed row on an UNwatched issue
	feedLegacy   int64 // feed row whose type the legacy whitelist excluded
	feedMention  int64 // feed mention row
	pushOnlyA    int64 // push-only row on A — visible nowhere
	archivedA    int64 // archived feed row on A
}

// visibleUnread is the expected id set of every surface that lists the
// caller's unread feed rows.
func (c corpus) visibleUnread() []int64 {
	return sorted([]int64{c.feedIssueA, c.feedCommentA, c.feedChild, c.feedIssueB, c.feedLegacy, c.feedMention})
}

// watchingAttributed is the subset that carries a Watching card (sk-622.21,
// US-4: the fan-out gives it at write time): A's three rows (including the
// one the legacy whitelist used to hide), carded to A, plus the epic's
// descendant (a parent-child edge), carded to the epic. Mentions never get a
// card, and the unwatched issue B belongs to no subscription.
func (c corpus) watchingAttributed() []int64 {
	return sorted([]int64{c.feedIssueA, c.feedCommentA, c.feedLegacy, c.feedChild})
}

func sorted(in []int64) []int64 {
	out := append([]int64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func newCorpus(t *testing.T, pool *pgxpool.Pool) corpus {
	t.Helper()
	ctx := context.Background()

	c := corpus{pool: pool}
	c.caller, _ = seedUser(t, pool, "user")
	c.actor, _ = seedUser(t, pool, "user")
	c.project, c.prefix = seedProject(t, pool, c.caller, "xsc")

	c.issueA = seedIssue(t, pool, c.project, c.actor, c.prefix+"-1", "watched exactly", "open", 2)
	c.issueB = seedIssue(t, pool, c.project, c.actor, c.prefix+"-2", "not watched", "open", 2)
	c.epic = seedIssue(t, pool, c.project, c.actor, c.prefix+"-3", "watched subtree", "open", 2)
	c.child = seedIssue(t, pool, c.project, c.actor, c.prefix+"-3.1", "descendant", "open", 2)
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, created_by) VALUES ($1, $2, 'parent-child', $3)`,
		c.child, c.epic, c.actor); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	seedWatcherScoped(t, pool, c.issueA, c.caller, []string{"manual"}, "issue")
	seedWatcherScoped(t, pool, c.epic, c.caller, []string{"manual"}, "subtree")

	c.feedIssueA = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.status_changed", "issue", c.issueA, true, false, c.issueA)

	commentID := seedCorpusComment(t, pool, c.issueA, c.actor)
	c.feedCommentA = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"comment.created", "comment", commentID.String(), true, false, c.issueA)

	c.feedChild = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.status_changed", "issue", c.child, true, false, c.epic)

	c.feedIssueB = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.status_changed", "issue", c.issueB, true, false, "")

	// A feed row of a type the legacy notify_event_types whitelist below
	// excludes. It must be visible everywhere: the read filter is gone.
	c.feedLegacy = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.closed", "issue", c.issueA, true, false, c.issueA)

	c.feedMention = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.mention", "issue", c.issueA, true, false, "")

	// Minted for push alone: in no feed, on no card, in no counter.
	c.pushOnlyA = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.updated", "issue", c.issueA, false, false, "")

	c.archivedA = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.reparented", "issue", c.issueA, true, true, c.issueA)

	// The legacy whitelist. Present precisely so the corpus proves it is
	// consulted by nobody (T014 / INV-2).
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, notify_event_types)
VALUES ($1, ARRAY['issue.status_changed']::text[])
ON CONFLICT (user_id) DO UPDATE SET notify_event_types = EXCLUDED.notify_event_types`,
		c.caller); err != nil {
		t.Fatalf("seed legacy prefs: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, c.caller)
	})

	return c
}

func seedWatcherScoped(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reasons []string, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope, since)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (issue_id, user_id) DO UPDATE
SET reasons = EXCLUDED.reasons, scope = EXCLUDED.scope`, issueID, userID, reasons, scope); err != nil {
		t.Fatalf("seed watcher: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, userID)
	})
}

func seedCorpusComment(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', 'human', 'corpus comment', now(), now())`,
		id, issueID, author); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, id)
	})
	return id
}

func seedCorpusRow(
	t *testing.T, pool *pgxpool.Pool,
	recipient, actor, projectID uuid.UUID,
	evtType, entityType, entityID string,
	feed, archived bool,
	card string, // the Watching card the US-4 rules give the row; "" = none
) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, $2, $3, $4, 'tester', $5, 'human', '{}'::jsonb)
RETURNING id`, evtType, entityType, entityID, projectID, actor).Scan(&eventID); err != nil {
		t.Fatalf("seed event %s: %v", evtType, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID)
	})
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at,
                           feed_channel, archived_at, card_issue_id, card_resolved)
VALUES ($1, $2, $3, $4, now(), $5,
        CASE WHEN $6 THEN now() ELSE NULL END, NULLIF($7, ''), true)
RETURNING id`, recipient, eventID, evtType, projectID, feed, archived, card).Scan(&id); err != nil {
		t.Fatalf("seed notification %s: %v", evtType, err)
	}
	return id
}

// ---- surface drivers ----

func (c corpus) restInboxIDs(t *testing.T, scope string) []int64 {
	t.Helper()
	h := handler.NewNotificationsHandler(c.pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications?limit=100&scope="+scope, nil)
	req = req.WithContext(auth.WithUserRole(auth.WithUserID(req.Context(), c.caller), "user"))
	rec := httptest.NewRecorder()
	h.ListNotifications(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("REST list scope=%s: status %d", scope, res.StatusCode)
	}
	var body struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode REST list: %v", err)
	}
	out := make([]int64, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, it.ID)
	}
	return sorted(out)
}

func (c corpus) restCountUnread(t *testing.T) int {
	t.Helper()
	h := handler.NewNotificationsHandler(c.pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/count", nil)
	req = req.WithContext(auth.WithUserRole(auth.WithUserID(req.Context(), c.caller), "user"))
	rec := httptest.NewRecorder()
	h.CountNotifications(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	var body struct {
		Unread int `json:"unread"`
		Total  int `json:"total"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	return body.Unread
}

func (c corpus) mcpInboxIDs(t *testing.T) []int64 {
	t.Helper()
	call := newNotificationsCaller(t, c.pool, c.caller)
	body := call(t, map[string]any{"action": "inbox", "scope": "unread", "limit": float64(100)})
	rows, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("mcp inbox items missing: %v", body)
	}
	out := make([]int64, 0, len(rows))
	for _, raw := range rows {
		row := raw.(map[string]any)
		id, ok := row["id"].(float64)
		if !ok {
			t.Fatalf("mcp inbox row without numeric id: %v", row)
		}
		out = append(out, int64(id))
	}
	return sorted(out)
}

func (c corpus) headerCounts(t *testing.T) map[string]int {
	t.Helper()
	h := handler.NewMeHandler(c.pool, issues.NewStore(c.pool), notifications.NewStore(c.pool))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/notification-counts", nil)
	req = req.WithContext(auth.WithUserRole(auth.WithUserID(req.Context(), c.caller), "user"))
	rec := httptest.NewRecorder()
	h.NotificationCounts(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("header counts: status %d", res.StatusCode)
	}
	// Decoded loosely on purpose. The counts response is no longer uniformly
	// numeric: sk-303.14 added `reviewMechanicEnabled` (bool — the ONLY thing
	// that distinguishes "no second number" from "second number is zero") and
	// `delegatedUnreviewedCount` (null when the mechanic is off). Decoding
	// straight into map[string]int made this helper fail on the bool. The
	// numeric projection below keeps every existing assertion intact and is
	// deliberately narrow: this file is about the Watching/Mentions badges,
	// and the review-mechanic contract is asserted where it belongs
	// (internal/handler/review_unreviewed_manifestations_test.go).
	var raw map[string]any
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode header counts: %v", err)
	}
	body := make(map[string]int, len(raw))
	for k, v := range raw {
		if n, ok := v.(float64); ok {
			body[k] = int(n)
		}
	}
	return body
}

func (c corpus) watching(t *testing.T) map[string]any {
	t.Helper()
	h := handler.NewMeHandler(c.pool, issues.NewStore(c.pool), notifications.NewStore(c.pool))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/watching", nil)
	req = req.WithContext(auth.WithUserRole(auth.WithUserID(req.Context(), c.caller), "user"))
	rec := httptest.NewRecorder()
	h.GetWatching(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watching: status %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode watching: %v", err)
	}
	return body
}

// ---- the corpus assertions ----

// TestCrossSurface_SameNotificationSet (row 28): REST, MCP and the
// counters answer with one and the same set of notification ids. The push-only
// row and the archived row are absent from all of them; the row whose type the
// legacy whitelist excluded is present in all of them.
func TestCrossSurface_SameNotificationSet(t *testing.T) {
	pool := newTestPool(t)
	c := newCorpus(t, pool)

	want := c.visibleUnread()

	rest := c.restInboxIDs(t, "unread")
	if !equalIDs(rest, want) {
		t.Errorf("REST scope=unread ids = %v, want %v", rest, want)
	}
	mcp := c.mcpInboxIDs(t)
	if !equalIDs(mcp, want) {
		t.Errorf("MCP inbox ids = %v, want %v", mcp, want)
	}
	if !equalIDs(rest, mcp) {
		t.Errorf("REST and MCP disagree: %v vs %v", rest, mcp)
	}
	if n := c.restCountUnread(t); n != len(want) {
		t.Errorf("REST unread count = %d, want %d — the counter and the list must count one set", n, len(want))
	}

	// Named rows, so a failure says WHICH class leaked rather than only
	// that a set differs.
	got := map[int64]bool{}
	for _, id := range rest {
		got[id] = true
	}
	if got[c.pushOnlyA] {
		t.Errorf("push-only row %d is visible (INV-11)", c.pushOnlyA)
	}
	if got[c.archivedA] {
		t.Errorf("archived row %d is in the unread scope", c.archivedA)
	}
	if !got[c.feedLegacy] {
		t.Errorf("row %d of a whitelist-excluded type is hidden — the reason read-filter is supposed to be gone (INV-2)", c.feedLegacy)
	}

	// The archived scope shows the archived row and STILL hides the
	// push-only one: "archived" is a different question from "feed".
	arch := c.restInboxIDs(t, "archived")
	if !equalIDs(arch, []int64{c.archivedA}) {
		t.Errorf("REST scope=archived ids = %v, want [%d]", arch, c.archivedA)
	}
}

// TestCrossSurface_WatchingAgreesWithBadge (rows 30/31): the Watching
// column, its total and the header badge are three views of one attributed
// subset — exact subscription plus the ancestor's subtree, each row once.
func TestCrossSurface_WatchingAgreesWithBadge(t *testing.T) {
	pool := newTestPool(t)
	c := newCorpus(t, pool)

	wantWatching := c.watchingAttributed()

	body := c.watching(t)
	total := int(body["unreadTotal"].(float64))
	if total != len(wantWatching) {
		t.Errorf("watching unreadTotal = %d, want %d (%v)", total, len(wantWatching), wantWatching)
	}
	if truncated, _ := body["truncated"].(bool); truncated {
		t.Errorf("truncated = true on a 2-subscription fixture")
	}

	sum := 0
	for _, raw := range body["items"].([]any) {
		sum += int(raw.(map[string]any)["unreadEventsCount"].(float64))
	}
	if sum != total {
		t.Errorf("sum of card counts = %d, unreadTotal = %d — the cards must partition the total", sum, total)
	}

	counts := c.headerCounts(t)
	if counts["watching"] != total {
		t.Errorf("header watching badge = %d, /me/watching unreadTotal = %d — INV-3 requires equality or a declared incompleteness",
			counts["watching"], total)
	}
	if counts["mentions"] != 1 {
		t.Errorf("mentions = %d, want 1", counts["mentions"])
	}

	// Two subscriptions, two cards: the descendant's activity is on the
	// ancestor's card and does not spawn one of its own (US-6).
	if n := len(body["items"].([]any)); n != 2 {
		t.Errorf("cards = %d, want 2 (the two subscriptions)", n)
	}
	if got := int(body["subscriptionsTotal"].(float64)); got != 2 {
		t.Errorf("subscriptionsTotal = %d, want 2", got)
	}
}

// TestCrossSurface_WatchingEmptyStatesAreDistinguishable (row 29): with
// the feed channel of every applicable reason off at write time, the caller's
// rows are push-only — the column is empty of activity and the badge counts
// none of it. The response still tells "no subscriptions" apart from
// "subscriptions with no matching events".
func TestCrossSurface_WatchingEmptyStatesAreDistinguishable(t *testing.T) {
	pool := newTestPool(t)

	// State 1 — no subscriptions at all.
	noSubs := corpus{pool: pool}
	noSubs.caller, _ = seedUser(t, pool, "user")
	body := noSubs.watching(t)
	if n := len(body["items"].([]any)); n != 0 {
		t.Errorf("no-subscriptions: items = %d, want 0", n)
	}
	if got := int(body["subscriptionsTotal"].(float64)); got != 0 {
		t.Errorf("no-subscriptions: subscriptionsTotal = %d, want 0", got)
	}

	// State 2 — subscriptions exist, every row for them was minted with the
	// feed channel off, so nothing matches.
	c := corpus{pool: pool}
	c.caller, _ = seedUser(t, pool, "user")
	c.actor, _ = seedUser(t, pool, "user")
	c.project, c.prefix = seedProject(t, pool, c.caller, "xse")
	c.issueA = seedIssue(t, pool, c.project, c.actor, c.prefix+"-1", "watched", "open", 2)
	seedWatcherScoped(t, pool, c.issueA, c.caller, []string{"manual"}, "issue")
	// Push-only: the fan-out gives a card to feed rows only (US-4).
	c.pushOnlyA = seedCorpusRow(t, pool, c.caller, c.actor, c.project,
		"issue.status_changed", "issue", c.issueA, false, false, "")

	body = c.watching(t)
	if got := int(body["subscriptionsTotal"].(float64)); got != 1 {
		t.Fatalf("channels-off: subscriptionsTotal = %d, want 1 — the subscription must still be listed", got)
	}
	if n := len(body["items"].([]any)); n != 1 {
		t.Fatalf("channels-off: items = %d, want 1", n)
	}
	if got := int(body["unreadTotal"].(float64)); got != 0 {
		t.Errorf("channels-off: unreadTotal = %d, want 0 — a push-only row is on no card (INV-11)", got)
	}
	card := body["items"].([]any)[0].(map[string]any)
	if got := int(card["unreadEventsCount"].(float64)); got != 0 {
		t.Errorf("channels-off: card count = %d, want 0", got)
	}
	if got := len(card["recentEvents"].([]any)); got != 0 {
		t.Errorf("channels-off: recentEvents = %d, want 0", got)
	}
	if got := c.headerCounts(t)["watching"]; got != 0 {
		t.Errorf("channels-off: badge = %d, want 0", got)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

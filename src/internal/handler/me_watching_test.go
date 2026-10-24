package handler

// sk-39 — integration tests for GET /api/v1/me/watching. Mirrors the
// fixture shape of me_test.go (newMeTestPool / seedMeUser / etc.) so
// the two surfaces share helpers without duplicating Postgres
// boilerplate.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
)

// seedWatcher inserts a live issue_watchers row directly with the given
// reasons[].
func seedWatcher(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reasons []string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, $3, now())
ON CONFLICT (issue_id, user_id) DO UPDATE
SET reasons = EXCLUDED.reasons`, issueID, userID, reasons); err != nil {
		t.Fatalf("seed watcher: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
			issueID, userID)
	})
}

// newMeWatchingHandler wires a MeHandler with real stores for the
// watching feed path (nil-stores would 500 in the actual handler).
func newMeWatchingHandler(pool *pgxpool.Pool) *MeHandler {
	return NewMeHandler(pool, issues.NewStore(pool), notifications.NewStore(pool))
}

// callWatching builds an httptest GET and decodes the JSON body. Uses
// the same auth context plumbing as callCounts.
func callWatching(t *testing.T, h *MeHandler, userID uuid.UUID) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/watching", nil)
	if userID != uuid.Nil {
		ctx := req.Context()
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, "user")
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	h.GetWatching(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// TestGetWatching_RequiresAuth: anonymous → 401.
func TestGetWatching_RequiresAuth(t *testing.T) {
	pool := newMeTestPool(t)
	h := newMeWatchingHandler(pool)
	res, _ := callWatching(t, h, uuid.Nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// TestGetWatching_EmptyWhenNoSubscriptions: a fresh user gets
// items: [].
func TestGetWatching_EmptyWhenNoSubscriptions(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	h := newMeWatchingHandler(pool)

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing/not array; body=%v", body)
	}
	if len(items) != 0 {
		t.Fatalf("items len = %d, want 0", len(items))
	}
}

// TestGetWatching_ReasonsAreOrderedAndSticky: two watched issues with
// various reason combos; check reasons come out in render order
// (manual > assignee > creator > mention > commenter). Since sk-622.21 an
// explicit unwatch deletes the subscription and records an opt-out, so
// there is no tombstone row to exclude (R2 forbids it by CHECK).
func TestGetWatching_ReasonsAreOrderedAndSticky(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	// Issue A: reasons=['creator','manual'] — should render manual,creator.
	seedMeIssue(t, pool, pid, user, prefix+"-1", "", nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"creator", "manual"})

	// Issue B: reasons=['assignee','commenter','mention'] — assignee,mention,commenter.
	seedMeIssue(t, pool, pid, user, prefix+"-2", "", nil)
	seedWatcher(t, pool, prefix+"-2", user, []string{"assignee", "commenter", "mention"})

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing/not array; body=%v", body)
	}
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}

	// Build issueId → reasons map for assertion.
	byID := make(map[string][]string, 2)
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("row not object: %v", raw)
		}
		item, ok := row["item"].(map[string]any)
		if !ok {
			t.Fatalf("item missing: %v", row)
		}
		id, _ := item["id"].(string)
		reasonsRaw, _ := row["reasons"].([]any)
		reasons := make([]string, 0, len(reasonsRaw))
		for _, r := range reasonsRaw {
			s, _ := r.(string)
			reasons = append(reasons, s)
		}
		byID[id] = reasons
	}

	wantA := []string{"manual", "creator"}
	gotA := byID[prefix+"-1"]
	if !equalStringSlice(gotA, wantA) {
		t.Fatalf("issue A reasons = %v, want %v", gotA, wantA)
	}
	wantB := []string{"assignee", "mention", "commenter"}
	gotB := byID[prefix+"-2"]
	if !equalStringSlice(gotB, wantB) {
		t.Fatalf("issue B reasons = %v, want %v", gotB, wantB)
	}
}

// TestGetWatching_UnreadCountMatchesNotifications: the
// unreadEventsCount field tracks the caller's unread non-mention
// notifications on the watched issue exactly.
func TestGetWatching_UnreadCountMatchesNotifications(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	// One watched issue, two unread issue-events, one mention (excluded
	// from count), one already-read event (excluded).
	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"manual"})

	// Rows written to the subscriber carry the issue as their Watching card
	// (sk-622.21 US-4); a mention never gets one.
	ev1 := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev1, "issue.status_changed", pid, prefix+"-1")
	ev2 := seedMeEvent(t, pool, pid, "issue.updated", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev2, "issue.updated", pid, prefix+"-1")

	// Mention — same issue but excluded from the unread bucket.
	evM := seedMeEvent(t, pool, pid, "issue.mention", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, evM, "issue.mention", pid)

	// Already-read event — also excluded.
	ev3 := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev3, "issue.status_changed", pid, prefix+"-1")
	if _, err := pool.Exec(t.Context(),
		`UPDATE notifications SET read_at = now() WHERE event_id = $1`, ev3); err != nil {
		t.Fatalf("set read: %v", err)
	}

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %v", items)
	}
	row := items[0].(map[string]any)
	count, ok := row["unreadEventsCount"].(float64)
	if !ok {
		t.Fatalf("unreadEventsCount not numeric: %v", row["unreadEventsCount"])
	}
	if int(count) != 2 {
		t.Fatalf("unreadEventsCount = %d, want 2 (mention + read excluded)", int(count))
	}
}

// TestGetWatching_UnreadCountIncludesCommentEvents (sk-24): a comment
// event on a watched issue must bump the per-card unreadEventsCount.
// Before sk-24 the unread_counts CTE only counted entity_type='issue'
// events, so a flood of comments on a watched issue showed no badge —
// inconsistent with the header bell which counts both via the
// COALESCE(issue, comment->issue) shape.
func TestGetWatching_UnreadCountIncludesCommentEvents(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"manual"})

	// One issue-event + one comment-event on the same watched issue.
	ev1 := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev1, "issue.status_changed", pid, prefix+"-1")

	// Seed a comment row pointing at the watched issue; the event's
	// entity_id must be the comment's UUID for the COALESCE join to
	// resolve back to prefix+"-1".
	commentID := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(t.Context(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', 'human', 'a comment', now(), now())`,
		commentID, prefix+"-1", other); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentID)
	})
	ev2 := seedMeEvent(t, pool, pid, "comment.created", "comment", commentID.String(), other)
	// A comment's task is the comment's issue (US-4), so its card is the
	// watched issue.
	seedMeCardNotification(t, pool, user, ev2, "comment.created", pid, prefix+"-1")

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	row := body["items"].([]any)[0].(map[string]any)
	count := int(row["unreadEventsCount"].(float64))
	if count != 2 {
		t.Fatalf("unreadEventsCount = %d, want 2 (issue event + comment event)", count)
	}
}

// TestGetWatching_UnreadCountIncludesSelfCreatedWatch (sk-24): per
// user's choice, Watching is "stuff on my subscriptions" — events on a
// self-created issue I'm subscribed to (creator-auto-watch is the
// canonical path) count toward the unreadEventsCount. Until sk-303.13 /
// T017 they were also counted by the header's `outbox` bucket; that
// counter is gone, so this is now the only place they appear.
func TestGetWatching_UnreadCountIncludesSelfCreatedWatch(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	// Caller authored the issue, delegated to `other`, auto-watches as creator.
	seedMeIssue(t, pool, pid, user, prefix+"-1", otherName, nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"creator"})
	ev := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev, "issue.status_changed", pid, prefix+"-1")

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	row := body["items"].([]any)[0].(map[string]any)
	count := int(row["unreadEventsCount"].(float64))
	if count != 1 {
		t.Fatalf("unreadEventsCount = %d, want 1 (self-created watched, sk-24 overlap)", count)
	}
}

// TestGetWatching_RecentEventsOrderedAndTrimmed: recent rows appear
// newest-first and the per-card cap is enforced.
//
// sk-303.13 / T015 — the timeline is built from the caller's own FEED
// notifications, not from the raw events table, so the fixture seeds a
// notification per event and backdates the NOTIFICATION's created_at
// (which is what the card, the inbox and the sort all read).
func TestGetWatching_RecentEventsOrderedAndTrimmed(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"manual"})

	// Seed 7 notified issue events; only top 5 (newest) should land in
	// recentEvents per the WatchingRecentTopN cap.
	for i := 0; i < 7; i++ {
		ev := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
		seedMeCardNotification(t, pool, user, ev, "issue.status_changed", pid, prefix+"-1")
		// Stagger created_at so the ORDER BY has a deterministic answer
		// (the seeds use now(), identical for rapid INSERTs within one
		// statement-timestamp).
		offset := time.Duration(i) * time.Second
		if _, err := pool.Exec(t.Context(),
			`UPDATE notifications SET created_at = now() - $2::interval WHERE event_id = $1`,
			ev, offset); err != nil {
			t.Fatalf("backdate notification: %v", err)
		}
	}

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	row := items[0].(map[string]any)
	recent, ok := row["recentEvents"].([]any)
	if !ok {
		t.Fatalf("recentEvents missing: %v", row)
	}
	if len(recent) != notifications.WatchingRecentTopN {
		t.Fatalf("recentEvents len = %d, want %d (top-N cap)",
			len(recent), notifications.WatchingRecentTopN)
	}

	// Confirm DESC ordering on createdAt.
	var prev time.Time
	for i, raw := range recent {
		ev := raw.(map[string]any)
		tsStr, _ := ev["createdAt"].(string)
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			t.Fatalf("bad createdAt: %v", err)
		}
		if i > 0 && ts.After(prev) {
			t.Fatalf("recentEvents not DESC: at idx %d, %v > prev %v", i, ts, prev)
		}
		prev = ts
	}
}

func equalStringSlice(a, b []string) bool {
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

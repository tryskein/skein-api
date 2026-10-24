package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// ---------------- helpers ----------------

func newMeTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedMeUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "mh" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(t.Context(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func seedMeProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	idPrefix := "mh" + suffix[:6]
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, idPrefix+"_"+suffix, idPrefix, owner, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner); err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, idPrefix
}

// seedMeIssue inserts a minimal open issue. acceptedAt nil keeps
// accepted_at NULL so the row stays in the Inbox surface.
func seedMeIssue(t *testing.T, pool *pgxpool.Pool, pid, creator uuid.UUID, idStr, assignee string, acceptedAt *time.Time) {
	t.Helper()
	var assigneeArg, acceptedAtArg, acceptedByArg any
	if assignee != "" {
		assigneeArg = assignee
	}
	if acceptedAt != nil {
		acceptedAtArg = *acceptedAt
		acceptedByArg = creator
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, 'open', false, 'human', NULL, $3::varchar,
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL, NULL, $4, now(), now(), $5::timestamptz, $6::uuid, '{}'::jsonb)`,
		idStr, pid, assigneeArg, creator, acceptedAtArg, acceptedByArg); err != nil {
		t.Fatalf("seed me-issue: %v", err)
	}
}

// seedMeEvent writes one events row and returns its id. The Counts
// query joins notifications → events → issues, so the test fixtures
// must produce a real event before they can attach a notification.
func seedMeEvent(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, eventType, entityType, entityID string, actorUserID uuid.UUID) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id)
VALUES ($1, 'tester', $2, 'human', $3, $4, $5)
RETURNING id`,
		eventType, actorUserID, entityType, entityID, projectID).Scan(&id); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// seedMeNotification inserts a notifications row tied to the given
// event. read_at + archived_at are left NULL so the row counts as
// unread for Counts.
func seedMeNotification(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, eventID int64, ntype string, projectID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel)
VALUES ($1, $2, $3, $4, true)`, userID, eventID, ntype, projectID); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
}

// seedMeCardNotification inserts an unread feed notifications row tied to
// the given event and carrying the Watching card `card` — the row the
// fan-out writes for a recipient who, at write time, watches the event's
// task (card = that task) or an ancestor of it with its subtree (card = the
// nearest such ancestor), sk-622.21 US-4. Since R2 (T022) the Watching
// badge and column read nothing but this card, so a fixture that means "a
// notification on my subscription" has to write it; seedMeNotification
// stays the row of a notification that reached the caller by any other
// reason (no card).
func seedMeCardNotification(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, eventID int64, ntype string, projectID uuid.UUID, card string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, $3, $4, true, $5, true)`, userID, eventID, ntype, projectID, card); err != nil {
		t.Fatalf("seed carded notification: %v", err)
	}
}

// seedMePushOnlyNotification inserts a notifications row that belongs to
// the PUSH channel only (feed_channel = false) — the third of the four
// channel states. No read surface may show or count it (INV-11).
func seedMePushOnlyNotification(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, eventID int64, ntype string, projectID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel)
VALUES ($1, $2, $3, $4, false)`, userID, eventID, ntype, projectID); err != nil {
		t.Fatalf("seed push-only notification: %v", err)
	}
}

// callCounts builds an httptest GET against the Me handler and decodes
// the JSON body. userID == uuid.Nil signals "no auth" — the handler
// should respond 401.
func callCounts(t *testing.T, h *MeHandler, userID uuid.UUID) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/notification-counts", nil)
	if userID != uuid.Nil {
		ctx := req.Context()
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, "user")
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	h.NotificationCounts(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
	} else {
		_ = json.NewDecoder(res.Body).Decode(&body)
	}
	return res, body
}

func mustInt(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in body: %v", key, body)
	}
	f, ok := raw.(float64)
	if !ok {
		t.Fatalf("key %q not numeric: %v (%T)", key, raw, raw)
	}
	return int(f)
}

// ---------------- tests ----------------

// TestNotificationCounts_AllZero: a fresh user with no issues and no
// notifications gets zeros — and no `outbox` key at all (sk-303.13 /
// T017: the counter is gone from the contract, not zeroed).
func TestNotificationCounts_AllZero(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	h := NewMeHandler(pool, nil, nil)

	res, body := callCounts(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	if got := mustInt(t, body, "inbox"); got != 0 {
		t.Fatalf("inbox = %d, want 0", got)
	}
	if _, present := body["outbox"]; present {
		t.Fatalf("counts response still carries an `outbox` field: %v", body)
	}
	if got := mustInt(t, body, "mentions"); got != 0 {
		t.Fatalf("mentions = %d, want 0", got)
	}
	if got := mustInt(t, body, "watching"); got != 0 {
		t.Fatalf("watching = %d, want 0", got)
	}
}

// TestNotificationCounts_InboxFlipsOnAccept: an open, unaccepted issue
// assigned to the caller BY SOMEONE ELSE increments inbox by one;
// flipping accepted_at brings it back to zero.
//
// The "by someone else" anchor matters — sk-23 added a `created_by !=
// caller` clause to countInbox so self-assigned tracking tasks don't
// inflate the badge. Using `other` as the creator keeps this test
// honest about what Inbox means.
func TestNotificationCounts_InboxFlipsOnAccept(t *testing.T) {
	pool := newMeTestPool(t)
	user, uname := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	seedMeIssue(t, pool, pid, other, prefix+"-1", uname, nil)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "inbox"); got != 1 {
		t.Fatalf("inbox before accept = %d, want 1", got)
	}

	// Flip accepted_at — issue leaves the inbox.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET accepted_at = now(), accepted_by = $2 WHERE id = $1`,
		prefix+"-1", user); err != nil {
		t.Fatalf("update accepted_at: %v", err)
	}
	_, body = callCounts(t, h, user)
	if got := mustInt(t, body, "inbox"); got != 0 {
		t.Fatalf("inbox after accept = %d, want 0", got)
	}
}

// TestNotificationCounts_InboxSkipsSelfCreated: an open, unaccepted
// issue the caller created AND assigned to themselves must NOT count
// toward inbox — Inbox semantically means "from others" (sk-23). This
// guards the symmetry the frontend `useInbox` hook expects: only
// issues delegated TO the caller by someone else show up.
func TestNotificationCounts_InboxSkipsSelfCreated(t *testing.T) {
	pool := newMeTestPool(t)
	user, uname := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// Self-created, self-assigned, unaccepted, open.
	seedMeIssue(t, pool, pid, user, prefix+"-1", uname, nil)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "inbox"); got != 0 {
		t.Fatalf("inbox for self-created self-assigned = %d, want 0 (sk-23)", got)
	}
}

// TestNotificationCounts_AuthoredIssueCountsNowhere (sk-303.13 / T017):
// authorship of the underlying issue no longer feeds any counter. Before
// T017 this exact fixture incremented `outbox`; now the only rule left is
// "there is a subscription", and the caller has none here, so the event lands in no
// badge at all.
func TestNotificationCounts_AuthoredIssueCountsNowhere(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// Issue created by the caller; another user comments on it (we
	// just write a status_changed event to keep the fixture simple —
	// the bucket only cares about notifications.type and the issue's
	// created_by).
	seedMeIssue(t, pool, pid, user, prefix+"-1", "", nil)
	eventID := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, eventID, "issue.status_changed", pid)

	_, body := callCounts(t, h, user)
	if _, present := body["outbox"]; present {
		t.Fatalf("counts response still carries an `outbox` field: %v", body)
	}
	if got := mustInt(t, body, "watching"); got != 0 {
		t.Fatalf("watching = %d, want 0 (event is on my own issue, and I am not subscribed)", got)
	}
	if got := mustInt(t, body, "mentions"); got != 0 {
		t.Fatalf("mentions = %d, want 0", got)
	}
}

// TestNotificationCounts_BucketsCountFeedRowsOnly (sk-303.13 / T014): the
// header buckets select on notifications.feed_channel and nothing else.
//
// Two halves, and they are the inverse of the sk-83 test this replaces:
//
//  1. a notify_event_types whitelist that EXCLUDES a row's type no longer
//     hides it — reason filtering moved to write time (INV-2), so the old
//     read-side whitelist is gone from every surface;
//  2. a push-only row (feed_channel=false) is counted by NO bucket — it
//     belongs to no feed and must not move a feed counter (INV-11).
func TestNotificationCounts_BucketsCountFeedRowsOnly(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// Issue authored by caller, delegated to `other`, and SUBSCRIBED to by
	// the caller — subscription is what a counter reads now (T017). Two
	// unread non-mention FEED rows on it.
	seedMeIssue(t, pool, pid, user, prefix+"-1", otherName, nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"creator"})
	ev1 := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev1, "issue.status_changed", pid, prefix+"-1")
	ev2 := seedMeEvent(t, pool, pid, "issue.closed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, ev2, "issue.closed", pid, prefix+"-1")

	// A third row on the same issue, minted for push alone.
	ev3 := seedMeEvent(t, pool, pid, "issue.updated", "issue", prefix+"-1", other)
	seedMePushOnlyNotification(t, pool, user, ev3, "issue.updated", pid)

	// A mention row minted for push alone — the mentions bucket must not
	// see it either.
	ev4 := seedMeEvent(t, pool, pid, "comment.mention", "issue", prefix+"-1", other)
	seedMePushOnlyNotification(t, pool, user, ev4, "comment.mention", pid)

	// Legacy whitelist excluding `issue.closed`. After T014 it filters
	// nothing.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, notify_event_types)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET notify_event_types = EXCLUDED.notify_event_types`,
		user, []string{"issue.status_changed"}); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, user)
	})

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "watching"); got != 2 {
		t.Fatalf("watching = %d, want 2 (both feed rows count; the whitelist no longer filters and the push-only row does not count); body=%v", got, body)
	}
	if got := mustInt(t, body, "mentions"); got != 0 {
		t.Fatalf("mentions = %d, want 0 (the only mention row is push-only); body=%v", got, body)
	}
}

// TestNotificationCounts_MentionsBucket: mention notifications land in
// the mentions counter regardless of who authored the underlying issue.
func TestNotificationCounts_MentionsBucket(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// Issue authored by `other`; `user` is mentioned in a comment.
	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	eventID := seedMeEvent(t, pool, pid, "issue.mention", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, eventID, "issue.mention", pid)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "mentions"); got != 1 {
		t.Fatalf("mentions = %d, want 1; body=%v", got, body)
	}
	// Mention is not double-counted into watching.
	if got := mustInt(t, body, "watching"); got != 0 {
		t.Fatalf("watching = %d, want 0", got)
	}
}

// TestNotificationCounts_WatchingBucket: a non-mention notification on
// an issue the caller has an active subscription to lands in watching
// (sk-24 — bucket is now subscription-driven, not "non-self-authored").
func TestNotificationCounts_WatchingBucket(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	// A notification on the task that reached the caller before they
	// subscribed (by another reason) carries no card and stays out of
	// Watching: the subscription does not pull it in (sk-622.21 US-6).
	earlier := seedMeEvent(t, pool, pid, "issue.updated", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, earlier, "issue.updated", pid)
	seedWatcher(t, pool, prefix+"-1", user, []string{"manual"})
	eventID := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, eventID, "issue.status_changed", pid, prefix+"-1")

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "watching"); got != 1 {
		t.Fatalf("watching = %d, want 1 — the carded row only, not the row written before the subscription", got)
	}
}

// TestNotificationCounts_WatchingRequiresSubscription: a non-mention
// notification on an issue the caller does NOT have an explicit
// subscription for falls into no bucket (sk-24). The bucket is now
// subscription-driven, not "non-self-authored" — notifications that
// reached the user via project_watchers / custom_watchers without
// landing in issue_watchers no longer pollute the header count.
func TestNotificationCounts_WatchingRequiresSubscription(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// Issue authored by `other`; user receives a notification (e.g. via
	// project_watcher) but is NOT in issue_watchers for this issue.
	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	eventID := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, eventID, "issue.status_changed", pid)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "watching"); got != 0 {
		t.Fatalf("watching = %d, want 0 (no issue_watchers subscription — sk-24)", got)
	}
}

// TestNotificationCounts_WatchingCountsCreatorWatch: a notification on an
// issue the caller authored, delegated AND is subscribed to
// (creator-auto-watch is the typical path) counts in watching.
//
// This is the case US-9 has in mind when it says Watching becomes the
// place a delegator sees activity on the tasks they filed: the rule is
// "there is a subscription", and creator-watch IS a subscription. Until T017 the
// same row was ALSO counted by `outbox`, and the double count was an
// accepted tradeoff; with that counter gone there is nothing left to
// overlap with.
func TestNotificationCounts_WatchingCountsCreatorWatch(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// Authored by caller, delegated to `other`, caller auto-watches as creator.
	seedMeIssue(t, pool, pid, user, prefix+"-1", otherName, nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"creator"})
	eventID := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeCardNotification(t, pool, user, eventID, "issue.status_changed", pid, prefix+"-1")

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "watching"); got != 1 {
		t.Fatalf("watching = %d, want 1 (caller subscribed via creator-watch)", got)
	}
	if _, present := body["outbox"]; present {
		t.Fatalf("counts response still carries an `outbox` field: %v", body)
	}
}

// setMeIssueStatus flips an issue's status. When moving to 'closed' it
// also stamps closed_at + closed_reason to satisfy the
// issues_closed_consistency CHECK; any non-closed status clears them.
// Used by the lens-count tests to exercise the non-closed exclusion
// without a second seed helper.
func setMeIssueStatus(t *testing.T, pool *pgxpool.Pool, idStr, status string) {
	t.Helper()
	const sql = `
UPDATE issues
   SET status = $2,
       closed_at     = CASE WHEN $2 = 'closed' THEN now()    ELSE NULL END,
       closed_reason = CASE WHEN $2 = 'closed' THEN 'test'   ELSE NULL END
 WHERE id = $1`
	if _, err := pool.Exec(t.Context(), sql, idStr, status); err != nil {
		t.Fatalf("set issue status: %v", err)
	}
}

// callCountsAdmin is like callCounts but stamps the caller role as
// "admin" so the visibility-gated lens counts bypass the
// project-membership filter.
func callCountsAdmin(t *testing.T, h *MeHandler, userID uuid.UUID) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/notification-counts", nil)
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, "admin")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.NotificationCounts(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return res, body
}

// TestNotificationCounts_OnMeLens exercises the global "On me" badge
// (sk-54): non-closed issues assigned to the caller by someone else that
// the caller has accepted. Asserts the predicate edges:
//   - counts an open, accepted, other-created, assigned-to-me issue;
//   - counts an in_progress + a deferred one (non-closed = all three);
//   - EXCLUDES a closed one (closed-exclusion);
//   - EXCLUDES an unaccepted one (accepted_at IS NOT NULL);
//   - EXCLUDES a self-created one (created_by <> me);
//   - EXCLUDES one assigned to someone else.
func TestNotificationCounts_OnMeLens(t *testing.T) {
	pool := newMeTestPool(t)
	user, uname := seedMeUser(t, pool, "user")
	other, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	// Membership: caller must see the project so the visibility gate
	// admits the rows (non-admin path). seedMeProject already adds the
	// owner (caller) as a member.
	h := NewMeHandler(pool, nil, nil)

	accepted := time.Now()

	// COUNTED: open, accepted, created by other, assigned to me.
	seedMeIssue(t, pool, pid, other, prefix+"-1", uname, &accepted)
	// COUNTED: in_progress, accepted, other-created, assigned to me.
	seedMeIssue(t, pool, pid, other, prefix+"-2", uname, &accepted)
	setMeIssueStatus(t, pool, prefix+"-2", "in_progress")
	// COUNTED: deferred, accepted, other-created, assigned to me.
	seedMeIssue(t, pool, pid, other, prefix+"-3", uname, &accepted)
	setMeIssueStatus(t, pool, prefix+"-3", "deferred")
	// EXCLUDED: closed.
	seedMeIssue(t, pool, pid, other, prefix+"-4", uname, &accepted)
	setMeIssueStatus(t, pool, prefix+"-4", "closed")
	// EXCLUDED: unaccepted (accepted_at IS NULL) — that's Inbox, not On me.
	seedMeIssue(t, pool, pid, other, prefix+"-5", uname, nil)
	// EXCLUDED: self-created.
	seedMeIssue(t, pool, pid, user, prefix+"-6", uname, &accepted)
	// EXCLUDED: assigned to someone else.
	seedMeIssue(t, pool, pid, other, prefix+"-7", otherName, &accepted)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "onMeOpenCount"); got != 3 {
		t.Fatalf("onMeOpenCount = %d, want 3 (open+in_progress+deferred, closed/unaccepted/self/other excluded)", got)
	}
	// Delegated must stay at 0 — none of these match the Delegated lens
	// (the self-created one is assigned to me, so it's self-work, not
	// delegated).
	if got := mustInt(t, body, "delegatedOpenCount"); got != 0 {
		t.Fatalf("delegatedOpenCount = %d, want 0", got)
	}
}

// TestNotificationCounts_DelegatedLens exercises the global "Delegated"
// badge (sk-54): non-closed issues the caller created (or was previously
// assigned, not dismissed) and that are NOT currently on the caller's
// plate (assignee <> me). Asserts:
//   - counts a created-by-me, assigned-to-other, open issue;
//   - counts in_progress + deferred ones (non-closed = all three);
//   - EXCLUDES a closed one (closed-exclusion);
//   - EXCLUDES a created-by-me, assigned-to-ME issue (self-work);
//   - EXCLUDES a created-by-me, unassigned issue (assignee IS NOT NULL).
func TestNotificationCounts_DelegatedLens(t *testing.T) {
	pool := newMeTestPool(t)
	user, uname := seedMeUser(t, pool, "user")
	_, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// COUNTED: created by me, assigned to other, open.
	seedMeIssue(t, pool, pid, user, prefix+"-1", otherName, nil)
	// COUNTED: created by me, assigned to other, in_progress.
	seedMeIssue(t, pool, pid, user, prefix+"-2", otherName, nil)
	setMeIssueStatus(t, pool, prefix+"-2", "in_progress")
	// COUNTED: created by me, assigned to other, deferred.
	seedMeIssue(t, pool, pid, user, prefix+"-3", otherName, nil)
	setMeIssueStatus(t, pool, prefix+"-3", "deferred")
	// EXCLUDED: closed.
	seedMeIssue(t, pool, pid, user, prefix+"-4", otherName, nil)
	setMeIssueStatus(t, pool, prefix+"-4", "closed")
	// EXCLUDED: created by me but assigned to ME (self-work, not delegated).
	seedMeIssue(t, pool, pid, user, prefix+"-5", uname, nil)
	// EXCLUDED: created by me but unassigned (assignee IS NULL).
	seedMeIssue(t, pool, pid, user, prefix+"-6", "", nil)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "delegatedOpenCount"); got != 3 {
		t.Fatalf("delegatedOpenCount = %d, want 3 (open+in_progress+deferred, closed/self/unassigned excluded)", got)
	}
	// On me must stay at 0 — none of these are assigned to me AND
	// accepted-by-someone-else.
	if got := mustInt(t, body, "onMeOpenCount"); got != 0 {
		t.Fatalf("onMeOpenCount = %d, want 0", got)
	}
}

// TestNotificationCounts_LensVisibilityGate verifies the lens counts
// honour the caller's visible-project horizon for non-admins (matching
// the cross-project tab list) while admins see across every project.
//
// Setup: a project the caller is NOT a member of, holding a Delegated
// row created by the caller. A non-admin caller must NOT see it (count
// 0); an admin caller must (count 1).
func TestNotificationCounts_LensVisibilityGate(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	owner, _ := seedMeUser(t, pool, "user")
	_, otherName := seedMeUser(t, pool, "user")
	// Project owned by `owner`; caller `user` is NOT a member.
	pid, prefix := seedMeProject(t, pool, owner)
	h := NewMeHandler(pool, nil, nil)

	// Delegated row: created by caller, assigned to someone else, open.
	seedMeIssue(t, pool, pid, user, prefix+"-1", otherName, nil)

	// Non-admin caller can't see the project → not counted.
	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "delegatedOpenCount"); got != 0 {
		t.Fatalf("delegatedOpenCount (non-member) = %d, want 0 (visibility-gated)", got)
	}

	// Admin caller bypasses the membership gate → counted. The admin's
	// reach is the access relation's, which reads users.role (sk-674.18
	// T010, D-2): the role on the context alone is not a grant.
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, user); err != nil {
		t.Fatalf("promote caller to admin: %v", err)
	}
	_, adminBody := callCountsAdmin(t, h, user)
	if got := mustInt(t, adminBody, "delegatedOpenCount"); got != 1 {
		t.Fatalf("delegatedOpenCount (admin) = %d, want 1 (admin sees all projects)", got)
	}
}

// TestNotificationCounts_RequiresAuth: anonymous → 401.
func TestNotificationCounts_RequiresAuth(t *testing.T) {
	pool := newMeTestPool(t)
	h := NewMeHandler(pool, nil, nil)
	res, _ := callCounts(t, h, uuid.Nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// TestNotificationCounts_InboxCountsPendingInvitationsToMe — row 129
// (sk-674.18 T029, US-13: the Inbox badge is the unaccepted issues plus the
// pending invitations to me). Reference — US-13 as the spec words it,
// written before the code: an invitation counts when it is pending, not
// expired, addressed to the caller's e-mail (whatever its case) and to a
// space he is not in yet. Every other invitation — to another person, an
// expired one, a declined, revoked or accepted one, one to a space he
// already participates in — counts nowhere.
//
// Red if the caller's invitations are not counted or another person's (or
// a finished one) are.
func TestNotificationCounts_InboxCountsPendingInvitationsToMe(t *testing.T) {
	pool := newMeTestPool(t)
	ctx := context.Background()
	user, uname := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	inviter, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, other)
	// One unaccepted issue to me in a project I can access (INV-11: only
	// issues accessible now count).
	testsupport.AddProjectMember(t, pool, pid, user, "member")
	seedMeIssue(t, pool, pid, other, prefix+"-1", uname, nil)

	emailOf := func(id uuid.UUID) string {
		var e string
		if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, id).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	mine, theirs := emailOf(user), emailOf(other)
	var invitations []uuid.UUID
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = ANY($1)`, invitations)
	})
	invite := func(space uuid.UUID, email, status, expires string) {
		t.Helper()
		resolved := "NULL"
		if status != "pending" {
			resolved = "now()"
		}
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, status, lang, expires_at, resolved_at)
VALUES ($1, lower(btrim($2)), $3, $4, 'ru', now() + $5::interval, `+resolved+`) RETURNING id`,
			space, email, inviter, status, expires).Scan(&id); err != nil {
			t.Fatalf("seed invitation (%s, %s): %v", status, expires, err)
		}
		invitations = append(invitations, id)
	}

	counted1 := testsupport.SeedSpace(t, pool, inviter)
	counted2 := testsupport.SeedSpace(t, pool, inviter)
	invite(counted1, strings.ToUpper(mine), "pending", "7 days") // to me, typed in another case
	invite(counted2, mine, "pending", "7 days")

	notCounted := testsupport.SeedSpace(t, pool, inviter)
	invite(notCounted, theirs, "pending", "7 days")                             // to another person
	invite(testsupport.SeedSpace(t, pool, inviter), mine, "pending", "-1 hour") // expired
	invite(testsupport.SeedSpace(t, pool, inviter), mine, "declined", "7 days")
	invite(testsupport.SeedSpace(t, pool, inviter), mine, "revoked", "7 days")
	invite(testsupport.SeedSpace(t, pool, inviter), mine, "accepted", "7 days")
	already := testsupport.SeedSpace(t, pool, inviter)
	if _, err := pool.Exec(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')`, already, user); err != nil {
		t.Fatalf("make him a participant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, already, user)
	})
	invite(already, mine, "pending", "7 days") // to a space he is already in

	h := NewMeHandler(pool, nil, nil)
	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "inbox"); got != 1+2 {
		t.Errorf("inbox = %d, want 3 (1 unaccepted issue + 2 pending invitations to me)", got)
	}
	// The other person sees his own invitation and none of mine.
	_, body = callCounts(t, h, other)
	if got := mustInt(t, body, "inbox"); got != 1 {
		t.Errorf("the other person's inbox = %d, want 1 (his one invitation)", got)
	}
}

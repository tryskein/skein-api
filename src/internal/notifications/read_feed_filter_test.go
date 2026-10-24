package notifications

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedReadFilterNotification inserts one events row plus one notifications
// row for the recipient and returns the notification id. feed says whether
// the row belongs to the feed channel; a false one is the "push-only"
// third channel state.
func seedReadFilterNotification(
	t *testing.T, pool *pgxpool.Pool,
	recipient, actor, projectID uuid.UUID,
	issueID, evtType string, feed bool,
) int64 {
	t.Helper()
	ctx := context.Background()
	var eid int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, 'issue', $2, $3, 'tester', $4, 'human', '{}'::jsonb)
RETURNING id`, evtType, issueID, projectID, actor).Scan(&eid); err != nil {
		t.Fatalf("seed event %s: %v", evtType, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eid)
	})
	var nid int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, $3, $4, now(), $5)
RETURNING id`, recipient, eid, evtType, projectID, feed).Scan(&nid); err != nil {
		t.Fatalf("seed notification %s: %v", evtType, err)
	}
	return nid
}

// TestReadSurfacesSelectFeedChannelOnly is the T014 smoke over the store's
// read surfaces: the reason whitelist no longer filters anything, and
// feed_channel is what every surface selects on.
//
// The fixture holds three rows for one caller on one issue:
//
//	feed row of an ALLOWED type       → visible everywhere
//	feed row of a WHITELIST-EXCLUDED  → visible everywhere (T014: the
//	                                    notify_event_types read filter is
//	                                    gone; INV-2)
//	push-only row (feed_channel=false)→ visible nowhere (INV-11)
//
// Both halves matter and they pull in opposite directions: dropping the
// whitelist alone would leak push-only rows, and keeping the whitelist
// would keep the per-surface reason filter INV-2 forbids.
func TestReadSurfacesSelectFeedChannelOnly(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, caller)

	issueID := "rff-1"
	seedTestIssue(t, pool, projectID, caller, issueID, 2)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID)
	})

	nAllowed := seedReadFilterNotification(t, pool, caller, actor, projectID, issueID, "issue.status_changed", true)
	nExcluded := seedReadFilterNotification(t, pool, caller, actor, projectID, issueID, "issue.closed", true)
	nPushOnly := seedReadFilterNotification(t, pool, caller, actor, projectID, issueID, "issue.updated", false)

	// Legacy whitelist that excludes issue.closed and issue.updated. After
	// T014 nothing on the read path consults it.
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, notify_event_types)
VALUES ($1, ARRAY['issue.status_changed']::text[])
ON CONFLICT (user_id) DO UPDATE SET notify_event_types = EXCLUDED.notify_event_types`,
		caller); err != nil {
		t.Fatalf("seed user_notification_prefs: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, caller)
	})

	store := NewStore(pool)

	for _, scope := range []string{"unread", "all"} {
		res, err := store.ListInbox(ctx, ListInboxArgs{UserID: caller, Scope: scope, Limit: 50})
		if err != nil {
			t.Fatalf("ListInbox scope=%s: %v", scope, err)
		}
		got := map[int64]bool{}
		for _, it := range res.Items {
			got[it.ID] = true
		}
		if !got[nAllowed] {
			t.Errorf("scope=%s: allowed-type feed row %d missing", scope, nAllowed)
		}
		if !got[nExcluded] {
			t.Errorf("scope=%s: feed row %d of a whitelist-excluded type must still be listed — the read filter is gone (T014)", scope, nExcluded)
		}
		if got[nPushOnly] {
			t.Errorf("scope=%s: push-only row %d leaked into the feed (INV-11)", scope, nPushOnly)
		}
	}

	counts, err := store.Counts(ctx, caller, nil)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.Unread != 2 || counts.Total != 2 {
		t.Errorf("Counts = {unread:%d,total:%d}, want {2,2} — two feed rows, the push-only row uncounted",
			counts.Unread, counts.Total)
	}

	// Per-project counts must agree with the global ones on the same data.
	scoped, err := store.Counts(ctx, caller, &projectID)
	if err != nil {
		t.Fatalf("Counts(project): %v", err)
	}
	if scoped.Unread != 2 || scoped.Total != 2 {
		t.Errorf("Counts(project) = {unread:%d,total:%d}, want {2,2}", scoped.Unread, scoped.Total)
	}

	// A push-only row must not be addressable either: the 204-vs-404 split
	// on the single-row mutators would otherwise be an existence oracle for
	// a row no surface can show.
	if err := store.MarkRead(ctx, nPushOnly, caller); err == nil {
		t.Errorf("MarkRead on push-only row %d succeeded; want ErrNotFound", nPushOnly)
	}
	if err := store.Archive(ctx, nPushOnly, caller); err == nil {
		t.Errorf("Archive on push-only row %d succeeded; want ErrNotFound", nPushOnly)
	}

	// mark-all-read reports only what the badge counted.
	updated, err := store.MarkAllRead(ctx, MarkAllReadArgs{UserID: caller})
	if err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	if updated != 2 {
		t.Errorf("MarkAllRead updated = %d, want 2 (feed rows only)", updated)
	}
}

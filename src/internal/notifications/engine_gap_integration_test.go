package notifications

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-105.4 (S3 engine gaps) — notifications fan-out arms (M10) +
// watch/unwatch idempotency matrix (m14).
//
// FanOut is driven synchronously (called directly inside a tx) — no
// background goroutine is started. All assertions are scoped to the seeded
// user + event id; DB-backed, skip without TEST_DATABASE_URL (in-package
// newTestPool).

// addMember adds a project membership row (the project arm of FanOut
// rechecks membership). Registers cleanup.
func addMember(t *testing.T, pool *pgxpool.Pool, projectID, userID, addedBy uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID, addedBy)
	if err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, userID)
	})
}

// TestFanOut_DependencyEvents_AreAuditOnly pins the sk-303.13 alignment of
// the fan-out with the reason registry. Dependency CRUD used to materialise a
// notification for every project watcher; the registry calls it audit-only —
// there is no reason it projects onto, and the spec names dependencies among
// the events that live only as journal records. So the correct answer is now
// ZERO rows, and it is not an unmapped-signal case either: audit-only is a
// decision, not a configuration error.
func TestFanOut_DependencyEvents_AreAuditOnly(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	watcher := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, watcher, owner)

	issueID := "fpdep-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	store := NewStore(pool)
	if err := store.AddProjectWatch(ctx, projectID, watcher); err != nil {
		t.Fatalf("AddProjectWatch: %v", err)
	}

	for _, evtType := range []string{"dependency.created", "dependency.deleted"} {
		tx, _ := pool.Begin(ctx)
		// dependency.* events are recorded on the issue entity in production.
		eid := addEvent(t, tx, evtType, "issue", issueID, projectID, owner, "human")
		inserted, err := FanOut(ctx, tx, FanOutInput{
			EventID:     eid,
			EventType:   evtType,
			EntityType:  "issue",
			IssueID:     issueID,
			ProjectID:   projectID,
			ActorUserID: owner,
			ActorType:   "human",
		})
		if err != nil {
			t.Fatalf("fanout %s: %v", evtType, err)
		}
		_ = tx.Commit(ctx)

		if inserted != 0 {
			t.Fatalf("%s: FanOut inserted %d rows, want 0 (audit-only)", evtType, inserted)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM notifications WHERE event_id = $1`, eid).Scan(&n); err != nil {
			t.Fatalf("count notifications for %s: %v", evtType, err)
		}
		if n != 0 {
			t.Fatalf("%s: expected 0 notifications (audit-only), got %d", evtType, n)
		}
	}
}

// TestFanOut_IssueAccepted_ReachesWatchers verifies the issue.accepted arm
// routes through the watcher pool (the creator typically auto-watches): a
// project watcher who is not the actor gets one notification.
func TestFanOut_IssueAccepted_ReachesWatchers(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	watcher := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, watcher, owner)

	issueID := "fpacc-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	store := NewStore(pool)
	if err := store.AddProjectWatch(ctx, projectID, watcher); err != nil {
		t.Fatalf("AddProjectWatch: %v", err)
	}

	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "issue.accepted", "issue", issueID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:        eid,
		EventType:      "issue.accepted",
		EntityType:     "issue",
		IssueID:        issueID,
		ProjectID:      projectID,
		ActorUserID:    owner,
		ActorType:      "human",
		ExplicitAccept: true,
	}); err != nil {
		t.Fatalf("fanout issue.accepted: %v", err)
	}
	_ = tx.Commit(ctx)

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		watcher, eid).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	if n != 1 {
		t.Fatalf("issue.accepted: expected 1 notification for watcher, got %d", n)
	}

	// The other half of the same rule: acceptance stamped AUTOMATICALLY
	// (claim, self-assign, create-on-self) is journal-only. Same event type,
	// same watcher, no ExplicitAccept — and therefore no row.
	tx2, _ := pool.Begin(ctx)
	eid2 := addEvent(t, tx2, "issue.accepted", "issue", issueID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx2, FanOutInput{
		EventID:     eid2,
		EventType:   "issue.accepted",
		EntityType:  "issue",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}); err != nil {
		t.Fatalf("fanout auto issue.accepted: %v", err)
	}
	_ = tx2.Commit(ctx)

	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE event_id = $1`, eid2).Scan(&n); err != nil {
		t.Fatalf("count auto-accept notifications: %v", err)
	}
	if n != 0 {
		t.Fatalf("auto issue.accepted: expected 0 notifications, got %d", n)
	}
}

// TestFanOut_MentionShortCircuit_DoesNotBroadcast verifies the M10
// invariant: comment.mention / issue.mention must NOT broadcast to project
// watchers. FanOut returns 0 and writes no rows even for a subscribed
// project watcher — mentions are point-to-point (WriteMentionNotification),
// not pool-broadcast.
func TestFanOut_MentionShortCircuit_DoesNotBroadcast(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	watcher := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, watcher, owner)

	issueID := "fpmnt-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	store := NewStore(pool)
	if err := store.AddProjectWatch(ctx, projectID, watcher); err != nil {
		t.Fatalf("AddProjectWatch: %v", err)
	}
	// Also a watcher on the issue itself — neither pool should receive the
	// mention via FanOut.
	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, watcher, "manual"); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, watcher)
	})

	for _, evtType := range []string{"comment.mention", "issue.mention"} {
		entityType := "issue"
		entityID := issueID
		if evtType == "comment.mention" {
			// comment.mention is recorded on a comment entity in production;
			// FanOut still short-circuits regardless of entity type.
			entityType = "comment"
			entityID = uuid.Must(uuid.NewRandom()).String()
		}
		tx, _ := pool.Begin(ctx)
		eid := addEvent(t, tx, evtType, entityType, entityID, projectID, owner, "human")
		n, err := FanOut(ctx, tx, FanOutInput{
			EventID:     eid,
			EventType:   evtType,
			EntityType:  entityType,
			IssueID:     issueID,
			ProjectID:   projectID,
			ActorUserID: owner,
			ActorType:   "human",
		})
		if err != nil {
			t.Fatalf("fanout %s: %v", evtType, err)
		}
		_ = tx.Commit(ctx)

		if n != 0 {
			t.Fatalf("%s: FanOut returned %d, want 0 (mentions must not broadcast)", evtType, n)
		}
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM notifications WHERE event_id = $1`, eid).Scan(&rows); err != nil {
			t.Fatalf("count notifications for %s: %v", evtType, err)
		}
		if rows != 0 {
			t.Fatalf("%s: expected 0 broadcast notifications, got %d", evtType, rows)
		}
	}
}

// TestIssueWatch_Idempotency verifies the m14 watch/unwatch matrix on
// issue watchers (sk-622.21, T010, row No. 23 — opt-outs instead of tombstones):
//   - RemoveIssueWatch → no subscription row, one opt-out
//   - double RemoveIssueWatch → still no row, still exactly one opt-out, no
//     error
//   - AddIssueWatch after the unwatch → re-subscribes (manual reason) and
//     deletes the opt-out
func TestIssueWatch_Idempotency(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	issueID := "fpwiq-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, owner)
	})

	store := NewStore(pool)

	isWatching := func() bool {
		ws, err := store.ListIssueWatchers(ctx, issueID)
		if err != nil {
			t.Fatalf("ListIssueWatchers: %v", err)
		}
		for _, w := range ws {
			if w.UserID == owner {
				return true
			}
		}
		return false
	}

	// Manual watch → present.
	if err := store.AddIssueWatch(ctx, issueID, owner, false); err != nil {
		t.Fatalf("AddIssueWatch: %v", err)
	}
	if !isWatching() {
		t.Fatalf("expected owner watching after AddIssueWatch")
	}

	// rowsAndOptOuts reads the raw state: subscription rows of the pair
	// (any reasons) and opt-outs of the pair.
	rowsAndOptOuts := func() (rows, optouts int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2),
       (SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2)`,
			issueID, owner).Scan(&rows, &optouts); err != nil {
			t.Fatalf("read state: %v", err)
		}
		return rows, optouts
	}

	// Unwatch → no row, one opt-out.
	if err := store.RemoveIssueWatch(ctx, issueID, owner); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}
	if isWatching() {
		t.Fatalf("expected owner NOT watching after RemoveIssueWatch")
	}
	if rows, optouts := rowsAndOptOuts(); rows != 0 || optouts != 1 {
		t.Fatalf("after unwatch: subscription rows = %d (want 0, no tombstone), opt-outs = %d (want 1)", rows, optouts)
	}

	// Double unwatch → still no row, still one opt-out, no error.
	if err := store.RemoveIssueWatch(ctx, issueID, owner); err != nil {
		t.Fatalf("second RemoveIssueWatch (no-op): %v", err)
	}
	if rows, optouts := rowsAndOptOuts(); rows != 0 || optouts != 1 {
		t.Fatalf("after double unwatch: subscription rows = %d (want 0), opt-outs = %d (want 1)", rows, optouts)
	}

	// Re-watch after the unwatch → re-subscribes and deletes the opt-out.
	if err := store.AddIssueWatch(ctx, issueID, owner, false); err != nil {
		t.Fatalf("re-AddIssueWatch after unwatch: %v", err)
	}
	if !isWatching() {
		t.Fatalf("re-watch after unwatch should re-subscribe the owner")
	}
	if _, optouts := rowsAndOptOuts(); optouts != 0 {
		t.Fatalf("opt-out survived the manual re-watch (%d)", optouts)
	}
}

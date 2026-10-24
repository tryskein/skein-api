package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests for the subtree delivery arm in FanOut (sk-142.9 T9;
// hierarchy since sk-622.21 T012, US-3, INV-3): a watcher with
// scope='subtree' on an ancestor issue receives events whose target issue
// has that ancestor in its chain of canonical parents (parent-child edges).
// An id prefix is NOT a hierarchy: descendants are seeded with real edges
// (seedChild), and an issue that merely shares the prefix gets nothing. The
// arm carries the issue arm's filters, so every issue-arm filter must still
// hold.
//
// Tests skip when TEST_DATABASE_URL is unset (newTestPool handles the skip).

// addMember (project membership for the FanOut recheck) is shared from
// engine_gap_integration_test.go in this package.

// seedChild seeds issue child in projectID and makes it a child of parent
// with a parent-child edge — the canonical parent the subtree arm walks
// (sk-622.21, US-3). The edge row has the shape the dependencies store
// writes; this package cannot import it (it imports notifications).
func seedChild(t *testing.T, pool *pgxpool.Pool, projectID, owner uuid.UUID, child, parent string) {
	t.Helper()
	seedTestIssue(t, pool, projectID, owner, child, 2)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`, child, parent, owner); err != nil {
		t.Fatalf("seed edge %s → %s: %v", child, parent, err)
	}
}

// watchSubtree subscribes user to issueID with scope='subtree' via the T8
// AddIssueWatcherScoped path (the same path
// skein_watchers(action="watch", subtree=true) uses).
func watchSubtree(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reason string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := AddIssueWatcherScoped(ctx, tx, issueID, userID, reason, ScopeSubtree); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("AddIssueWatcherScoped: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit watch: %v", err)
	}
}

// watchIssue subscribes user to issueID with the default scope='issue'.
func watchIssue(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reason string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := AddIssueWatcher(ctx, tx, issueID, userID, reason); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit watch: %v", err)
	}
}

// fanoutIssueEvent drives an issue.status_changed event whose target issue is
// targetIssueID (passed as the $4 issueID arg). entity_type='issue' so the
// event row is self-consistent, but FanOut never reads the event row — it
// keys solely off the passed args. Returns the event id.
func fanoutIssueEvent(t *testing.T, pool *pgxpool.Pool, targetIssueID string,
	projectID, actorUserID uuid.UUID, actorType string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	eid := addEvent(t, tx, "issue.status_changed", "issue", targetIssueID, projectID, actorUserID, actorType)
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:     eid,
		EventType:   "issue.status_changed",
		EntityType:  "issue",
		IssueID:     targetIssueID,
		ProjectID:   projectID,
		ActorUserID: actorUserID,
		ActorType:   actorType,
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fanout: %v", err)
	}
	return eid
}

func countNotif(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, eid int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		userID, eid).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// TestFanOut_Subtree_DescendantDelivery: a subtree watcher on the parent
// receives events targeting a child AND a grandchild — descendants by
// parent-child edges.
func TestFanOut_Subtree_DescendantDelivery(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, subscriber, owner)

	parent := "stsd42"
	seedTestIssue(t, pool, projectID, owner, parent, 2)
	seedChild(t, pool, projectID, owner, parent+".1", parent)
	seedChild(t, pool, projectID, owner, parent+".1.3", parent+".1")

	watchSubtree(t, pool, parent, subscriber, "manual")

	// Child event.
	eidChild := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidChild); n != 1 {
		t.Errorf("subtree watcher must receive child event, got %d notifications", n)
	}

	// Grandchild event.
	eidGrand := fanoutIssueEvent(t, pool, parent+".1.3", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidGrand); n != 1 {
		t.Errorf("subtree watcher must receive grandchild event, got %d notifications", n)
	}
}

// TestFanOut_Subtree_IssueScopeDoesNotDescend: a plain scope='issue' watcher
// on the parent must NOT receive a child event (only the subtree arm descends).
func TestFanOut_Subtree_IssueScopeDoesNotDescend(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, subscriber, owner)

	parent := "stis42"
	seedTestIssue(t, pool, projectID, owner, parent, 2)
	seedChild(t, pool, projectID, owner, parent+".1", parent)

	watchIssue(t, pool, parent, subscriber, "manual")

	eidChild := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidChild); n != 0 {
		t.Errorf("issue-scope watcher must NOT receive child event, got %d notifications", n)
	}

	// Sanity: it DOES receive the parent's own event.
	eidParent := fanoutIssueEvent(t, pool, parent, projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidParent); n != 1 {
		t.Errorf("issue-scope watcher must receive parent event, got %d notifications", n)
	}
}

// TestFanOut_Subtree_ExactIdStrictDescendantsOnly: the exact-id case is
// covered by the issue-watcher arm; the subtree arm matches STRICT
// descendants only — issues with the watched issue on their chain of
// canonical parents.
//
// The isolating half (sk-338, re-anchored by sk-622.21 T012): "stx1425"
// shares the id PREFIX with the watched "stx142" and has no edge to it, so it
// is not a descendant and must get nothing; the true child "stx142.1" (with
// an edge) must get its event. Under the old id-prefix rule the zero was held
// by the dot in the LIKE suffix; under the hierarchy it is held by the edge,
// and a return to prefix matching without the dot reddens it. The
// exactly-once count on the watched issue itself is a cheap invariant guard
// (dedup by user + ON CONFLICT make it hard to fail), kept beside it.
func TestFanOut_Subtree_ExactIdStrictDescendantsOnly(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, subscriber, owner)

	parent := "stx142"
	seedTestIssue(t, pool, projectID, owner, parent, 2)
	seedTestIssue(t, pool, projectID, owner, parent+"5", 2)
	seedChild(t, pool, projectID, owner, parent+".1", parent)

	watchSubtree(t, pool, parent, subscriber, "manual")

	eid := fanoutIssueEvent(t, pool, parent, projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eid); n != 1 {
		t.Errorf("subtree watcher on the exact issue must be notified exactly once, got %d", n)
	}

	eidSibling := fanoutIssueEvent(t, pool, parent+"5", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidSibling); n != 0 {
		t.Errorf("subtree watch on %q delivered an event on %q: a shared id prefix without a "+
			"parent-child edge is not a descendant, got %d rows", parent, parent+"5", n)
	}

	eidChild := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidChild); n != 1 {
		t.Errorf("true child %q (edge to %q) must be delivered, got %d rows", parent+".1", parent, n)
	}
}

// TestFanOut_Subtree_FiltersApply asserts every issue-arm filter is retained
// in the cloned subtree arm: explicit unwatch, removed-member, self-action, and
// the agent-action suppression all exclude the subtree watcher from a child
// event. (The last one is no longer «suppress-own-agent»: sk-335 made the
// setting cover any agent, and the name survived here only in this comment.)
func TestFanOut_Subtree_FiltersApply(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)

	t.Run("explicit-unwatch", func(t *testing.T) {
		sub := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, sub, owner)
		parent := "stf_sticky"
		seedTestIssue(t, pool, projectID, owner, parent, 2)
		seedChild(t, pool, projectID, owner, parent+".1", parent)

		watchSubtree(t, pool, parent, sub, "manual")
		// sk-622.21: the explicit unwatch deletes the subtree subscription
		// and records an opt-out (no tombstone row since R2).
		if err := NewStore(pool).RemoveIssueWatch(ctx, parent, sub); err != nil {
			t.Fatalf("explicit unwatch: %v", err)
		}

		eid := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "human")
		if n := countNotif(t, pool, sub, eid); n != 0 {
			t.Errorf("a subtree watcher who explicitly unwatched must be excluded, got %d", n)
		}
	})

	t.Run("removed-member", func(t *testing.T) {
		sub := seedTestUser(t, pool, "user") // NOT a member, not admin
		parent := "stf_member"
		seedTestIssue(t, pool, projectID, owner, parent, 2)
		seedChild(t, pool, projectID, owner, parent+".1", parent)

		watchSubtree(t, pool, parent, sub, "manual")

		eid := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "human")
		if n := countNotif(t, pool, sub, eid); n != 0 {
			t.Errorf("non-member subtree watcher must be excluded by membership recheck, got %d", n)
		}
	})

	t.Run("self-action", func(t *testing.T) {
		sub := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, sub, owner)
		parent := "stf_self"
		seedTestIssue(t, pool, projectID, owner, parent, 2)
		seedChild(t, pool, projectID, owner, parent+".1", parent)

		watchSubtree(t, pool, parent, sub, "manual")
		// Actor == the watcher → self-action exclusion.
		eid := fanoutIssueEvent(t, pool, parent+".1", projectID, sub, "human")
		if n := countNotif(t, pool, sub, eid); n != 0 {
			t.Errorf("self-action subtree watcher must be excluded, got %d", n)
		}
	})

	// The subtree arm carries the agent-action suppression too, and this is
	// the gate a per-arm behavioural test can actually see.
	//
	// sk-335 also fixed what this subcase PROVED. It used to act as the
	// subscriber's OWN agent (actor == sub), which the self-action exclusion
	// on the line above already silences — so the assertion held whether the
	// suppression worked or not, and it held for years while the branch was
	// dead. The actor is now another owner's agent, which nothing but the
	// suppression can stop, and the OFF state is asserted beside the ON state
	// so the difference is attributable to the setting.
	t.Run("suppress-agent-actions", func(t *testing.T) {
		sub := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, sub, owner)
		parent := "stf_agent"
		seedTestIssue(t, pool, projectID, owner, parent, 2)
		seedChild(t, pool, projectID, owner, parent+".1", parent)

		watchSubtree(t, pool, parent, sub, "manual")
		enableReasonChannels(t, pool, sub, ReasonKeyDelegatedStatus, true, false)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, sub)
		})

		setSuppress := func(on bool) {
			if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET suppress_agent_actions = EXCLUDED.suppress_agent_actions`,
				sub, on); err != nil {
				t.Fatalf("set prefs: %v", err)
			}
		}

		// Actor is ANOTHER owner's agent: only the suppression can stop it.
		setSuppress(false)
		eidOff := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "agent")
		if n := countNotif(t, pool, sub, eidOff); n != 1 {
			t.Errorf("suppression off: subtree watcher got %d rows, want 1", n)
		}

		setSuppress(true)
		eidOn := fanoutIssueEvent(t, pool, parent+".1", projectID, owner, "agent")
		if n := countNotif(t, pool, sub, eidOn); n != 0 {
			t.Errorf("suppression on: subtree watcher got %d rows, want 0", n)
		}
	})
}

// TestFanOut_Subtree_EscapeLikeSibling: a neighbouring branch without an
// edge is not a descendant. Watch "sta_b"; an event on "staXb.1" (whose
// parent is "staXb", not "sta_b") must NOT deliver, while an event on the
// true child "sta_b.1" must.
//
// sk-622.21 T012: the property this test was written for — LIKE
// metacharacters in a custom id ('_') must not leak into a subtree match —
// disappeared together with the LIKE; it is replaced by "a descendant only
// by edge". The fixture keeps the '_' id, so a return to an unescaped
// prefix match reddens it as before.
func TestFanOut_Subtree_EscapeLikeSibling(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, subscriber, owner)

	parent := "sta_b"
	seedTestIssue(t, pool, projectID, owner, parent, 2)
	seedTestIssue(t, pool, projectID, owner, "staXb", 2)
	seedChild(t, pool, projectID, owner, "staXb.1", "staXb")
	seedChild(t, pool, projectID, owner, "sta_b.1", parent)

	watchSubtree(t, pool, parent, subscriber, "manual")

	eidSibling := fanoutIssueEvent(t, pool, "staXb.1", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidSibling); n != 0 {
		t.Errorf("staXb.1 (child of staXb) must NOT reach a subtree watch on sta_b, got %d", n)
	}

	eidTrueChild := fanoutIssueEvent(t, pool, "sta_b.1", projectID, owner, "human")
	if n := countNotif(t, pool, subscriber, eidTrueChild); n != 1 {
		t.Errorf("true child sta_b.1 must be delivered, got %d", n)
	}
}

// TestFanOut_Subtree_OptOutOnDescendantKeepsAncestorSubtree — sk-622.21
// (US-1, US-3): an opt-out is read by the automatic subscription paths
// only. A user who explicitly unwatched a descendant C (subscription deleted,
// opt-out recorded) and watches its ancestor A with subtree scope still gets
// C's events — through A, with A as the card.
func TestFanOut_Subtree_OptOutOnDescendantKeepsAncestorSubtree(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, subscriber, owner)

	parent := "stoo42"
	child := parent + ".1"
	seedTestIssue(t, pool, projectID, owner, parent, 2)
	seedChild(t, pool, projectID, owner, child, parent)

	watchSubtree(t, pool, parent, subscriber, "manual")
	watchIssue(t, pool, child, subscriber, "manual")
	if err := store.RemoveIssueWatch(ctx, child, subscriber); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}
	var optouts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`,
		child, subscriber).Scan(&optouts); err != nil {
		t.Fatalf("read opt-out: %v", err)
	}
	if optouts != 1 {
		t.Fatalf("precondition: opt-out on %s = %d, want 1", child, optouts)
	}

	eid := fanoutIssueEvent(t, pool, child, projectID, owner, "human")
	var card *string
	if err := pool.QueryRow(ctx,
		`SELECT card_issue_id FROM notifications WHERE user_id = $1 AND event_id = $2`,
		subscriber, eid).Scan(&card); err != nil {
		t.Fatalf("the opt-out on %s shadowed the subtree subscription on %s: no row (%v)", child, parent, err)
	}
	if card == nil || *card != parent {
		got := "<none>"
		if card != nil {
			got = *card
		}
		t.Errorf("card = %s, want %s (the ancestor's subscription)", got, parent)
	}
}

// TestFanOut_Subtree_E2E_PollDelivery is the cross-arm proof for a real
// comment.created event on a child under a subtree-watched parent: FanOut (T9)
// materialises the human-inbox notification AND PollForMonitor scoped to the
// parent subtree (T4) surfaces the same child comment.
//
// sk-164 re-anchor (F8): after the monitor source switched from `notifications`
// to `events`, the poll no longer DEPENDS on FanOut having materialised a row —
// it reads the `events` audit log directly. The two assertions below are now
// INDEPENDENT proofs of two separate contracts on the same committed event:
//   - countNotif == 1  → FanOut's subtree arm still writes the human inbox row
//     (SC-004 human path unchanged);
//   - poll returns the child comment → the event-tail delivers by scope,
//     regardless of any notifications row (the sk-164 delivery contract).
//
// This is intentionally NOT a mute green-regress: the poll here would return
// the event even if FanOut had written no notification at all.
func TestFanOut_Subtree_E2E_PollDelivery(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, subscriber, owner)

	parent := "ste2e42"
	child := "ste2e42.1"
	seedTestIssue(t, pool, projectID, owner, parent, 2)
	seedChild(t, pool, projectID, owner, child, parent)

	// T8: subtree watch on the parent.
	watchSubtree(t, pool, parent, subscriber, "manual")

	// A real comment on the child + comment.created event routed through
	// FanOut (the production write path). FanOut materialises the
	// notification for the subtree watcher via the new T9 arm.
	// "Comment" is off in the narrow default; this test is about the
	// subtree ARM, so switch the reason on explicitly.
	enableReasonChannels(t, pool, subscriber, ReasonKeyComment, true, false)

	commentID := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', 'blocked, need a decision', now(), now())`,
		commentID, child, actor); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentID) })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Comment events carry the comment uuid as entity_id; the parent issue id
	// (here the child issue id) is passed separately as the issueID arg.
	eid := addEvent(t, tx, "comment.created", "comment", commentID.String(), projectID, actor, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:     eid,
		EventType:   "comment.created",
		EntityType:  "comment",
		IssueID:     child,
		ProjectID:   projectID,
		ActorUserID: actor,
		ActorType:   "human",
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut comment.created: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Contract 1 (FanOut / human path, SC-004): the notification was
	// materialised for the subtree watcher.
	if n := countNotif(t, pool, subscriber, eid); n != 1 {
		t.Fatalf("expected 1 materialised notification for subtree watcher, got %d", n)
	}

	// Contract 2 (sk-164 event-tail): poll scoped to the parent subtree returns
	// the child comment — read from `events`, NOT from the notification row
	// above. epoch cursor (not live-start) + SafetyLag 0 so the freshly-committed
	// row is eligible deterministically.
	sinceTS := time.Unix(1, 0).UTC()
	sinceID := int64(0)
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       subscriber,
		ProjectID:    projectID,
		ScopeIssueID: parent,
		ScopeSubtree: true,
		EventTypes:   []string{"comment.created"},
		SinceTS:      &sinceTS,
		SinceID:      &sinceID,
		Limit:        50,
		SafetyLag:    0,
	})
	if err != nil {
		t.Fatalf("PollForMonitor: %v", err)
	}
	found := false
	for _, it := range res.Items {
		if it.Type == "comment.created" && it.Entity.ID == child {
			found = true
		}
	}
	if !found {
		t.Errorf("poll on parent subtree must return the child comment notification; got %d items", len(res.Items))
	}
}

// cycleLogRecord returns the error-level record of hierarchy.LogCycle whose
// issueId attribute is issueID, or nil. The record is found by level and
// attribute, never by message text.
func cycleLogRecord(t *testing.T, logs *logCapture, issueID string) map[string]any {
	t.Helper()
	for _, r := range logs.records(t) {
		if r["level"] == "ERROR" && r["issueId"] == issueID {
			return r
		}
	}
	return nil
}

// assertCycleLogged checks the loop report of US-3 / edge 7: an error record
// that names the issue whose walk met the loop, the repeated issue, and a
// chain that carries every id of the loop.
func assertCycleLogged(t *testing.T, logs *logCapture, issueID, repeated string, chainIDs ...string) {
	t.Helper()
	r := cycleLogRecord(t, logs, issueID)
	if r == nil {
		t.Fatalf("no error-level log record with issueId=%s — the hierarchy loop was not reported (US-3)", issueID)
	}
	if r["repeatedIssueId"] != repeated {
		t.Errorf("repeatedIssueId = %v, want %s", r["repeatedIssueId"], repeated)
	}
	chain, _ := r["chain"].(string)
	for _, id := range chainIDs {
		if !strings.Contains(chain, id) {
			t.Errorf("chain %q does not name issue %s", chain, id)
		}
	}
}

// seedHierarchyLoop seeds issues a and b in projectID with a loop in their
// canonical parents: b → a and a → b. Edge creation refuses such a loop, so it
// is written straight into dependencies — the "data closes a loop anyway" case
// of US-3.
func seedHierarchyLoop(t *testing.T, pool *pgxpool.Pool, projectID, owner uuid.UUID, a, b string) {
	t.Helper()
	seedTestIssue(t, pool, projectID, owner, a, 2)
	seedChild(t, pool, projectID, owner, b, a)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`, a, b, owner); err != nil {
		t.Fatalf("seed loop edge %s → %s: %v", a, b, err)
	}
}

// TestFanOut_HierarchyLoopWithoutCandidatesIsLogged — sk-622.21 US-3 / edge 7
// (fix found in review): the fan-out of an event whose issue sits on
// a loop of canonical parents (A → B → A) terminates and reports the loop as an
// error with the issue ids EVEN WHEN no recipient survives — the case the
// per-candidate report used to lose. Red before the fix: the loop flag rode on
// candidate rows, so zero candidates meant no record at all.
func TestFanOut_HierarchyLoopWithoutCandidatesIsLogged(t *testing.T) {
	pool := newTestPool(t)
	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)

	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	a, b := "fhla"+sfx, "fhlb"+sfx
	seedHierarchyLoop(t, pool, projectID, owner, a, b)

	logs := captureLogs(t)
	// The owner acts on B; nobody watches anything, so the pool is empty —
	// the self-action exclusion removes the only member.
	eid := fanoutIssueEvent(t, pool, b, projectID, owner, "human")

	var written int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE event_id = $1`, eid).Scan(&written); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	if written != 0 {
		t.Fatalf("precondition: the event must have no recipient, fan-out wrote %d rows", written)
	}
	assertCycleLogged(t, logs, b, b, a, b)
}

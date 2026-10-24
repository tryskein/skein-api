package notifications

// watching_count_plan_test — sk-303.13, coverage-matrix row 32.
//
// The requirement is non-functional and stated once in the spec:
//
//	"Lifting the watching-counter cap must not turn the count into a full
//	 walk of every notification the user has ever received, on every header
//	 request."
//
// It did not appear out of nowhere: before T015 the watching column was
// capped at a hundred issues, and lifting that ceiling is exactly the
// change that is easy to pay for with a full scan of every notification
// ever received.
//
// The assertion is the SHAPE: the counter is one set-based query per
// header request, not a query per subscription. Since sk-622.21 (T022) that
// query is notifications.WatchingBadgeSQL — a count of the caller's carded
// notifications in the Watching window, which reads no subscription at all. It is counted by a pgx
// tracer, i.e. by the statements that actually left for the server, not by
// call sites someone remembered to mark. Two corpora an order of magnitude
// apart are compared: the statement count must MATCH, not "grow slowly".
//
// There are no timing thresholds here and there will not be: the test plan
// forbids them outright, and seconds on a shared CI Postgres are a flake
// generator, not a proof.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedWatchingCorpus gives one user `subs` subscriptions and, on each of
// them, `perSub` unread feed notifications, plus `readPerSub` already-read
// ones. The read rows matter: they are exactly the volume the partial index
// is supposed to keep the count from walking.
func seedWatchingCorpus(t *testing.T, pool *pgxpool.Pool, prefix string, subs, perSub, readPerSub int) (uuid.UUID, func()) {
	t.Helper()
	ctx := context.Background()

	user := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, user)

	issueIDs := make([]string, 0, subs)
	for i := range subs {
		id := fmt.Sprintf("%s-%d", prefix, i+1)
		seedTestIssue(t, pool, projectID, user, id, 2)
		issueIDs = append(issueIDs, id)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope, since)
SELECT unnest($1::text[]), $2, ARRAY['manual']::text[], 'issue', now()
ON CONFLICT (issue_id, user_id) DO NOTHING`, issueIDs, user); err != nil {
		t.Fatalf("seed watchers: %v", err)
	}

	// events + notifications in two set-based statements per class. Each
	// row carries its issue as the Watching card — what the fan-out writes
	// for an exact subscriber (sk-622.21 US-4); the badge reads the card.
	seedRows := func(perIssue int, read bool) {
		if perIssue == 0 {
			return
		}
		if _, err := pool.Exec(ctx, `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    SELECT now() - (g % 1000) * interval '1 minute',
           'issue.status_changed', 'issue', i.id, $3,
           'tester', $4, 'human', '{}'::jsonb
      FROM unnest($1::text[]) AS i(id)
      CROSS JOIN generate_series(1, $2) g
    RETURNING id, entity_id
)
INSERT INTO notifications (user_id, event_id, type, project_id, created_at,
                           feed_channel, read_at, card_issue_id, card_resolved)
SELECT $5, ev.id, 'issue.status_changed', $3, now(), true,
       CASE WHEN $6::boolean THEN now() ELSE NULL END,
       ev.entity_id, true
  FROM ev`, issueIDs, perIssue, projectID, actor, user, read); err != nil {
			t.Fatalf("seed notification corpus (read=%v): %v", read, err)
		}
	}
	seedRows(perSub, false)
	seedRows(readPerSub, true)

	cleanup := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM notifications WHERE user_id = $1`, user)
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE actor_user_id = $1`, actor)
	}
	t.Cleanup(cleanup)
	return user, cleanup
}

// TestWatchingCount_IsSetBased asserts the FORM: the header's Watching count
// issues the same number of statements whatever the subscription count.
//
// Equality, not a bound: a per-subscription query is the defect the
// requirement names, and it would show up as a difference of (40 - 4)
// statements. Comparing two runs also keeps the assertion independent of how
// many statements the count happens to need in total, so a future refactor
// into two set-based steps would not force a test edit — only reintroducing
// per-subscription work would.
func TestWatchingCount_IsSetBased(t *testing.T) {
	pool, tracer := newTracedTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	smallUser, _ := seedWatchingCorpus(t, pool, "wcsmall", 4, 2, 0)
	largeUser, _ := seedWatchingCorpus(t, pool, "wclarge", 40, 2, 0)

	run := func(u uuid.UUID) []string {
		return tracer.record(func() {
			if _, err := store.CountWatchingUnread(ctx, u); err != nil {
				t.Fatalf("CountWatchingUnread: %v", err)
			}
		})
	}
	small := run(smallUser)
	large := run(largeUser)

	if len(small) != len(large) {
		t.Errorf("statement count grew with the subscription corpus: %d for 4 subscriptions, %d for 40\n4:\n%s\n40:\n%s",
			len(small), len(large), strings.Join(small, "\n---\n"), strings.Join(large, "\n---\n"))
	}
	if len(large) != 1 {
		t.Errorf("the Watching count issued %d statements, want exactly 1 per header request:\n%s",
			len(large), strings.Join(large, "\n---\n"))
	}

	// And the numbers are right — a count that is cheap because it is wrong
	// would satisfy every assertion above.
	if n, err := store.CountWatchingUnread(ctx, smallUser); err != nil || n != 8 {
		t.Errorf("CountWatchingUnread(small) = %d, %v; want 8, nil (4 subs x 2 unread)", n, err)
	}
	if n, err := store.CountWatchingUnread(ctx, largeUser); err != nil || n != 80 {
		t.Errorf("CountWatchingUnread(large) = %d, %v; want 80, nil (40 subs x 2 unread)", n, err)
	}
}

package notifications

// watching_cost_test.go — sk-622.21, T022, matrix row No. 43 (SC-001 for the
// Watching badge, SC-011 for the column, SC-002 for both). Run: final.
//
// Measured, not timed (project test guidelines): the statements the
// store's Watching reads actually send are captured with their arguments by
// a pgx tracer (cardPlanCapture — the test never re-types the SQL), each is
// re-run as EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON), and the pages of the
// top node (Shared Hit Blocks + Shared Read Blocks, inclusive) are summed.
// Tables are ANALYZEd after seeding; plans are walked by Node Type and
// Relation Name for a Seq Scan on any relation of SC-002 (cardPlanTables).
//
// Literal bounds of the spec:
//
//   - SC-001, badge: pages with 20 000 subscriptions and 5 000 notifications
//     outside the window ≤ 2 × pages with 100 subscriptions and 50 outside,
//     with the same notifications inside the window;
//   - SC-011, column: pages with 5 000 notifications outside the window and
//     10 000 opt-outs ≤ 2 × pages without them, same subscriptions and
//     notifications inside the window; with 10 × the live subscriptions ≤
//     10 × pages.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// watchingCostInWindow is the number of unread notifications inside the
// window every corpus carries — the constant of each comparison.
const watchingCostInWindow = 50

// watchingCostIssues is the size of the shared issue pool: enough for the
// widest corpus (20 000 subscriptions) and for 10 000 opt-outs on issues
// beyond the subscribed ones, and a table of the scale where a read by key
// is what the planner picks.
const watchingCostIssues = 300000

// watchingCostWorld is one project with watchingCostIssues issues
// (prefix-1 … prefix-N) and its owner, who acts in every event.
type watchingCostWorld struct {
	projectID uuid.UUID
	owner     uuid.UUID
	prefix    string
}

func seedWatchingCostWorld(t *testing.T, pool *pgxpool.Pool) watchingCostWorld {
	t.Helper()
	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	prefix := "wc" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-' || g, $2, NULL, 1, 'watching cost', '', 'task', 2, 'open',
       false, 'human', NULL, '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series(1, $4) g`, prefix, projectID, owner, watchingCostIssues); err != nil {
		t.Fatalf("seed issues: %v", err)
	}
	return watchingCostWorld{projectID: projectID, owner: owner, prefix: prefix}
}

// seedWatchingCostUser gives a fresh member `subs` live subscriptions
// (issues 1…subs), watchingCostInWindow unread carded notifications written
// an hour ago on issues 1…50, `outside` unread carded notifications written
// 30 days ago (outside the default 21-day window) on the subscribed issues,
// and `optOuts` opt-outs on issues beyond the subscribed ones.
func seedWatchingCostUser(t *testing.T, pool *pgxpool.Pool, w watchingCostWorld, subs, outside, optOuts int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	user := seedTestUser(t, pool, "user")
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, w.projectID, user, w.owner); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1 || '-' || g, $2, ARRAY['manual']::text[], now() - interval '60 days', 'issue'
  FROM generate_series(1, $3) g`, w.prefix, user, subs); err != nil {
		t.Fatalf("seed subscriptions: %v", err)
	}
	if optOuts > 0 {
		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watch_optouts (issue_id, user_id)
SELECT $1 || '-' || g, $2 FROM generate_series($3 + 1, $3 + $4) g`, w.prefix, user, subs, optOuts); err != nil {
			t.Fatalf("seed opt-outs: %v", err)
		}
	}
	seed := func(n int, age string) {
		if n == 0 {
			return
		}
		// Notification g is on issue ((g-1) % subs) + 1 — always a
		// subscribed issue, carded to it (an exact subscription, US-4).
		if _, err := pool.Exec(ctx, `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    SELECT now() - $6::interval, 'issue.status_changed', 'issue',
           $1 || '-' || (((g - 1) % $2) + 1), $3, 'tester', $4, 'human', '{}'::jsonb
      FROM generate_series(1, $5) g
    RETURNING id, entity_id, occurred_at
)
INSERT INTO notifications (user_id, event_id, type, project_id, created_at,
                           feed_channel, card_issue_id, card_resolved)
SELECT $7, ev.id, 'issue.status_changed', $3, ev.occurred_at, true, ev.entity_id, true
  FROM ev`, w.prefix, subs, w.projectID, w.owner, n, age, user); err != nil {
			t.Fatalf("seed notifications (%s): %v", age, err)
		}
	}
	seed(watchingCostInWindow, "1 hour")
	seed(outside, "30 days")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM notifications WHERE user_id = $1`, user)
	})
	return user
}

// analyzeWatchingCost brings the seeded tables to the steady state of the
// production database: VACUUM ANALYZE refreshes the planner statistics (on
// unanalysed tables the planner rightly prefers a Seq Scan) and settles the
// bulk insert as autovacuum would.
func analyzeWatchingCost(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, tbl := range []string{"notifications", "issue_watchers", "issue_watch_optouts", "issues", "dependencies", "users", "events"} {
		if _, err := pool.Exec(context.Background(), "VACUUM ANALYZE "+tbl); err != nil {
			t.Fatalf("vacuum analyze %s: %v", tbl, err)
		}
	}
}

// seedWatchingCostBackground gives the owner n notifications — the other
// users' rows of the table, so a corpus user's rows are the small part of it
// they are in production.
func seedWatchingCostBackground(t *testing.T, pool *pgxpool.Pool, w watchingCostWorld, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    SELECT now() - (g % 900) * interval '1 hour', 'issue.status_changed', 'issue',
           $1 || '-' || (((g - 1) % 1000) + 1), $2, 'tester', $3, 'human', '{}'::jsonb
      FROM generate_series(1, $4) g
    RETURNING id, entity_id, occurred_at
)
INSERT INTO notifications (user_id, event_id, type, project_id, created_at,
                           feed_channel, card_issue_id, card_resolved)
SELECT $3, ev.id, 'issue.status_changed', $2, ev.occurred_at, true, ev.entity_id, true
  FROM ev`, w.prefix, w.projectID, w.owner, n); err != nil {
		t.Fatalf("seed background notifications: %v", err)
	}
	// And the owner's own subscriptions: the other users' rows of
	// issue_watchers.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1 || '-' || g, $2, ARRAY['creator']::text[], now(), 'issue'
  FROM generate_series(1, $3) g`, w.prefix, w.owner, n); err != nil {
		t.Fatalf("seed background subscriptions: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, w.owner)
	})
}

// measureWatchingRead runs read with the capture on and returns the pages
// its statements read under EXPLAIN (ANALYZE, BUFFERS) and the guarded
// relations any of their plans Seq Scans.
func measureWatchingRead(t *testing.T, pool *pgxpool.Pool, capture *cardPlanCapture, read func() error) (int64, []string) {
	t.Helper()
	ctx := context.Background()
	var readErr error
	stmts := capture.record(func() { readErr = read() })
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if len(stmts) == 0 {
		t.Fatalf("no statement captured — nothing measured")
	}
	var pages int64
	var seqs []string
	for _, s := range stmts {
		var raw []byte
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+s.sql, s.args...).Scan(&raw); err != nil {
			t.Fatalf("explain %q: %v", firstLine(s.sql), err)
		}
		var plan []struct {
			Plan cardPlanNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plan); err != nil || len(plan) == 0 {
			t.Fatalf("parse plan of %q: %v", firstLine(s.sql), err)
		}
		pages += plan[0].Plan.SharedHit + plan[0].Plan.SharedRead
		seqs = append(seqs, plan[0].Plan.seqScans()...)
	}
	return pages, seqs
}

// TestWatchingCost_BadgePagesIndependentOfSubscriptionsAndOldRows — No. 43,
// SC-001 (badge) and SC-002.
func TestWatchingCost_BadgePagesIndependentOfSubscriptionsAndOldRows(t *testing.T) {
	pool, capture := newCardPlanPool(t)
	store := NewStore(pool)
	w := seedWatchingCostWorld(t, pool)
	seedWatchingCostBackground(t, pool, w, 50000)
	small := seedWatchingCostUser(t, pool, w, 100, 50, 0)
	large := seedWatchingCostUser(t, pool, w, 20000, 5000, 0)
	analyzeWatchingCost(t, pool)

	measure := func(u uuid.UUID) (int64, []string) {
		var n int
		pages, seqs := measureWatchingRead(t, pool, capture, func() error {
			var err error
			n, err = store.CountWatchingUnread(context.Background(), u)
			return err
		})
		if n != watchingCostInWindow {
			t.Fatalf("badge = %d, want %d (the unread rows inside the window)", n, watchingCostInWindow)
		}
		return pages, seqs
	}
	smallPages, smallSeqs := measure(small)
	largePages, largeSeqs := measure(large)

	if smallPages == 0 {
		t.Fatalf("badge read 0 pages for the small corpus — nothing measured")
	}
	if largePages > 2*smallPages {
		t.Errorf("badge pages: %d with 20 000 subscriptions and 5 000 rows outside the window, %d with 100 and 50 — more than 2× (SC-001)",
			largePages, smallPages)
	}
	for label, seqs := range map[string][]string{"small": smallSeqs, "large": largeSeqs} {
		if len(seqs) > 0 {
			t.Errorf("badge plan (%s corpus) has Seq Scan on %v (SC-002)", label, seqs)
		}
	}
}

// TestWatchingCost_ColumnPagesBoundedByLiveSubscriptions — No. 43, SC-011 and
// SC-002 for the column (cards + totals, and the in-card timelines of the
// cards it returns).
func TestWatchingCost_ColumnPagesBoundedByLiveSubscriptions(t *testing.T) {
	pool, capture := newCardPlanPool(t)
	store := NewStore(pool)
	w := seedWatchingCostWorld(t, pool)
	seedWatchingCostBackground(t, pool, w, 50000)
	base := seedWatchingCostUser(t, pool, w, 100, 0, 0)
	noisy := seedWatchingCostUser(t, pool, w, 100, 5000, 10000)
	wide := seedWatchingCostUser(t, pool, w, 1000, 0, 0)
	analyzeWatchingCost(t, pool)

	measure := func(u uuid.UUID) (int64, []string) {
		var res WatchingFeedResult
		pages, seqs := measureWatchingRead(t, pool, capture, func() error {
			var err error
			res, err = store.ListWatchingFeed(context.Background(), u, WatchingStateAll)
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(res.Rows))
			for _, r := range res.Rows {
				ids = append(ids, r.IssueID)
			}
			_, err = store.HydrateWatchingRecentEvents(context.Background(), u, ids)
			return err
		})
		if res.UnreadTotal != watchingCostInWindow {
			t.Fatalf("unreadTotal = %d, want %d (the unread rows inside the window)", res.UnreadTotal, watchingCostInWindow)
		}
		return pages, seqs
	}
	basePages, baseSeqs := measure(base)
	noisyPages, noisySeqs := measure(noisy)
	widePages, wideSeqs := measure(wide)

	if basePages == 0 {
		t.Fatalf("column read 0 pages for the base corpus — nothing measured")
	}
	if noisyPages > 2*basePages {
		t.Errorf("column pages: %d with 5 000 rows outside the window and 10 000 opt-outs, %d without — more than 2× (SC-011)",
			noisyPages, basePages)
	}
	if widePages > 10*basePages {
		t.Errorf("column pages: %d with 1 000 live subscriptions, %d with 100 — more than 10× (SC-011)",
			widePages, basePages)
	}
	for label, seqs := range map[string][]string{"base": baseSeqs, "noisy": noisySeqs, "wide": wideSeqs} {
		if len(seqs) > 0 {
			t.Errorf("column plan (%s corpus) has Seq Scan on %v (SC-002)", label, seqs)
		}
	}
}

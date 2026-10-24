package handler

// notification_counts_cost_test.go — sk-622.21, T024, matrix row No. 51
// (SC-015, SC-001 and SC-002 for the header counts). Run: final.
//
// The statement computeCounts actually sends is captured with its arguments
// by the pgx tracer of review_unreviewed_index_plan_test.go (sqlCapture —
// the test never re-types the SQL), re-run as EXPLAIN (ANALYZE, BUFFERS,
// FORMAT JSON), and the pages of the top node (Shared Hit Blocks + Shared
// Read Blocks, inclusive) are compared. Tables are ANALYZEd after seeding.
//
// Literal bounds of the spec:
//
//   - SC-015: pages with 200 000 tasks of other users ≤ 2 × pages with
//     2 000, with the caller's own tasks unchanged; with 10 × the caller's
//     own active tasks ≤ 10 × pages;
//   - SC-001: pages with 20 000 subscriptions and 5 000 notifications
//     outside the window ≤ 2 × pages with 100 and 50, the same notifications
//     inside the window;
//   - SC-002: no Seq Scan on notifications, issue_watchers,
//     issue_watch_optouts, issues, dependencies or users.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// countsCostTables are the relations no plan node of the counts may Seq Scan.
var countsCostTables = map[string]bool{
	"notifications": true, "issue_watchers": true, "issue_watch_optouts": true,
	"issues": true, "dependencies": true, "users": true,
}

type countsCostNode struct {
	NodeType     string           `json:"Node Type"`
	RelationName string           `json:"Relation Name"`
	SharedHit    int64            `json:"Shared Hit Blocks"`
	SharedRead   int64            `json:"Shared Read Blocks"`
	Plans        []countsCostNode `json:"Plans"`
}

func (n countsCostNode) seqScans() []string {
	var out []string
	if n.NodeType == "Seq Scan" && countsCostTables[n.RelationName] {
		out = append(out, n.RelationName)
	}
	for _, c := range n.Plans {
		out = append(out, c.seqScans()...)
	}
	return out
}

// countsCostWorld is one project, the owner who creates and holds the other
// users' tasks, and the issue-id prefix.
type countsCostWorld struct {
	pool    *pgxpool.Pool
	project uuid.UUID
	owner   uuid.UUID
	prefix  string
	next    int // next free issue number
}

// seedOthersTasks adds n tasks created by and assigned to the owner (other
// users' work, from the callers' point of view), half of them closed.
func (w *countsCostWorld) seedOthersTasks(t *testing.T, n int) {
	t.Helper()
	ownerName := testsupport.Username(t, w.pool, w.owner)
	if _, err := w.pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    closed_at, closed_reason, close_outcome, created_by, created_at, updated_at, metadata)
SELECT $1 || '-' || g, $2, NULL, 1, 'others', '', 'task', 2,
       CASE WHEN g % 2 = 0 THEN 'closed' ELSE 'open' END,
       false, 'human', $5, '', '', '',
       CASE WHEN g % 2 = 0 THEN now() END, CASE WHEN g % 2 = 0 THEN '' END,
       CASE WHEN g % 2 = 0 THEN 'done' END,
       $3, now(), now(), '{}'::jsonb
  FROM generate_series($4::int, $4::int + $6::int - 1) g`,
		w.prefix, w.project, w.owner, w.next, ownerName, n); err != nil {
		t.Fatalf("seed others' tasks: %v", err)
	}
	w.next += n
}

// seedCaller creates a member whose own active tasks are `own` (created by
// the owner, assigned to the caller, half accepted), with `subs`
// subscriptions on the world's first issues, `outside` unread carded
// notifications 30 days old, and 50 unread carded ones an hour old.
func (w *countsCostWorld) seedCaller(t *testing.T, own, subs, outside int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	caller, name := testsupport.SeedUser(t, w.pool, "user")
	testsupport.AddProjectMember(t, w.pool, w.project, caller, "member")
	if _, err := w.pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, accepted_at, accepted_by, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-' || g, $2, NULL, 1, 'own', '', 'task', 2, 'open',
       false, 'human', $5,
       CASE WHEN g % 2 = 0 THEN now() END, CASE WHEN g % 2 = 0 THEN $6::uuid END,
       '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series($4::int, $4::int + $7::int - 1) g`,
		w.prefix, w.project, w.owner, w.next, name, caller, own); err != nil {
		t.Fatalf("seed own tasks: %v", err)
	}
	w.next += own
	if _, err := w.pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1 || '-' || g, $2, ARRAY['manual']::text[], now(), 'issue'
  FROM generate_series(1, $3) g`, w.prefix, caller, subs); err != nil {
		t.Fatalf("seed subscriptions: %v", err)
	}
	seed := func(n int, age string) {
		if _, err := w.pool.Exec(ctx, `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    SELECT now() - $5::interval, 'issue.status_changed', 'issue',
           $1 || '-' || (((g - 1) % $6) + 1), $2, 'tester', $3, 'human', '{}'::jsonb
      FROM generate_series(1, $4) g
    RETURNING id, entity_id, occurred_at
)
INSERT INTO notifications (user_id, event_id, type, project_id, created_at,
                           feed_channel, card_issue_id, card_resolved)
SELECT $7, ev.id, 'issue.status_changed', $2, ev.occurred_at, true, ev.entity_id, true
  FROM ev`, w.prefix, w.project, w.owner, n, age, subs, caller); err != nil {
			t.Fatalf("seed notifications (%s): %v", age, err)
		}
	}
	seed(50, "1 hour")
	if outside > 0 {
		seed(outside, "30 days")
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, caller)
	})
	return caller
}

// analyzeCountsCost brings the seeded tables to the steady state the
// production database is in: VACUUM ANALYZE refreshes the planner statistics
// (on unanalysed tables the planner rightly prefers a Seq Scan) and flushes
// the GIN pending lists (idx_issues_previous_assignees after a bulk insert
// keeps the new entries in a list every search reads whole, until autovacuum
// merges it — the measurement would be of the bulk insert, not of the query).
// seedBackgroundUsers adds n users — a directory of a realistic size, so the
// planner reads users by key and not by a scan of a handful of rows.
func seedBackgroundUsers(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	tag := "cc" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	// In statements of 100: the users trigger of 000060 takes an advisory
	// lock per handle, and one statement of thousands of users would exhaust
	// the lock table.
	for from := 1; from <= n; from += 100 {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
SELECT gen_random_uuid(), $1 || g || '@test.local', $1 || g, $1 || g, 'user', 'fake-hash', now(), '{}'::jsonb
  FROM generate_series($2::int, LEAST($2::int + 99, $3::int)) g`, tag, from, n); err != nil {
			t.Fatalf("seed background users: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username LIKE $1 || '%'`, tag)
	})
}

func analyzeCountsCost(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, tbl := range []string{"issues", "notifications", "issue_watchers", "issue_watch_optouts",
		"users", "project_members", "events", "user_notification_prefs", "projects"} {
		if _, err := pool.Exec(context.Background(), "VACUUM ANALYZE "+tbl); err != nil {
			t.Fatalf("vacuum analyze %s: %v", tbl, err)
		}
	}
}

// seedBackgroundNotifications gives the owner n notifications — the other
// users' rows of the table, so that the caller's rows are the small part of
// it they are in production.
func (w *countsCostWorld) seedBackgroundNotifications(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.pool.Exec(ctx, `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    SELECT now() - (g % 500) * interval '1 hour', 'issue.status_changed', 'issue',
           $1 || '-' || (((g - 1) % 1000) + 1), $2, 'tester', $3, 'human', '{}'::jsonb
      FROM generate_series(1, $4) g
    RETURNING id, entity_id, occurred_at
)
INSERT INTO notifications (user_id, event_id, type, project_id, created_at,
                           feed_channel, card_issue_id, card_resolved)
SELECT $3, ev.id, 'issue.status_changed', $2, ev.occurred_at, true, ev.entity_id, true
  FROM ev`, w.prefix, w.project, w.owner, n); err != nil {
		t.Fatalf("seed background notifications: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, w.owner)
	})
}

// measureCounts runs computeCounts for caller with the capture on and
// returns the pages of the statements it sent and the guarded Seq Scans.
func measureCounts(t *testing.T, pool *pgxpool.Pool, capture *sqlCapture, caller uuid.UUID) (int64, []string) {
	t.Helper()
	h := NewMeHandler(pool, nil, notifications.NewStore(pool))
	capture.reset()
	if _, err := h.computeCounts(context.Background(), caller); err != nil {
		t.Fatalf("computeCounts: %v", err)
	}
	capture.mu.Lock()
	stmts := append([]capturedStmt(nil), capture.stmts...)
	capture.mu.Unlock()
	// One counts statement, plus the caller's visible set read once from
	// the access relation (authz.Scope.Bind; sk-674.18 K-1 — a set of
	// project ids by the relation's own index, not a scan of the counted
	// tables). Pages and Seq Scans are measured on the counts statement.
	var counts []capturedStmt
	sets := 0
	for _, s := range stmts {
		if strings.Contains(s.sql, "skein_project_access_set(") {
			sets++
			continue
		}
		counts = append(counts, s)
	}
	if len(counts) != 1 || sets != 1 {
		t.Fatalf("computeCounts sent %d counts statement(s) and %d visible-set read(s), want exactly 1 and 1", len(counts), sets)
	}
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+counts[0].sql, counts[0].args...).Scan(&raw); err != nil {
		t.Fatalf("explain counts: %v", err)
	}
	var plan []struct {
		Plan countsCostNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) == 0 {
		t.Fatalf("parse plan: %v", err)
	}
	return plan[0].Plan.SharedHit + plan[0].Plan.SharedRead, plan[0].Plan.seqScans()
}

// TestNotificationCountsCost_PagesBoundedAndNoSeqScan — No. 51.
func TestNotificationCountsCost_PagesBoundedAndNoSeqScan(t *testing.T) {
	pool, capture := newTracingPool(t)
	// Seeded first, so their cleanup runs last — after the project's tasks
	// are gone, when deleting a user no longer checks 200 000 rows.
	seedBackgroundUsers(t, pool, 2000)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "ccost")
	w := &countsCostWorld{pool: pool, project: pid, owner: owner, prefix: prefix, next: 1}

	w.seedOthersTasks(t, 2000)
	w.seedBackgroundNotifications(t, 20000)
	base := w.seedCaller(t, 10, 100, 50)
	ownHeavy := w.seedCaller(t, 100, 100, 50)
	analyzeCountsCost(t, pool)

	basePages, baseSeqs := measureCounts(t, pool, capture, base)
	ownPages, ownSeqs := measureCounts(t, pool, capture, ownHeavy)
	if basePages == 0 {
		t.Fatalf("the counts read 0 pages — nothing measured")
	}
	if ownPages > 10*basePages {
		t.Errorf("pages: %d with 100 own active tasks, %d with 10 — more than 10× (SC-015)", ownPages, basePages)
	}

	// 20 000 subscriptions (on the world's first issues) and 5 000
	// notifications outside the window (SC-001).
	w.seedOthersTasks(t, 198000)
	subsHeavy := w.seedCaller(t, 10, 20000, 5000)
	analyzeCountsCost(t, pool)

	basePagesLarge, baseLargeSeqs := measureCounts(t, pool, capture, base)
	if basePagesLarge > 2*basePages {
		t.Errorf("pages: %d with 200 000 tasks of other users, %d with 2 000 — more than 2× (SC-015)",
			basePagesLarge, basePages)
	}
	subsPages, subsSeqs := measureCounts(t, pool, capture, subsHeavy)
	if subsPages > 2*basePagesLarge {
		t.Errorf("pages: %d with 20 000 subscriptions and 5 000 notifications outside the window, %d with 100 and 50 — more than 2× (SC-001)",
			subsPages, basePagesLarge)
	}

	for label, seqs := range map[string][]string{
		"base": baseSeqs, "own ×10": ownSeqs, "200 000 others": baseLargeSeqs, "20 000 subscriptions": subsSeqs,
	} {
		if len(seqs) > 0 {
			t.Errorf("%s: counts plan has Seq Scan on %s (SC-002)", label, strings.Join(seqs, ", "))
		}
	}
}

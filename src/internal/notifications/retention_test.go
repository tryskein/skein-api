package notifications_test

// retention_test.go — sk-622.21, test matrix row 38 (T019; US-8, INV-8,
// "Retention cleanup — no signal").
//
// Subject: notifications.RetentionRunner.Sweep against the real schema.
//
// Reference (spec US-8, INV-8, plan T019): notifications are kept
// NOTIFICATIONS_RETENTION_DAYS days; older ones are deleted in batches;
// mentions are never deleted whatever their age; a row whose push is still
// owed (push_state pending / leased) is not touched; the agents' feed (REST
// and MCP) holds no row older than the window except mentions; the
// «deleted» metric grows by exactly the number of deleted rows; the sweep
// sends no personal signal; replicas sweep concurrently without waiting on
// each other — each batch skips rows somebody else holds.
//
// The collision is guaranteed rather than hoped for: a second connection of
// the test holds FOR UPDATE on part of the eligible rows in an open
// transaction while the sweep runs with lock_timeout = 100ms in its session
// (a parameter of the test's pool, not of the product). A sweep that waited
// for those rows would fail on lock_timeout; one that skips them finishes,
// leaves them, and takes them on the next run once the lock is gone.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// retentionWindow is the product default of NOTIFICATIONS_RETENTION_DAYS.
const retentionWindow = 90 * 24 * time.Hour

// retentionDeletedMetric reads skein_notifications_retention_deleted_total
// from the default registry the product registers it in.
func retentionDeletedMetric(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "skein_notifications_retention_deleted_total" {
			continue
		}
		var sum float64
		for _, m := range f.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
		return sum
	}
	t.Fatal("metric skein_notifications_retention_deleted_total is not registered")
	return 0
}

// lockTimeoutPool is a pool whose every session runs with lock_timeout =
// 100ms: a statement that waits for a row lock longer than that fails.
func lockTimeoutPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testsupport.TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testsupport.TestDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", testsupport.TestDatabaseURL, err)
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "100ms"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("lock_timeout pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// retentionRow describes one seeded notification.
type retentionRow struct {
	name      string
	eventType string
	age       time.Duration
	feed      bool
	pushState string // none | pending | leased | final
}

func seedRetentionRow(t *testing.T, pool *pgxpool.Pool, user, actor, pid uuid.UUID, issueID string, r retentionRow) int64 {
	t.Helper()
	ctx := context.Background()
	created := time.Now().UTC().Add(-r.age)
	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES ($1, $2, 'issue', $3, $4, 'tester', $5, 'human', '{}'::jsonb)
RETURNING id`, created, r.eventType, issueID, pid, actor).Scan(&eventID); err != nil {
		t.Fatalf("seed event for %s: %v", r.name, err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel,
                           push_state, push_outcome, push_finalized_at,
                           push_next_attempt_at, push_deadline_at, push_leased_until,
                           card_resolved)
VALUES ($1, $2, $3, $4, $5, $6,
        $7,
        CASE WHEN $7 = 'final' THEN 'success' END,
        CASE WHEN $7 = 'final' THEN $5::timestamptz END,
        CASE WHEN $7 IN ('pending', 'leased') THEN now() END,
        CASE WHEN $7 IN ('pending', 'leased') THEN now() + interval '1 hour' END,
        CASE WHEN $7 = 'leased' THEN now() + interval '1 hour' END,
        true)
RETURNING id`, user, eventID, r.eventType, pid, created, r.feed, r.pushState).Scan(&id); err != nil {
		t.Fatalf("seed notification %s: %v", r.name, err)
	}
	return id
}

// existingIDs answers which of ids are still in the table.
func existingIDs(t *testing.T, pool *pgxpool.Pool, ids []int64) map[int64]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id FROM notifications WHERE id = ANY($1)`, ids)
	if err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// restFeedIDs reads GET /api/v1/notifications?scope=all as the user.
func restFeedIDs(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, username string) []int64 {
	t.Helper()
	h := handler.NewNotificationsHandler(pool)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			ctx := auth.WithUserID(req.Context(), user)
			ctx = auth.WithUsername(ctx, username)
			ctx = auth.WithUserRole(ctx, "user")
			next.ServeHTTP(rw, req.WithContext(ctx))
		})
	})
	r.Get("/notifications", h.ListNotifications)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/notifications?scope=all&limit=100", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /notifications → %d %s", rec.Code, rec.Body.String())
	}
	var body notifications.NotificationListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode REST feed: %v", err)
	}
	if body.HasMore {
		t.Fatal("REST feed has more than one page; the fixture holds a handful of rows")
	}
	out := make([]int64, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, it.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// mcpFeedIDs reads skein_notifications(action=inbox, scope=all) as the user
// through a real MCP server.
func mcpFeedIDs(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) []int64 {
	t.Helper()
	s := server.NewMCPServer("retention-test", "1", server.WithToolCapabilities(false))
	tools.RegisterNotifications(s, pool, notifications.NewStore(pool))
	ctx := auth.WithUserID(context.Background(), user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "mcp-session-"+uuid.NewString())
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "skein_notifications", "arguments": map[string]any{
			"action": "inbox", "scope": "all", "limit": float64(100),
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := json.Marshal(s.HandleMessage(ctx, msg))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var resp struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if resp.Error != nil || resp.Result.IsError || len(resp.Result.Content) == 0 {
		t.Fatalf("skein_notifications inbox failed: %s", raw)
	}
	var body struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
		HasMore bool `json:"hasMore"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &body); err != nil {
		t.Fatalf("decode MCP inbox %s: %v", resp.Result.Content[0].Text, err)
	}
	if body.HasMore {
		t.Fatal("MCP inbox has more than one page; the fixture holds a handful of rows")
	}
	out := make([]int64, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, it.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
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

// TestRetentionSweep_ExpiredNonMentionsGoMentionsAndOwedPushStayLockedRowsSkipped
// is row 38.
func TestRetentionSweep_ExpiredNonMentionsGoMentionsAndOwedPushStayLockedRowsSkipped(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	sweepPool := lockTimeoutPool(t)
	ctx := context.Background()

	runner, err := notifications.NewRetentionRunner(sweepPool, notifications.RetentionOptions{Retention: retentionWindow})
	if err != nil {
		t.Fatalf("NewRetentionRunner: %v", err)
	}
	// The table is shared with the rest of the suite: rows other tests left
	// behind that are already eligible go first, so the metric below counts
	// this fixture alone. Nothing else writes while this non-parallel test
	// runs (-p 1).
	if _, err := runner.Sweep(ctx); err != nil {
		t.Fatalf("preliminary sweep: %v", err)
	}

	reader, readerName := testsupport.SeedUser(t, pool, "")
	owner, _ := testsupport.SeedUser(t, pool, "")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "rtn")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, pid)
	})
	testsupport.AddProjectMember(t, pool, pid, reader, "member")
	issueID := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "retention issue", "open", 2)

	day := 24 * time.Hour
	fixture := []retentionRow{
		{name: "expired feed row", eventType: "issue.updated", age: 100 * day, feed: true, pushState: "none"},
		{name: "expired push-only row, delivery final", eventType: "issue.updated", age: 100 * day, feed: false, pushState: "final"},
		{name: "expired feed row, held by another connection (1)", eventType: "issue.status_changed", age: 100 * day, feed: true, pushState: "none"},
		{name: "expired feed row, held by another connection (2)", eventType: "issue.status_changed", age: 120 * day, feed: true, pushState: "none"},
		{name: "comment mention, very old", eventType: "comment.mention", age: 400 * day, feed: true, pushState: "none"},
		{name: "issue mention, very old", eventType: "issue.mention", age: 400 * day, feed: true, pushState: "none"},
		{name: "expired row, push pending", eventType: "issue.updated", age: 100 * day, feed: false, pushState: "pending"},
		{name: "expired row, push leased", eventType: "issue.updated", age: 100 * day, feed: false, pushState: "leased"},
		{name: "young feed row", eventType: "issue.updated", age: 10 * day, feed: true, pushState: "none"},
	}
	ids := map[string]int64{}
	all := make([]int64, 0, len(fixture))
	for _, r := range fixture {
		id := seedRetentionRow(t, pool, reader, owner, pid, issueID, r)
		ids[r.name] = id
		all = append(all, id)
	}
	freeExpired := []string{"expired feed row", "expired push-only row, delivery final"}
	held := []string{"expired feed row, held by another connection (1)", "expired feed row, held by another connection (2)"}
	kept := []string{"comment mention, very old", "issue mention, very old", "expired row, push pending", "expired row, push leased", "young feed row"}

	sink := startSigSink(t)
	mark := sink.mark()
	before := retentionDeletedMetric(t)

	// A second connection holds the two rows in an open transaction.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	holderOpen := true
	t.Cleanup(func() {
		if holderOpen {
			_ = holder.Rollback(context.Background())
		}
	})
	heldIDs := []int64{ids[held[0]], ids[held[1]]}
	if tag, err := holder.Exec(ctx, `SELECT id FROM notifications WHERE id = ANY($1) FOR UPDATE`, heldIDs); err != nil || tag.RowsAffected() != 2 {
		t.Fatalf("hold rows: tag=%v err=%v", tag, err)
	}

	n1, err := runner.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep while rows are held: %v (a batch waited for a held row instead of skipping it)", err)
	}
	left := existingIDs(t, pool, all)
	for _, name := range freeExpired {
		if left[ids[name]] {
			t.Errorf("first sweep: %q (older than the window, not held) is still there", name)
		}
	}
	for _, name := range held {
		if !left[ids[name]] {
			t.Errorf("first sweep: %q was held by another connection and must still be there", name)
		}
	}
	for _, name := range kept {
		if !left[ids[name]] {
			t.Errorf("first sweep: %q must never be deleted by the sweep", name)
		}
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("rollback holder: %v", err)
	}
	holderOpen = false

	n2, err := runner.Sweep(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	left = existingIDs(t, pool, all)
	for _, name := range append(append([]string{}, freeExpired...), held...) {
		if left[ids[name]] {
			t.Errorf("after both sweeps: %q is still there", name)
		}
	}
	for _, name := range kept {
		if !left[ids[name]] {
			t.Errorf("after both sweeps: %q must never be deleted by the sweep", name)
		}
	}

	// Four eligible rows, each deleted exactly once across the two runs.
	const eligible = 4
	if got := retentionDeletedMetric(t) - before; got != eligible {
		t.Errorf("skein_notifications_retention_deleted_total grew by %v over both sweeps, want %d", got, eligible)
	}
	if n1+n2 != eligible {
		t.Errorf("sweeps reported %d + %d deleted rows, want %d in total", n1, n2, eligible)
	}

	// The feed the agents read holds no row older than the window except
	// mentions. Push-only rows (pending, leased) are in no feed.
	wantFeed := []int64{ids["comment mention, very old"], ids["issue mention, very old"], ids["young feed row"]}
	sort.Slice(wantFeed, func(i, j int) bool { return wantFeed[i] < wantFeed[j] })
	if got := restFeedIDs(t, pool, reader, readerName); !equalIDs(got, wantFeed) {
		t.Errorf("REST feed ids = %v, want %v", got, wantFeed)
	}
	if got := mcpFeedIDs(t, pool, reader); !equalIDs(got, wantFeed) {
		t.Errorf("MCP inbox ids = %v, want %v", got, wantFeed)
	}

	// No personal signal: the sweep names nobody.
	if msgs := sink.settle(t, pool, mark); len(msgs) != 0 {
		t.Errorf("the retention sweep published %d personal signal(s), want none: %+v", len(msgs), msgs)
	}
}

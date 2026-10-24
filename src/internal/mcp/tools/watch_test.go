package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
)

// TestWatchTool_HappyPath fires skein_watchers(action="watch") on a
// fresh issue and
// verifies the row appears in issue_watchers with reason='manual'.
func TestWatchTool_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "wht")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "watch happy", "open", 2)

	notifStore := notifications.NewStore(pool)
	handler := handleWatch(pool, notifStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"issueId": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %s", extractToolJSON(t, res))
	}

	body := extractToolJSON(t, res)
	if got, _ := body["watching"].(bool); !got {
		t.Errorf("expected watching=true, got %v", body)
	}

	// Verify DB state.
	var reasons []string
	err = pool.QueryRow(context.Background(),
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&reasons)
	if err != nil {
		t.Fatalf("query reasons: %v", err)
	}
	found := false
	for _, r := range reasons {
		if r == "manual" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected reason 'manual' in %v", reasons)
	}
}

// TestWatchTool_Subtree verifies watch with subtree=true writes
// scope='subtree' on the issue_watchers row, and a subsequent
// watch (no subtree) does NOT downgrade it (monotonic dominance,
// sk-142.8 / spec §7).
func TestWatchTool_Subtree(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "wsub")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "watch subtree", "open", 2)

	notifStore := notifications.NewStore(pool)
	handler := handleWatch(pool, notifStore)
	ctx := withCtx(t, userID, "user")

	readScope := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(context.Background(),
			`SELECT scope FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
			issueID, userID).Scan(&s); err != nil {
			t.Fatalf("read scope: %v", err)
		}
		return s
	}

	// subtree=true → scope='subtree'.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"issueId": issueID, "subtree": true}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("subtree watch failed: err=%v body=%s", err, extractToolJSON(t, res))
	}
	if s := readScope(); s != notifications.ScopeSubtree {
		t.Fatalf("after subtree=true: scope=%q, want %q", s, notifications.ScopeSubtree)
	}

	// Plain watch (no subtree) must NOT downgrade.
	req2 := mcpgo.CallToolRequest{}
	req2.Params.Arguments = map[string]any{"issueId": issueID}
	res2, err := handler(ctx, req2)
	if err != nil || res2.IsError {
		t.Fatalf("plain watch failed: err=%v body=%s", err, extractToolJSON(t, res2))
	}
	if s := readScope(); s != notifications.ScopeSubtree {
		t.Fatalf("plain watch downgraded scope to %q, want %q", s, notifications.ScopeSubtree)
	}
}

// TestWatchTool_IssueNotFound returns issue_not_found for a missing id.
func TestWatchTool_IssueNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	notifStore := notifications.NewStore(pool)
	handler := handleWatch(pool, notifStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"issueId": "nope-999"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error result, got %s", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if got, _ := body["error"].(string); got != ErrCodeIssueNotFound {
		t.Errorf("expected %q, got %q", ErrCodeIssueNotFound, got)
	}
}

// TestFilterWatchTool_CreateAndDelete exercises the create→list→delete
// round-trip for a custom_watcher via the MCP tool handlers.
//
// sk-255.3 split the one handleFilterWatch (which switched on its own `op`
// argument) into three handlers, one per top-level action, so the
// round-trip now walks three functions instead of one. The assertions are
// unchanged — the same three store calls in the same order — which is what
// makes this test evidence that flattening `op` into `action` moved the
// dispatch and nothing else.
func TestFilterWatchTool_CreateAndDelete(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	notifStore := notifications.NewStore(pool)
	ctx := withCtx(t, userID, "user")

	// Create.
	createReq := mcpgo.CallToolRequest{}
	createReq.Params.Arguments = map[string]any{
		"name": "tool_test_watcher",
		"filter": map[string]any{
			"event_types": []any{"issue.created", "comment.created"},
		},
	}
	res, err := handleFilterWatchCreate(pool, notifStore)(ctx, createReq)
	if err != nil || res.IsError {
		t.Fatalf("create failed: err=%v body=%s", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	w, ok := body["watcher"].(map[string]any)
	if !ok {
		t.Fatalf("expected watcher object, got %v", body)
	}
	id, _ := w["id"].(string)
	if id == "" {
		t.Fatalf("expected id in watcher response: %v", w)
	}

	// List.
	listReq := mcpgo.CallToolRequest{}
	listReq.Params.Arguments = map[string]any{}
	res, err = handleFilterWatchList(pool, notifStore)(ctx, listReq)
	if err != nil || res.IsError {
		t.Fatalf("list failed: err=%v body=%s", err, extractToolJSON(t, res))
	}
	listBody := extractToolJSON(t, res)
	watchers, _ := listBody["watchers"].([]any)
	if len(watchers) == 0 {
		t.Errorf("expected at least one watcher after create")
	}

	// Delete. The watcher UUID travels as `filterId`: inside the
	// consolidated schema `id` was already taken by the issue-side
	// identifier, which is a hierarchical id and not a UUID at all.
	delReq := mcpgo.CallToolRequest{}
	delReq.Params.Arguments = map[string]any{"filterId": id}
	res, err = handleFilterWatchDelete(pool, notifStore)(ctx, delReq)
	if err != nil || res.IsError {
		t.Fatalf("delete failed: err=%v body=%s", err, extractToolJSON(t, res))
	}

	// Verify deletion.
	var n int
	pool.QueryRow(context.Background(),
		`SELECT count(*) FROM custom_watchers WHERE user_id = $1`, userID).Scan(&n)
	if n != 0 {
		t.Errorf("expected zero custom watchers after delete, got %d", n)
	}
}

// TestInboxTool_SmokeTest verifies skein_notifications(action="inbox")
// returns a well-formed
// envelope (items, hasMore) even when the inbox is empty.
func TestInboxTool_SmokeTest(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	notifStore := notifications.NewStore(pool)
	handler := handleInbox(notifStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("inbox failed: err=%v body=%s", err, extractToolJSON(t, res))
	}

	body := extractToolJSON(t, res)
	if _, ok := body["items"]; !ok {
		t.Errorf("expected items field, got %v", body)
	}
	if _, ok := body["hasMore"]; !ok {
		t.Errorf("expected hasMore field, got %v", body)
	}
}

// extractToolJSON is defined in show_test.go.

// ---- sk-622.21, T010, matrix row No. 20: the MCP paths go through the
// subscription primitives (US-1, US-6, R3). Expected values are the US-1
// rules: an explicit unwatch of a subscription deletes it, records the
// opt-out and clears the caller's Watching card of the issue; without a
// subscription only the card is cleared (R3); a manual watch deletes the
// opt-out; skein_drop_delegated is an explicit unwatch too.

// seedWatchCard gives userID a feed notification whose Watching card is
// issueID.
func seedWatchCard(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, issueID string, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var eid int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', 'human', '{}'::jsonb)
RETURNING id`, issueID, projectID).Scan(&eid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, reason, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, 'issue.status_changed', $3, 'delegated_status', true, $4, true)`,
		userID, eid, projectID, issueID); err != nil {
		t.Fatalf("seed carded notification: %v", err)
	}
}

func watchCardRows(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, issueID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND card_issue_id = $2`, userID, issueID).
		Scan(&n); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	return n
}

func watchRowExists(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, userID).
		Scan(&n); err != nil {
		t.Fatalf("count watcher rows: %v", err)
	}
	return n > 0
}

func watchOptOutExists(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, issueID, userID).
		Scan(&n); err != nil {
		t.Fatalf("count opt-outs: %v", err)
	}
	return n > 0
}

func callWatchTool(t *testing.T, h server.ToolHandlerFunc, ctx context.Context, args map[string]any) {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %v", extractToolJSON(t, res))
	}
}

// TestWatchTool_UnwatchRecordsOptOutAndClearsTheCard — No. 20.
func TestWatchTool_UnwatchRecordsOptOutAndClearsTheCard(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "wuo")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "unwatch with opt-out", "open", 2)

	notifStore := notifications.NewStore(pool)
	ctx := withCtx(t, userID, "user")
	callWatchTool(t, handleWatch(pool, notifStore), ctx, map[string]any{"issueId": issueID})
	seedWatchCard(t, pool, pid, issueID, userID)
	seedWatchCard(t, pool, pid, issueID, other)

	callWatchTool(t, handleUnwatch(pool, notifStore), ctx, map[string]any{"issueId": issueID})

	if watchRowExists(t, pool, issueID, userID) {
		t.Errorf("subscription row left after unwatch (want no row, no tombstone)")
	}
	if !watchOptOutExists(t, pool, issueID, userID) {
		t.Errorf("unwatch of a subscription recorded no opt-out")
	}
	if n := watchCardRows(t, pool, userID, issueID); n != 0 {
		t.Errorf("caller still has %d notifications carded to the issue", n)
	}
	if n := watchCardRows(t, pool, other, issueID); n != 1 {
		t.Errorf("another user's card: %d rows, want 1", n)
	}
}

// TestWatchTool_UnwatchHistoricalCardWritesNoOptOut — No. 20, R3.
func TestWatchTool_UnwatchHistoricalCardWritesNoOptOut(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "wuh")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "historical card", "open", 2)
	seedWatchCard(t, pool, pid, issueID, userID)

	notifStore := notifications.NewStore(pool)
	callWatchTool(t, handleUnwatch(pool, notifStore), withCtx(t, userID, "user"),
		map[string]any{"issueId": issueID})

	if n := watchCardRows(t, pool, userID, issueID); n != 0 {
		t.Errorf("historical card still on %d rows", n)
	}
	if watchOptOutExists(t, pool, issueID, userID) {
		t.Errorf("unwatch without a subscription recorded an opt-out (R3)")
	}
	if watchRowExists(t, pool, issueID, userID) {
		t.Errorf("unwatch without a subscription left a row")
	}
}

// TestWatchTool_WatchRemovesOptOut — No. 20.
func TestWatchTool_WatchRemovesOptOut(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "wwo")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "watch after unwatch", "open", 2)

	notifStore := notifications.NewStore(pool)
	ctx := withCtx(t, userID, "user")
	callWatchTool(t, handleWatch(pool, notifStore), ctx, map[string]any{"issueId": issueID})
	callWatchTool(t, handleUnwatch(pool, notifStore), ctx, map[string]any{"issueId": issueID})
	if !watchOptOutExists(t, pool, issueID, userID) {
		t.Fatalf("precondition: opt-out recorded by unwatch")
	}

	callWatchTool(t, handleWatch(pool, notifStore), ctx, map[string]any{"issueId": issueID})
	if watchOptOutExists(t, pool, issueID, userID) {
		t.Errorf("opt-out survived skein_watchers watch")
	}
	if !watchRowExists(t, pool, issueID, userID) {
		t.Errorf("watch after unwatch did not subscribe")
	}
}

// TestDropDelegated_UnwatchesWithOptOutAndClearsTheCard — No. 20:
// skein_drop_delegated stops the caller's subscription through the same
// primitive.
func TestDropDelegated_UnwatchesWithOptOutAndClearsTheCard(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "wdd")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "drop delegated", "open", 2)

	notifStore := notifications.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-wdd")
	callWatchTool(t, handleWatch(pool, notifStore), ctx, map[string]any{"issueId": issueID})
	seedWatchCard(t, pool, pid, issueID, userID)

	callWatchTool(t, handleDelegationAction(pool, issues.NewStore(pool), true), ctx,
		map[string]any{"id": issueID})

	if watchRowExists(t, pool, issueID, userID) {
		t.Errorf("subscription row left after skein_drop_delegated")
	}
	if !watchOptOutExists(t, pool, issueID, userID) {
		t.Errorf("skein_drop_delegated recorded no opt-out")
	}
	if n := watchCardRows(t, pool, userID, issueID); n != 0 {
		t.Errorf("card still on %d rows after skein_drop_delegated", n)
	}
}

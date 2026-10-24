package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestComment_HappyPath inserts a comment and verifies the persisted
// row carries author_type='agent' and author_kind='agent_directed'
// (sk-16 — agent caller + kind="directed").
func TestComment_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ccm")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "agent says hi",
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	cmt, ok := body["comment"].(map[string]any)
	if !ok {
		t.Fatalf("comment not present or wrong type: %T", body["comment"])
	}
	if cmt["authorType"] != "agent" {
		t.Fatalf("authorType = %v, want \"agent\"", cmt["authorType"])
	}
	if cmt["authorKind"] != "agent_directed" {
		t.Fatalf("authorKind = %v, want \"agent_directed\"", cmt["authorKind"])
	}
	commentID, _ := cmt["id"].(string)
	if commentID == "" {
		t.Fatalf("missing comment id")
	}

	// Verify persisted row.
	var dbAuthorType, dbAuthorKind, dbBody string
	if err := pool.QueryRow(context.Background(), `
SELECT author_type, author_kind, body FROM comments WHERE id = $1`,
		commentID).Scan(&dbAuthorType, &dbAuthorKind, &dbBody); err != nil {
		t.Fatalf("verify comment row: %v", err)
	}
	if dbAuthorType != "agent" {
		t.Fatalf("db author_type = %q, want \"agent\"", dbAuthorType)
	}
	if dbAuthorKind != "agent_directed" {
		t.Fatalf("db author_kind = %q, want \"agent_directed\"", dbAuthorKind)
	}
	if dbBody != "agent says hi" {
		t.Fatalf("db body = %q, want %q", dbBody, "agent says hi")
	}

	// Event should be agent-typed too.
	var evtActorType, evtSession string
	if err := pool.QueryRow(context.Background(), `
SELECT actor_type, COALESCE(actor_session_id, '')
  FROM events
 WHERE entity_type = 'comment' AND entity_id = $1
   AND type = 'comment.created'`,
		commentID).Scan(&evtActorType, &evtSession); err != nil {
		t.Fatalf("verify comment event: %v", err)
	}
	if evtActorType != "agent" {
		t.Fatalf("event actor_type = %q, want \"agent\"", evtActorType)
	}
	if evtSession == "" {
		t.Fatalf("event actor_session_id should be populated")
	}
}

// TestComment_Autonomous — kind="autonomous" maps to
// author_kind='agent_autonomous'.
func TestComment_Autonomous(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ccma")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "self-discovered side-quest note",
		"kind":    "autonomous",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	cmt, _ := body["comment"].(map[string]any)
	if cmt["authorKind"] != "agent_autonomous" {
		t.Fatalf("authorKind = %v, want \"agent_autonomous\"", cmt["authorKind"])
	}
}

// TestComment_KindRequiredForAgent — agent caller (api-key in ctx) MUST
// pass `kind`; omission → validation_error. sk-16.
func TestComment_KindRequiredForAgent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ccmk")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "missing kind",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing kind on agent caller")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestComment_KindUnknownValue — unknown `kind` value → validation_error.
func TestComment_KindUnknownValue(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ccmu")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "irrelevant",
		"kind":    "manual",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for unknown kind")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestComment_EmptyBody asserts whitespace-only body is rejected as a
// validation_error before any DB hit.
func TestComment_EmptyBody(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ccmeb")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "   \t\n  ",
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for empty body")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestComment_IssueNotVisible — non-member user gets issue_not_found
// (not forbidden) for visibility safety.
func TestComment_IssueNotVisible(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "ccmnv")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "private", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "should not land",
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-member access")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error = %v, want %s (no leak via 'forbidden')", got, ErrCodeIssueNotFound)
	}
}

// TestComment_BareNumericIDRejected ensures the hierarchical-id guard
// fires.
func TestComment_BareNumericIDRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	issuesStore := issues.NewStore(pool)
	handler := handleComment(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": "42",
		"body":    "irrelevant",
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bare numeric id")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

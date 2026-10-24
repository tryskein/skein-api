package tools

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestDepRemove_HappyPath adds a related edge then removes it. Asserts
// the success body and that the edge is gone from the table.
func TestDepRemove_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "drh")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	addH := handleDepAdd(pool, depsStore, issuesStore)
	rmH := handleDepRemove(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	// Seed the edge via skein_dep_add.
	addReq := mcpgo.CallToolRequest{}
	addReq.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepRelated,
	}
	if res, err := addH(ctx, addReq); err != nil || res.IsError {
		t.Fatalf("seed dep: err=%v isErr=%v", err, res != nil && res.IsError)
	}

	// Remove it.
	rmReq := mcpgo.CallToolRequest{}
	rmReq.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepRelated,
	}
	res, err := rmH(ctx, rmReq)
	if err != nil {
		t.Fatalf("dep_remove handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError removing edge: %+v", body)
	}
	body := extractToolJSON(t, res)
	if body["removed"] != true {
		t.Fatalf("removed = %v, want true", body["removed"])
	}

	// The edge must be gone from the table.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = $3`,
		a, b, dependencies.DepRelated).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 0 {
		t.Fatalf("edge count after remove = %d, want 0", n)
	}
}

// TestDepRemove_NotFound — removing an edge that doesn't exist returns
// dependency_not_found (not a silent success).
func TestDepRemove_NotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "drnf")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	rmH := handleDepRemove(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	rmReq := mcpgo.CallToolRequest{}
	rmReq.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepBlocks,
	}
	res, err := rmH(ctx, rmReq)
	if err != nil {
		t.Fatalf("dep_remove handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing edge")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeDependencyNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeDependencyNotFound)
	}
}

// TestDepAdd_SecondParentRejected — adding a parent-child edge to an issue
// that already has a (different) parent returns parent_already_set
// (Defect 2 guard). The first parent edge stays put.
func TestDepAdd_SecondParentRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dasp")
	parentX := seedIssue(t, pool, pid, userID, prefix+"-1", "parent X", "open", 2)
	parentY := seedIssue(t, pool, pid, userID, prefix+"-2", "parent Y", "open", 2)
	child := seedIssue(t, pool, pid, userID, prefix+"-3", "child", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	addH := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	// First parent: child -> parentX. Succeeds.
	first := mcpgo.CallToolRequest{}
	first.Params.Arguments = map[string]any{
		"fromId": child, "toId": parentX, "type": dependencies.DepParentChild,
	}
	if res, err := addH(ctx, first); err != nil || res.IsError {
		body := map[string]any{}
		if res != nil {
			body = extractToolJSON(t, res)
		}
		t.Fatalf("seed first parent: err=%v body=%+v", err, body)
	}

	// Second parent: child -> parentY. Must be rejected with parent_already_set.
	second := mcpgo.CallToolRequest{}
	second.Params.Arguments = map[string]any{
		"fromId": child, "toId": parentY, "type": dependencies.DepParentChild,
	}
	res, err := addH(ctx, second)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for second parent")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeParentAlreadySet {
		t.Fatalf("error = %v, want %s", got, ErrCodeParentAlreadySet)
	}

	// The original parent edge (X) must still be the only one.
	var to string
	if err := pool.QueryRow(context.Background(), `
SELECT to_id FROM dependencies WHERE from_id = $1 AND type = 'parent-child'`,
		child).Scan(&to); err != nil {
		t.Fatalf("read surviving parent edge: %v", err)
	}
	if to != parentX {
		t.Fatalf("surviving parent = %s, want %s (original)", to, parentX)
	}
}

// TestDepRemove_LastOutcomeLinkRefused — sk-540.8, MCP surface: the last
// `supersedes` edge into an issue closed as "superseded" is not removable;
// the answer is close_outcome_link_required naming the closed issue, and
// the edge stays.
func TestDepRemove_LastOutcomeLinkRefused(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "drol")
	replacement := seedIssue(t, pool, pid, userID, prefix+"-1", "replacement", "open", 2)
	replaced := seedIssue(t, pool, pid, userID, prefix+"-2", "replaced", "closed", 2)
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET close_outcome = 'superseded' WHERE id = $1`, replaced); err != nil {
		t.Fatalf("set outcome: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'supersedes', '{}'::jsonb, 1, $3, now())`, replacement, replaced, userID); err != nil {
		t.Fatalf("seed supersedes edge: %v", err)
	}

	rmH := handleDepRemove(pool, dependencies.NewStore(pool), issues.NewStore(pool))
	rmReq := mcpgo.CallToolRequest{}
	rmReq.Params.Arguments = map[string]any{
		"fromId": replacement, "toId": replaced, "type": dependencies.DepSupersedes,
	}
	res, err := rmH(withCtx(t, userID, "admin"), rmReq)
	if err != nil {
		t.Fatalf("dep_remove handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for the last supersedes edge")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeCloseOutcomeLinkRequired {
		t.Fatalf("error = %v, want %s", got, ErrCodeCloseOutcomeLinkRequired)
	}
	if body["issueId"] != replaced || body["closeOutcome"] != "superseded" {
		t.Fatalf("body = %v, want issueId %s and closeOutcome superseded", body, replaced)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'supersedes'`,
		replacement, replaced).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 1 {
		t.Fatalf("edge count after refusal = %d, want 1", n)
	}
}

// TestDepRemove_LastOutcomeLinkRefused_HiddenClosedIssue — sk-540.8 review,
// MCP surface: the last `supersedes` edge into an issue closed as
// "superseded" in a project the caller cannot see is still refused, but the
// refusal carries neither the issue id nor its outcome, and the message does
// not name it (sk-423: the reader learns nothing about issues it cannot see).
func TestDepRemove_LastOutcomeLinkRefused_HiddenClosedIssue(t *testing.T) {
	pool := newTestPool(t)
	callerID, _ := seedUser(t, pool, "user")
	ownerID, _ := seedUser(t, pool, "user")
	pidA, prefixA := seedProject(t, pool, callerID, "drha")
	pidB, prefixB := seedProject(t, pool, ownerID, "drhb")
	replacement := seedIssue(t, pool, pidA, callerID, prefixA+"-1", "replacement", "open", 2)
	hidden := seedIssue(t, pool, pidB, ownerID, prefixB+"-1", "replaced", "closed", 2)
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET close_outcome = 'superseded' WHERE id = $1`, hidden); err != nil {
		t.Fatalf("set outcome: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'supersedes', '{}'::jsonb, 1, $3, now())`, replacement, hidden, ownerID); err != nil {
		t.Fatalf("seed supersedes edge: %v", err)
	}

	rmH := handleDepRemove(pool, dependencies.NewStore(pool), issues.NewStore(pool))
	rmReq := mcpgo.CallToolRequest{}
	rmReq.Params.Arguments = map[string]any{
		"fromId": replacement, "toId": hidden, "type": dependencies.DepSupersedes,
	}
	res, err := rmH(withCtx(t, callerID, "user"), rmReq)
	if err != nil {
		t.Fatalf("dep_remove handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for the last supersedes edge into a hidden issue")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeCloseOutcomeLinkRequired {
		t.Fatalf("error = %v, want %s", got, ErrCodeCloseOutcomeLinkRequired)
	}
	if got := body["type"]; got != dependencies.DepSupersedes {
		t.Errorf("type = %v, want %s", got, dependencies.DepSupersedes)
	}
	for _, key := range []string{"issueId", "closeOutcome"} {
		if v, present := body[key]; present {
			t.Errorf("refusal carries %s=%v about an issue the caller cannot see", key, v)
		}
	}
	raw := res.Content[0].(mcpgo.TextContent).Text
	if strings.Contains(raw, hidden) {
		t.Errorf("refusal names the hidden issue %s: %s", hidden, raw)
	}
	if strings.Contains(raw, "superseded") {
		t.Errorf("refusal reveals the hidden issue's outcome: %s", raw)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'supersedes'`,
		replacement, hidden).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 1 {
		t.Fatalf("edge count after refusal = %d, want 1", n)
	}
}

package tools

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestDepAdd_HappyPathBlocks creates two issues in the same project and
// adds a `blocks` dep between them. Asserts the wire shape.
func TestDepAdd_HappyPathBlocks(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dah")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "blocked", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "blocker", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a,
		"toId":   b,
		"type":   dependencies.DepBlocks,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	dep, ok := body["dependency"].(map[string]any)
	if !ok {
		t.Fatalf("dependency missing or wrong type: %T", body["dependency"])
	}
	if dep["from"] != a {
		t.Fatalf("from = %v, want %s", dep["from"], a)
	}
	if dep["to"] != b {
		t.Fatalf("to = %v, want %s", dep["to"], b)
	}
	if dep["type"] != dependencies.DepBlocks {
		t.Fatalf("type = %v, want %s", dep["type"], dependencies.DepBlocks)
	}
	if dep["gate"] != nil {
		t.Fatalf("gate should be null for blocks, got %v", dep["gate"])
	}
}

// TestDepAdd_Cycle inserts a→b blocks edge then attempts b→a, which
// closes a cycle and must return dependency_cycle.
func TestDepAdd_Cycle(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dac")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	// First edge: a blocks b
	first := mcpgo.CallToolRequest{}
	first.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepBlocks,
	}
	if res, err := handler(ctx, first); err != nil || res.IsError {
		t.Fatalf("seed first dep: err=%v isErr=%v", err, res != nil && res.IsError)
	}

	// Second edge: b blocks a — should close cycle.
	second := mcpgo.CallToolRequest{}
	second.Params.Arguments = map[string]any{
		"fromId": b, "toId": a, "type": dependencies.DepBlocks,
	}
	res, err := handler(ctx, second)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for cycle")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeDependencyCycle {
		t.Fatalf("error = %v, want %s", got, ErrCodeDependencyCycle)
	}
}

// TestDepAdd_AlreadyExists creates the same edge twice; the second call
// returns dependency_already_exists.
func TestDepAdd_AlreadyExists(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dae")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	makeReq := func() mcpgo.CallToolRequest {
		r := mcpgo.CallToolRequest{}
		r.Params.Arguments = map[string]any{
			"fromId": a, "toId": b, "type": dependencies.DepRelated,
		}
		return r
	}
	if res, err := handler(ctx, makeReq()); err != nil || res.IsError {
		t.Fatalf("seed first dep: err=%v isErr=%v", err, res != nil && res.IsError)
	}
	res, err := handler(ctx, makeReq())
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for already-exists")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeDependencyAlreadyExists {
		t.Fatalf("error = %v, want %s", got, ErrCodeDependencyAlreadyExists)
	}
}

// TestDepAdd_FromNotVisible — caller cannot see the from-side project,
// so the FROM lookup fails and we return issue_not_found.
func TestDepAdd_FromNotVisible(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "dafnv")
	a := seedIssue(t, pool, pid, owner, prefix+"-1", "private", "open", 2)
	b := seedIssue(t, pool, pid, owner, prefix+"-2", "private2", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepBlocks,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-member access")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeIssueNotFound)
	}
}

// TestDepAdd_CrossProjectVisible — both projects visible to caller →
// success.
func TestDepAdd_CrossProjectVisible(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pidA, prefA := seedProject(t, pool, userID, "dacv1")
	pidB, prefB := seedProject(t, pool, userID, "dacv2")
	_ = pidA
	_ = pidB
	a := seedIssue(t, pool, pidA, userID, prefA+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pidB, userID, prefB+"-1", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepRelated,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}
}

// TestDepAdd_CrossProjectToNotVisible — caller can see the FROM project
// but not the TO; the answer is that of a nonexistent target (INV-3,
// sk-674.18 T033): issue_not_found with the same message.
func TestDepAdd_CrossProjectToNotVisible(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	// Stranger owns project A.
	pidA, prefA := seedProject(t, pool, stranger, "dacnv1")
	// Owner owns project B (stranger cannot see it).
	pidB, prefB := seedProject(t, pool, owner, "dacnv2")
	a := seedIssue(t, pool, pidA, stranger, prefA+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pidB, owner, prefB+"-1", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepRelated,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for invisible target project")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeIssueNotFound)
	}
	missingReq := mcpgo.CallToolRequest{}
	missingReq.Params.Arguments = map[string]any{
		"fromId": a, "toId": prefB + "-999", "type": dependencies.DepRelated,
	}
	missingRes, err := handler(ctx, missingReq)
	if err != nil {
		t.Fatalf("handler (missing): %v", err)
	}
	missing := extractToolJSON(t, missingRes)
	if body["message"] != missing["message"] || body["error"] != missing["error"] {
		t.Errorf("an invisible target answers %v / %v, a nonexistent one %v / %v",
			body["error"], body["message"], missing["error"], missing["message"])
	}
}

// TestDepAdd_WaitsForWithGate — happy path for waits-for + gate=all-children.
func TestDepAdd_WaitsForWithGate(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dawg")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepWaitsFor,
		"gate": dependencies.GateAllChildren,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}
	body := extractToolJSON(t, res)
	dep, _ := body["dependency"].(map[string]any)
	if dep["gate"] != dependencies.GateAllChildren {
		t.Fatalf("gate = %v, want %s", dep["gate"], dependencies.GateAllChildren)
	}
}

// TestDepAdd_GateOnNonWaitsForRejected — gate is only valid for waits-for.
func TestDepAdd_GateOnNonWaitsForRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dagn")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": dependencies.DepBlocks,
		"gate": dependencies.GateAnyChildren,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for gate on non-waits-for")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestDepAdd_ParentChildAccepted — sk-35 restored parent-child as the
// canonical hierarchy dep type. Supplying it on a fresh pair should
// succeed (the FROM-side has no other parent-child edge yet so no
// cycle).
func TestDepAdd_ParentChildAccepted(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dapc")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)
	b := seedIssue(t, pool, pid, userID, prefix+"-2", "b", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": a, "toId": b, "type": "parent-child",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError=false for parent-child accepted; body=%+v", body)
	}
}

// TestDepAdd_BareNumericIDRejected — bare numeric ids are explicitly
// rejected; only full hierarchical ids are accepted.
func TestDepAdd_BareNumericIDRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAdd(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": "42", "toId": "demo-9", "type": dependencies.DepBlocks,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bare numeric id")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

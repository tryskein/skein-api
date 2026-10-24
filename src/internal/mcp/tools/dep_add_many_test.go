package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestDepAddMany_CycleSecondRowFailsFirstPersists is the marquee
// acceptance test from the plan: insert [A→B blocks, B→A blocks] in
// one batch. The first row persists; the second sees the live graph
// state (post-row-1) and fails with dependency_cycle.
func TestDepAddMany_CycleSecondRowFailsFirstPersists(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "damc")

	idA := prefix + "-1"
	idB := prefix + "-2"
	seedIssue(t, pool, pid, userID, idA, "A", "open", 2)
	seedIssue(t, pool, pid, userID, idB, "B", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAddMany(pool, depsStore, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-cycle")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"fromId": idA,
				"toId":   idB,
				"type":   dependencies.DepBlocks,
			},
			map[string]any{
				"fromId": idB,
				"toId":   idA,
				"type":   dependencies.DepBlocks,
			},
		},
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}

	row0 := results[0].(map[string]any)
	if okFlag, _ := row0["ok"].(bool); !okFlag {
		t.Fatalf("row0 (A→B) should succeed: %+v", row0)
	}

	row1 := results[1].(map[string]any)
	if okFlag, _ := row1["ok"].(bool); okFlag {
		t.Fatalf("row1 (B→A) should fail with cycle: %+v", row1)
	}
	errObj := row1["error"].(map[string]any)
	if got := errObj["code"]; got != ErrCodeDependencyCycle {
		t.Fatalf("row1 error.code = %v, want %s", got, ErrCodeDependencyCycle)
	}

	// Verify A→B persisted; B→A did NOT.
	var nAB, nBA int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'blocks'`,
		idA, idB).Scan(&nAB); err != nil {
		t.Fatalf("query A→B: %v", err)
	}
	if nAB != 1 {
		t.Fatalf("A→B count = %d, want 1", nAB)
	}
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'blocks'`,
		idB, idA).Scan(&nBA); err != nil {
		t.Fatalf("query B→A: %v", err)
	}
	if nBA != 0 {
		t.Fatalf("B→A count = %d, want 0", nBA)
	}
}

// TestDepAddMany_PartialSuccessMixed: one row has invalid type, the
// rest persist.
func TestDepAddMany_PartialSuccessMixed(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "damp")

	idA := prefix + "-1"
	idB := prefix + "-2"
	idC := prefix + "-3"
	seedIssue(t, pool, pid, userID, idA, "A", "open", 2)
	seedIssue(t, pool, pid, userID, idB, "B", "open", 2)
	seedIssue(t, pool, pid, userID, idC, "C", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAddMany(pool, depsStore, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-mixed")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"fromId": idA,
				"toId":   idB,
				"type":   dependencies.DepRelated,
			},
			map[string]any{
				"fromId": idA,
				"toId":   idC,
				"type":   "not-a-real-type",
			},
			map[string]any{
				"fromId": idB,
				"toId":   idC,
				"type":   dependencies.DepRelated,
			},
		},
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	results := body["results"].([]any)

	if okFlag, _ := results[0].(map[string]any)["ok"].(bool); !okFlag {
		t.Fatalf("row0 should succeed: %+v", results[0])
	}
	if okFlag, _ := results[1].(map[string]any)["ok"].(bool); okFlag {
		t.Fatalf("row1 should fail validation: %+v", results[1])
	}
	row1Err := results[1].(map[string]any)["error"].(map[string]any)
	if got := row1Err["code"]; got != ErrCodeValidationError {
		t.Fatalf("row1 error.code = %v, want %s", got, ErrCodeValidationError)
	}
	if okFlag, _ := results[2].(map[string]any)["ok"].(bool); !okFlag {
		t.Fatalf("row2 should succeed: %+v", results[2])
	}

	summary := body["summary"].(map[string]any)
	if v, _ := summary["succeeded"].(float64); int(v) != 2 {
		t.Fatalf("summary.succeeded = %v, want 2", summary["succeeded"])
	}
	if v, _ := summary["failed"].(float64); int(v) != 1 {
		t.Fatalf("summary.failed = %v, want 1", summary["failed"])
	}
}

// TestDepAddMany_EmptyItems → top-level validation_error.
func TestDepAddMany_EmptyItems(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAddMany(pool, depsStore, issuesStore)
	ctx := withCtxSession(t, userID, "user", "sess-empty")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": []any{}}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError on empty items")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestDepAddMany_DuplicateInBatchRowFailsOnAlreadyExists exercises the
// per-row visibility-of-prior-commits rule for dependency_already_exists
// (mirrors the cycle-detection test): two identical edges in one batch
// — row[0] persists, row[1] sees row[0]'s commit and fails with
// dependency_already_exists. Documents the partial-success contract for
// agents who pass the same edge twice (e.g. retrying a partial earlier
// batch).
func TestDepAddMany_DuplicateInBatchRowFailsOnAlreadyExists(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "damd")

	idA := prefix + "-1"
	idB := prefix + "-2"
	seedIssue(t, pool, pid, userID, idA, "A", "open", 2)
	seedIssue(t, pool, pid, userID, idB, "B", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAddMany(pool, depsStore, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-dup")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"fromId": idA,
				"toId":   idB,
				"type":   dependencies.DepBlocks,
			},
			map[string]any{
				"fromId": idA,
				"toId":   idB,
				"type":   dependencies.DepBlocks,
			},
		},
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}

	row0 := results[0].(map[string]any)
	if okFlag, _ := row0["ok"].(bool); !okFlag {
		t.Fatalf("row0 (first A→B) should succeed: %+v", row0)
	}

	row1 := results[1].(map[string]any)
	if okFlag, _ := row1["ok"].(bool); okFlag {
		t.Fatalf("row1 (duplicate A→B) should fail: %+v", row1)
	}
	errObj := row1["error"].(map[string]any)
	if got := errObj["code"]; got != ErrCodeDependencyAlreadyExists {
		t.Fatalf("row1 error.code = %v, want %s", got, ErrCodeDependencyAlreadyExists)
	}

	// Row 0's persisted edge must still be there — no rollback from the
	// row 1 failure.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'blocks'`,
		idA, idB).Scan(&n); err != nil {
		t.Fatalf("query A→B: %v", err)
	}
	if n != 1 {
		t.Fatalf("A→B count = %d, want 1 (one survives, one rejected)", n)
	}

	summary := body["summary"].(map[string]any)
	if v, _ := summary["succeeded"].(float64); int(v) != 1 {
		t.Fatalf("summary.succeeded = %v, want 1", summary["succeeded"])
	}
	if v, _ := summary["failed"].(float64); int(v) != 1 {
		t.Fatalf("summary.failed = %v, want 1", summary["failed"])
	}
}

// TestDepAddMany_ForceTopLevelSilentlyIgnored verifies the schema does
// NOT declare a top-level `force` parameter (skein_dep_add has no
// admin-bypass path either). Passing `force: true` at the top level
// must not cause a top-level error: the handler reads only `items`,
// extra args are silently ignored. This documents the absence of the
// flag — agents that assume parity with skein_close_many's force
// semantics get a partial-success success envelope, NOT a forbidden
// error.
func TestDepAddMany_ForceTopLevelSilentlyIgnored(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "damf")

	idA := prefix + "-1"
	idB := prefix + "-2"
	seedIssue(t, pool, pid, userID, idA, "A", "open", 2)
	seedIssue(t, pool, pid, userID, idB, "B", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAddMany(pool, depsStore, issuesStore)
	ctx := withCtxSession(t, userID, "user", "sess-force-ign")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"fromId": idA,
				"toId":   idB,
				"type":   dependencies.DepRelated,
			},
		},
		"force": true, // not declared in schema; must be silently ignored
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected partial-success success envelope; got isError=true: %+v", body)
	}
	body := extractToolJSON(t, res)
	results := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	row0 := results[0].(map[string]any)
	if okFlag, _ := row0["ok"].(bool); !okFlag {
		t.Fatalf("row0 (A→B related) should succeed; force should not have changed semantics: %+v", row0)
	}
}

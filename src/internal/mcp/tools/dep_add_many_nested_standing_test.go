package tools

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestDepAddMany_NestedStanding_RowLevelCode is the sk-118 (F2e) row-contract
// regression: skein_dep_add_many must promote the nested-standing ban to the
// ROW-level code `nested_standing_forbidden` — matching the single-row
// skein_dep_add / skein_reparent surfaces — instead of folding it under the
// generic `validation_error` with the real code buried in details.
//
// Scenario: a standing parent and a standing child. A batch with two rows —
//  1. a parent-child edge (from = standing child, to = standing parent) that
//     would nest a standing task under a standing task → REJECTED with the
//     promoted row-level code; and
//  2. an unrelated, valid `related` edge between two open issues → succeeds.
//
// Proves the special-case fires per-row and leaves other rows in the batch
// unaffected.
func TestDepAddMany_NestedStanding_RowLevelCode(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "damns")

	standingParent := prefix + "-1"
	standingChild := prefix + "-2"
	openX := prefix + "-3"
	openY := prefix + "-4"
	seedIssue(t, pool, pid, userID, standingParent, "standing parent", "standing", 2)
	seedIssue(t, pool, pid, userID, standingChild, "standing child", "standing", 2)
	seedIssue(t, pool, pid, userID, openX, "X", "open", 2)
	seedIssue(t, pool, pid, userID, openY, "Y", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleDepAddMany(pool, depsStore, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-nested-standing")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"fromId": standingChild,
				"toId":   standingParent,
				"type":   dependencies.DepParentChild,
			},
			map[string]any{
				"fromId": openX,
				"toId":   openY,
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
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}

	// Row 0: nested-standing parent-child → row-level nested_standing_forbidden.
	row0 := results[0].(map[string]any)
	if okFlag, _ := row0["ok"].(bool); okFlag {
		t.Fatalf("row0 (standing child under standing parent) should fail: %+v", row0)
	}
	errObj := row0["error"].(map[string]any)
	if got := errObj["code"]; got != ErrCodeNestedStandingForbidden {
		t.Fatalf("row0 error.code = %v, want %s (must be promoted to row level, not buried under validation_error)",
			got, ErrCodeNestedStandingForbidden)
	}
	// Details preserve fromId / toId / type so the agent can pinpoint the row.
	details, _ := errObj["details"].(map[string]any)
	if details == nil {
		t.Fatalf("row0 error.details missing; want fromId/toId/type preserved: %+v", errObj)
	}
	if got := details["fromId"]; got != standingChild {
		t.Fatalf("details.fromId = %v, want %s", got, standingChild)
	}
	if got := details["toId"]; got != standingParent {
		t.Fatalf("details.toId = %v, want %s", got, standingParent)
	}
	if got := details["type"]; got != dependencies.DepParentChild {
		t.Fatalf("details.type = %v, want %s", got, dependencies.DepParentChild)
	}

	// Row 1: the unrelated related edge must still succeed — other rows unaffected.
	row1 := results[1].(map[string]any)
	if okFlag, _ := row1["ok"].(bool); !okFlag {
		t.Fatalf("row1 (X related Y) should succeed: %+v", row1)
	}

	summary := body["summary"].(map[string]any)
	if v, _ := summary["succeeded"].(float64); int(v) != 1 {
		t.Fatalf("summary.succeeded = %v, want 1", summary["succeeded"])
	}
	if v, _ := summary["failed"].(float64); int(v) != 1 {
		t.Fatalf("summary.failed = %v, want 1", summary["failed"])
	}
}

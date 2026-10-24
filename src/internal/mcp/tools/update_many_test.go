package tools

import (
	"context"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestUpdateMany_PartialSuccessMixedRows: 5 rows, item #3 has stale
// expectedVersion → that row fails with version_conflict, the rest
// succeed with title patches applied.
func TestUpdateMany_PartialSuccessMixedRows(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "umm")

	const total = 5
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		seedIssue(t, pool, pid, userID, ids[i], "old title "+ids[i], "open", 2)
	}

	stale := ids[2]
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET version = 2, updated_at = now() WHERE id = $1`, stale); err != nil {
		t.Fatalf("bump version: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleUpdateMany(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-um")

	items := make([]any, total)
	for i := 0; i < total; i++ {
		items[i] = map[string]any{
			"id":              ids[i],
			"expectedVersion": float64(1),
			"title":           "patched " + ids[i],
		}
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	// Matrix row 89 (sk-282 T020): skein_update_many is the reference
	// form US-5 forbids "fixing" — its wire shape must be unchanged by
	// the acknowledgement work on the single-issue tools.
	assertBatchWireFormUnchanged(t, body, total)
	results := body["results"].([]any)
	if len(results) != total {
		t.Fatalf("results len = %d, want %d", len(results), total)
	}

	for i, raw := range results {
		row := raw.(map[string]any)
		if row["id"] != ids[i] {
			t.Fatalf("results[%d].id = %v, want %s", i, row["id"], ids[i])
		}
		if i == 2 {
			if okFlag, _ := row["ok"].(bool); okFlag {
				t.Fatalf("results[%d].ok = true; want false (stale)", i)
			}
			errObj, _ := row["error"].(map[string]any)
			if got := errObj["code"]; got != ErrCodeVersionConflict {
				t.Fatalf("results[%d].error.code = %v, want %s",
					i, got, ErrCodeVersionConflict)
			}
		} else {
			if okFlag, _ := row["ok"].(bool); !okFlag {
				t.Fatalf("results[%d] failed: %+v", i, row)
			}
		}
	}

	// Verify titles persisted on the 4 success rows.
	for i, id := range ids {
		if i == 2 {
			continue
		}
		var title string
		if err := pool.QueryRow(context.Background(),
			`SELECT title FROM issues WHERE id = $1`, id,
		).Scan(&title); err != nil {
			t.Fatalf("read title %s: %v", id, err)
		}
		if title != "patched "+id {
			t.Fatalf("issue %s title = %q, want patched", id, title)
		}
	}

	summary := body["summary"].(map[string]any)
	if v, _ := summary["succeeded"].(float64); int(v) != total-1 {
		t.Fatalf("summary.succeeded = %v, want %d", summary["succeeded"], total-1)
	}
	if v, _ := summary["failed"].(float64); int(v) != 1 {
		t.Fatalf("summary.failed = %v, want 1", summary["failed"])
	}
}

// TestUpdateMany_PerRowFieldValidationError: one row has an invalid
// title (empty after trim) → that row reports validation_error,
// others succeed.
func TestUpdateMany_PerRowFieldValidationError(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ump")

	id1 := prefix + "-1"
	id2 := prefix + "-2"
	seedIssue(t, pool, pid, userID, id1, "ok title", "open", 2)
	seedIssue(t, pool, pid, userID, id2, "ok title 2", "open", 2)

	store := issues.NewStore(pool)
	handler := handleUpdateMany(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-up")

	items := []any{
		map[string]any{
			"id":              id1,
			"expectedVersion": float64(1),
			"title":           "", // invalid: empty after trim
		},
		map[string]any{
			"id":              id2,
			"expectedVersion": float64(1),
			"title":           "valid",
		},
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("handler: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	results := body["results"].([]any)

	row0 := results[0].(map[string]any)
	if okFlag, _ := row0["ok"].(bool); okFlag {
		t.Fatalf("row0 should fail validation: %+v", row0)
	}
	errObj0 := row0["error"].(map[string]any)
	if got := errObj0["code"]; got != ErrCodeValidationError {
		t.Fatalf("row0 error.code = %v, want %s", got, ErrCodeValidationError)
	}

	row1 := results[1].(map[string]any)
	if okFlag, _ := row1["ok"].(bool); !okFlag {
		t.Fatalf("row1 should succeed: %+v", row1)
	}
}

// TestUpdateMany_EmptyItems → top-level validation_error.
func TestUpdateMany_EmptyItems(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := issues.NewStore(pool)
	handler := handleUpdateMany(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-empty")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": []any{}}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on empty items")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestUpdateMany_OwnerForceAllowed — row 190 (sk-674.18, US-16): the
// project owner, who is no instance admin, now holds force_unlock — his
// forced row edits past another holder's claim (the claim stays: a force
// edit bypasses it, decision (b)).
func TestUpdateMany_OwnerForceAllowed(t *testing.T) {
	f := newForceFixture(t, "umown")
	handler := handleUpdateMany(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.owner, "user", "sess-owner")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{map[string]any{"id": f.issueID, "expectedVersion": float64(1), "priority": float64(0)}},
		"force": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	rows, _ := body["results"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["ok"] != true {
		t.Fatalf("the project owner's forced row was refused: %v", body)
	}
	var priority int
	if err := f.pool.QueryRow(context.Background(), `SELECT priority FROM issues WHERE id = $1`, f.issueID).Scan(&priority); err != nil {
		t.Fatal(err)
	}
	if priority != 0 {
		t.Errorf("priority = %d after the owner's forced edit, want 0", priority)
	}
}

// TestUpdateMany_MemberForceForbidden — force in a batch is checked per row
// on the row's project (US-16, K-28): a member's row fails with
// force_unlock_forbidden and nothing is written.
func TestUpdateMany_MemberForceForbidden(t *testing.T) {
	f := newForceFixture(t, "umnaf")
	handler := handleUpdateMany(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.member, "user", "sess-force")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"id":              f.issueID,
				"expectedVersion": float64(1),
				"status":          "deferred",
			},
		},
		"force": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	rows, _ := body["results"].([]any)
	if len(rows) != 1 {
		t.Fatalf("results = %v, want one row", body["results"])
	}
	row := rows[0].(map[string]any)
	if row["ok"] == true {
		t.Fatalf("a member's forced row succeeded: %v", row)
	}
	errObj, _ := row["error"].(map[string]any)
	f.assertForceRefused(t, errObj["code"])
}

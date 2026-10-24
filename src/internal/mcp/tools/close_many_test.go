package tools

import (
	"context"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestCloseMany_PartialSuccessMixedRows exercises the central
// partial-success contract: one bad row (wrong expectedVersion) does
// not poison the rest of the batch. 10 items, item #5 stale → its
// row.ok=false code=version_conflict, the other 9 succeed.
func TestCloseMany_PartialSuccessMixedRows(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clmm")

	const total = 10
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		seedIssue(t, pool, pid, userID, ids[i], "batch close item", "open", 2)
	}

	// Bump version on item #5 directly so the caller's expectedVersion=1
	// is stale.
	stale := ids[4] // 0-indexed; item #5 in human terms
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET version = 2, updated_at = now() WHERE id = $1`, stale); err != nil {
		t.Fatalf("bump version: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleCloseMany(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-batch")

	items := make([]any, total)
	for i := 0; i < total; i++ {
		items[i] = map[string]any{
			"id":              ids[i],
			"expectedVersion": float64(1),
			"closeOutcome":    "done",
			"reason":          "batch close",
		}
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError=true: %+v", body)
	}
	body := extractToolJSON(t, res)
	// Matrix row 89 (sk-282 T020), retargeted by sk-489 row 29: this tool
	// now has its OWN row and envelope — the reference pair stays frozen
	// and is still guarded on skein_update_many — so the wire check is
	// against close_many's declaration. Every property of the old guard
	// survives; see assertCloseManyWireForm for why the extension is a
	// decision rather than a regression.
	assertCloseManyWireForm(t, body, total)
	results, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("results missing or not array: %+v", body)
	}
	if len(results) != total {
		t.Fatalf("results len = %d, want %d", len(results), total)
	}

	for i, raw := range results {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("results[%d] not object: %T", i, raw)
		}
		if row["id"] != ids[i] {
			t.Fatalf("results[%d].id = %v, want %s", i, row["id"], ids[i])
		}
		if i == 4 { // the stale one
			okFlag, _ := row["ok"].(bool)
			if okFlag {
				t.Fatalf("results[%d].ok = true; want false (stale row)", i)
			}
			errObj, _ := row["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("results[%d].error missing: %+v", i, row)
			}
			if got := errObj["code"]; got != ErrCodeVersionConflict {
				t.Fatalf("results[%d].error.code = %v, want %s",
					i, got, ErrCodeVersionConflict)
			}
		} else {
			okFlag, _ := row["ok"].(bool)
			if !okFlag {
				t.Fatalf("results[%d].ok = false on item %s; want true: %+v",
					i, ids[i], row)
			}
			// Successful close bumps version 1→2.
			if v, _ := row["version"].(float64); int(v) != 2 {
				t.Fatalf("results[%d].version = %v, want 2", i, row["version"])
			}
		}
	}

	summary, _ := body["summary"].(map[string]any)
	if summary == nil {
		t.Fatalf("summary missing: %+v", body)
	}
	if v, _ := summary["requested"].(float64); int(v) != total {
		t.Fatalf("summary.requested = %v, want %d", summary["requested"], total)
	}
	if v, _ := summary["succeeded"].(float64); int(v) != total-1 {
		t.Fatalf("summary.succeeded = %v, want %d", summary["succeeded"], total-1)
	}
	if v, _ := summary["failed"].(float64); int(v) != 1 {
		t.Fatalf("summary.failed = %v, want 1", summary["failed"])
	}

	// Verify status of the 9 succeeded rows is closed in the DB.
	for i, id := range ids {
		if i == 4 {
			continue
		}
		var status string
		if err := pool.QueryRow(context.Background(),
			`SELECT status FROM issues WHERE id = $1`, id,
		).Scan(&status); err != nil {
			t.Fatalf("read status %s: %v", id, err)
		}
		if status != "closed" {
			t.Fatalf("issue %s status = %s, want closed", id, status)
		}
	}
}

// TestCloseMany_EmptyItems → top-level validation_error (no results[]).
func TestCloseMany_EmptyItems(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := issues.NewStore(pool)
	handler := handleCloseMany(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-empty")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": []any{}}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on empty items")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestCloseMany_ExceedsMaxItems → 101 items rejected with validation_error.
func TestCloseMany_ExceedsMaxItems(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := issues.NewStore(pool)
	handler := handleCloseMany(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-max")

	items := make([]any, 101)
	for i := range items {
		items[i] = map[string]any{
			"id":              "x-1",
			"expectedVersion": float64(1),
			"closeOutcome":    "done",
			"reason":          "x",
		}
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on >100 items")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestCloseMany_OwnerForceAllowed — row 189 (sk-674.18, US-16): the
// project owner, who is no instance admin, now holds force_unlock — his
// forced row closes past another holder's claim.
func TestCloseMany_OwnerForceAllowed(t *testing.T) {
	f := newForceFixture(t, "cmown")
	handler := handleCloseMany(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.owner, "user", "sess-owner")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{map[string]any{
			"id": f.issueID, "expectedVersion": float64(1), "closeOutcome": "done", "reason": "x",
		}},
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
	f.assertReleased(t)
}

// TestCloseMany_MemberForceForbidden — force in a batch is checked per row
// on the row's project (US-16, K-28): a member's row fails with
// force_unlock_forbidden and nothing is written.
func TestCloseMany_MemberForceForbidden(t *testing.T) {
	f := newForceFixture(t, "cmnaf")
	handler := handleCloseMany(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.member, "user", "sess-force")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"id":              f.issueID,
				"expectedVersion": float64(1),
				"closeOutcome":    "done",
				"reason":          "x",
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

// TestCloseMany_PerRowEmptyReason → that row gets validation_error with
// the rest of the batch (in this case just one) returning intact shape.
func TestCloseMany_PerRowEmptyReason(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "clmer")
	id := prefix + "-1"
	seedIssue(t, pool, pid, userID, id, "x", "open", 2)

	store := issues.NewStore(pool)
	handler := handleCloseMany(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-er")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"items": []any{
			map[string]any{
				"id":              id,
				"expectedVersion": float64(1),
				"closeOutcome":    "done",
				"reason":          "   ",
			},
		},
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("expected partial-success success envelope: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	results := body["results"].([]any)
	row := results[0].(map[string]any)
	if okFlag, _ := row["ok"].(bool); okFlag {
		t.Fatalf("expected ok=false on empty reason, got %+v", row)
	}
	errObj := row["error"].(map[string]any)
	if got := errObj["code"]; got != ErrCodeValidationError {
		t.Fatalf("error.code = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestCloseMany_AllRowsFail is sk-282 test-plan row 90: "a group
// close or update where every row fails — the call succeeds, the
// summary reports zero succeeded; this is not a call failure."
//
// It is the boundary case of the partial-success contract, and it matters
// because the obvious implementation gets it wrong in the obvious way: a
// batch where NOTHING succeeded looks like a failed call, and returning a
// tool error there would (a) hide the per-row reasons the agent needs to
// recover and (b) make the response shape depend on how many rows
// happened to fail.
//
// The spec names the surface deliberately: partial success is the
// contract of the group CLOSE and UPDATE only. The group claim and
// epic_create are all-or-nothing and a zero-succeeded summary must never
// appear there — that is asserted where those tools live, not here.
func TestCloseMany_AllRowsFail(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clmz")

	const total = 4
	ids := make([]string, total)
	for i := range total {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		seedIssue(t, pool, pid, userID, ids[i], "batch close all-fail", "open", 2)
		// Bump EVERY row so the caller's expectedVersion=1 is stale on
		// all of them.
		if _, err := pool.Exec(context.Background(),
			`UPDATE issues SET version = 2, updated_at = now() WHERE id = $1`, ids[i]); err != nil {
			t.Fatalf("bump version %s: %v", ids[i], err)
		}
	}

	store := issues.NewStore(pool)
	handler := handleCloseMany(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-batch-allfail")

	items := make([]any, total)
	for i := range total {
		items[i] = map[string]any{
			"id":              ids[i],
			"expectedVersion": float64(1),
			"closeOutcome":    "done",
			"reason":          "batch close",
		}
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("the call was reported as an error although every row failed for a "+
			"row-level reason — a batch where nothing succeeded is still a successful "+
			"call: %+v", extractToolJSON(t, res))
	}

	body := extractToolJSON(t, res)
	results, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("results missing or not an array: %+v", body)
	}
	if len(results) != total {
		t.Fatalf("results len = %d, want %d — every INPUT row gets a row, failures included",
			len(results), total)
	}
	for i, raw := range results {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("results[%d] is not an object: %T", i, raw)
		}
		if row["id"] != ids[i] {
			t.Errorf("results[%d].id = %v, want %s", i, row["id"], ids[i])
		}
		if okFlag, _ := row["ok"].(bool); okFlag {
			t.Errorf("results[%d].ok = true, want false", i)
		}
		errObj, _ := row["error"].(map[string]any)
		if errObj == nil {
			t.Fatalf("results[%d].error missing — a failed row must carry the structured "+
				"reason, that is the whole point of not failing the call: %+v", i, row)
		}
		if got := errObj["code"]; got != ErrCodeVersionConflict {
			t.Errorf("results[%d].error.code = %v, want %s", i, got, ErrCodeVersionConflict)
		}
	}

	summary, _ := body["summary"].(map[string]any)
	if summary == nil {
		t.Fatalf("summary missing: %+v", body)
	}
	if v, _ := summary["requested"].(float64); int(v) != total {
		t.Errorf("summary.requested = %v, want %d", summary["requested"], total)
	}
	if v, _ := summary["succeeded"].(float64); int(v) != 0 {
		t.Errorf("summary.succeeded = %v, want 0 — the summary is what reports the outcome "+
			"when the call itself did not fail", summary["succeeded"])
	}
	if v, _ := summary["failed"].(float64); int(v) != total {
		t.Errorf("summary.failed = %v, want %d", summary["failed"], total)
	}

	// INV-3-adjacent: nothing was written. A batch that reported zero
	// successes must not have closed anything.
	for _, id := range ids {
		var status string
		if err := pool.QueryRow(context.Background(),
			`SELECT status FROM issues WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read status %s: %v", id, err)
		}
		if status != "open" {
			t.Errorf("issue %s status = %s, want open — no row succeeded, so none may have "+
				"been closed", id, status)
		}
	}
}

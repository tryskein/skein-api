package tools

import (
	"context"
	"strconv"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func TestReady_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rdy")
	for i := 1; i <= 2; i++ {
		seedIssue(t, pool, pid, userID, prefix+"-"+strconv.Itoa(i), "ready issue", "open", 0)
	}

	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleReady(pool, depsStore, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pid.String()}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("expected items array, got %T", body["items"])
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 ready items, got %d", len(items))
	}
}

func TestReady_ProjectNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleReady(pool, depsStore, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": "no-such-project"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error code = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

func TestReady_PriorityFilter(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rdyp")
	seedIssue(t, pool, pid, userID, prefix+"-1", "p0", "open", 0)
	seedIssue(t, pool, pid, userID, prefix+"-2", "p3", "open", 3)

	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleReady(pool, depsStore, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":     pid.String(),
		"priorityMax": float64(1),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	body := extractToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("expected items array")
	}
	// Only the P0 issue should pass the priorityMax=1 filter.
	for _, raw := range items {
		m := raw.(map[string]any)
		p := int(m["priority"].(float64))
		if p > 1 {
			t.Fatalf("priorityMax=1 violated: got %d", p)
		}
	}
}

// ---------------------------------------------------------------------
// sk-282 T017 — SC-010 (test-plan row 66). Row 67 is the REST side and
// lives with the REST handler.
// ---------------------------------------------------------------------

// TestReady_NoLabelOrComponentKeysWhenTheIssueHasThem is row 66.
//
// The fixture is the whole point: the issue REALLY HAS labels and a
// component. A test on an issue without them would pass against an
// implementation that returns `[]` — which is the exact lie SC-010
// removes, because `[]` on a query that never loaded the aggregate says
// «this issue has none» with confidence and is wrong.
func TestReady_NoLabelOrComponentKeysWhenTheIssueHasThem(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rdykeys")
	issueID := seedIssue(t, pool, pid, userID, prefix+"-1", "ready with labels", "open", 0)

	componentID := testsupport.SeedComponent(t, pool, pid, userID, "rdycomp", "")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'backend'), ($1, 'p1')`,
		issueID); err != nil {
		t.Fatalf("seed labels: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
		issueID, componentID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	handler := handleReady(pool, dependencies.NewStore(pool), projects.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pid.String()}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	items := body["items"].([]any)
	if len(items) == 0 {
		t.Fatal("ready returned nothing — the fixture does not exercise the row at all")
	}

	for _, raw := range items {
		row := raw.(map[string]any)
		for _, key := range []string{"labels", "componentIds"} {
			v, present := row[key]
			if present {
				t.Errorf("ready row carries %q = %#v. SC-010 is about the KEY, not its value: "+
					"this query never loads the aggregate, so an empty array here asserts "+
					"«this issue has none» — and this fixture's issue demonstrably HAS two "+
					"labels and a component.", key, v)
			}
		}
		// The rest of the row is Unchanged (US-9): the removal is two
		// keys, not a re-projection.
		for _, key := range []string{"id", "title", "status", "priority", "type", "updatedAt"} {
			if _, ok := row[key]; !ok {
				t.Errorf("ready row lost %q — US-9 declares the rest of the row unchanged", key)
			}
		}
	}

	// And the data really is there — the narrowing is on the wire, not in
	// the store (INV-3). The route the tool description names is the
	// skein_show card for both, with skein_list a route for `labels`
	// alone unless `componentIds` is named in `fields` — see
	// TestList_ComponentIdsAreReachableOnlyWhenNamed.
	var labelCount, componentCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM issue_labels WHERE issue_id = $1),
		        (SELECT count(*) FROM issue_components WHERE issue_id = $1)`,
		issueID).Scan(&labelCount, &componentCount); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if labelCount != 2 || componentCount != 1 {
		t.Errorf("stored labels/components = %d/%d, want 2/1 — the cut must be on the wire, "+
			"never in the data", labelCount, componentCount)
	}
}

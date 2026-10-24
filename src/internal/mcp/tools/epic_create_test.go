package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// TestEpicCreate_HappyPath verifies the basic decomposition path —
// epic + 5 children → all 6 issues exist with hierarchical IDs.
func TestEpicCreate_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ech")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Refactor auth subsystem",
		"kind":    "directed",
		"childTemplates": []any{
			map[string]any{"title": "design new middleware"},
			map[string]any{"title": "migrate user store", "type": "task", "priority": 1},
			map[string]any{"title": "wire SSO", "type": "feature"},
			map[string]any{"title": "delete legacy adapter", "type": "chore"},
			map[string]any{"title": "write integration tests"},
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	epic, ok := body["epic"].(map[string]any)
	if !ok {
		t.Fatalf("epic missing or wrong type: %T", body["epic"])
	}
	epicID, _ := epic["id"].(string)
	if epicID == "" {
		t.Fatalf("epic.id missing")
	}
	// sk-282 T024: `epic` and each `children` row are acknowledgements —
	// the type is card data and is read back from the row.
	assertAcknowledgementShape(t, epic)
	var epicType string
	if err := pool.QueryRow(context.Background(),
		`SELECT type FROM issues WHERE id = $1`, epicID).Scan(&epicType); err != nil {
		t.Fatalf("read back epic type: %v", err)
	}
	if epicType != "epic" {
		t.Errorf("epic type = %v, want epic", epicType)
	}
	children, ok := body["children"].([]any)
	if !ok {
		t.Fatalf("children missing or wrong type: %T", body["children"])
	}
	if len(children) != 5 {
		t.Fatalf("expected 5 children, got %d", len(children))
	}

	// Each child id is of the form epicID.N, and the parent-child edge is
	// verified in the graph — `parentIdCurrent` is card data, not
	// post-state, so it is no longer on the wire (sk-35 named the field;
	// sk-282 T024 moved the oracle to the edge that derives it).
	for i, c := range children {
		obj := c.(map[string]any)
		assertAcknowledgementShape(t, obj)
		gotID, _ := obj["id"].(string)
		// Hierarchical IDs are `<epicID>.<n>` — verify the prefix.
		if len(gotID) <= len(epicID)+1 || gotID[:len(epicID)+1] != epicID+"." {
			t.Errorf("child[%d].id = %q, expected prefix %q.", i, gotID, epicID)
		}
		var edges int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'parent-child'`,
			gotID, epicID).Scan(&edges); err != nil {
			t.Fatalf("count parent edge of %s: %v", gotID, err)
		}
		if edges != 1 {
			t.Errorf("child[%d] (%s) has %d parent-child edges to %s, want 1",
				i, gotID, edges, epicID)
		}
	}

	// Verify DB state: 6 issues created (epic + 5 children).
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&count); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if count != 6 {
		t.Errorf("expected 6 issues in project, got %d", count)
	}

	// And the epic's Epic rollup reads back populated (slice 1 + 2
	// integration smoke) — for the caller: a read without a reader counts
	// no child at all (INV-28 (4), sk-674.18 T017).
	dto, err := issuesStore.GetIssue(ctx, epicID)
	if err != nil {
		t.Fatalf("GetIssue epic: %v", err)
	}
	if dto.Epic == nil || dto.Epic.TotalChildren != 5 {
		t.Errorf("epic rollup: %+v; want TotalChildren=5", dto.Epic)
	}
}

// TestEpicCreate_EmptyChildTemplates verifies the affordance accepts
// (but is not very useful for) a children-less call — produces just
// the epic. Callers who want a plain empty epic should use skein_create
// with type=epic; this tool tolerates the path so the agent doesn't
// have to learn two contracts.
func TestEpicCreate_EmptyChildTemplates(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ece")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Solo epic",
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
	children, _ := body["children"].([]any)
	if len(children) != 0 {
		t.Errorf("expected zero children, got %d", len(children))
	}
}

// TestEpicCreate_Atomicity verifies a child failure rolls back the
// entire batch — epic AND every preceding child vanish. We trigger the
// failure by inserting a child with an invalid title (empty after
// trim).
func TestEpicCreate_Atomicity(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "eca")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Atomicity epic",
		"kind":    "directed",
		"childTemplates": []any{
			map[string]any{"title": "good first child"},
			map[string]any{"title": "good second child"},
			map[string]any{"title": "   "}, // bad — fails ValidateTitle
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on atomicity failure; got: %+v", res)
	}

	// Matrix row 87 / FP-10 (sk-282 T024). The acknowledgement gave this
	// tool a per-row shape, which makes «two created, one failed» a thing
	// the response could now express — and expressing it would turn an
	// all-or-nothing decomposition into a half-built epic, the exact
	// failure mode this tool exists to prevent. So the assertion is that
	// the failure response is not the success envelope at all.
	failure := extractToolJSON(t, res)
	for _, key := range []string{"epic", "children"} {
		if v, present := failure[key]; present {
			t.Errorf("the failure response carries %q = %v — a rolled-back batch must not "+
				"report rows", key, v)
		}
	}

	// DB should have no issues in this project.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&count); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 issues after rollback, got %d", count)
	}
}

// TestEpicCreate_RejectsEpicChildType verifies the tool rejects a
// child with type=epic. Nested epics aren't a supported decomposition
// affordance.
func TestEpicCreate_RejectsEpicChildType(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ecn")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Outer epic",
		"kind":    "directed",
		"childTemplates": []any{
			map[string]any{"title": "nested epic", "type": "epic"},
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on nested-epic child; got: %+v", res)
	}
}

// TestEpicCreate_KindRequiredForAgent verifies an agent caller without
// `kind` gets a validation_error.
func TestEpicCreate_KindRequiredForAgent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "eck")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "No kind",
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true without kind; got: %+v", res)
	}
}

// readComponentIDsFromDB queries the issue_components junction
// directly and returns a sorted slice of stringified uuids. Bypasses
// the GetIssue path so the test asserts the on-disk truth, not a
// derived response field.
func readComponentIDsFromDB(t *testing.T, pool *pgxpool.Pool, issueID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT component_id FROM issue_components WHERE issue_id = $1`, issueID)
	if err != nil {
		t.Fatalf("query issue_components: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan component_id: %v", err)
		}
		out = append(out, id.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestEpicCreate_EpicComponentIds_ChildrenInherit asserts that
// supplying componentIds at the epic level — without per-child
// overrides — propagates the same component set to every child via
// the store's existing parent-component inheritance path.
func TestEpicCreate_EpicComponentIds_ChildrenInherit(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ecih")
	compA := seedComponent(t, pool, pid, userID, "alpha", "first")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "Epic with component",
		"kind":         "directed",
		"componentIds": []any{compA.String()},
		"childTemplates": []any{
			map[string]any{"title": "first child"},
			map[string]any{"title": "second child"},
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	epic, _ := body["epic"].(map[string]any)
	epicID, _ := epic["id"].(string)
	if epicID == "" {
		t.Fatalf("epic.id missing")
	}

	// Verify epic carries [A] in the junction.
	got := readComponentIDsFromDB(t, pool, epicID)
	if len(got) != 1 || got[0] != compA.String() {
		t.Errorf("epic components = %v, want [%s]", got, compA.String())
	}

	// Verify both children carry [A] (inherited).
	children, _ := body["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	for i, c := range children {
		cm, _ := c.(map[string]any)
		cid, _ := cm["id"].(string)
		gotChild := readComponentIDsFromDB(t, pool, cid)
		if len(gotChild) != 1 || gotChild[0] != compA.String() {
			t.Errorf("child[%d] (%s) components = %v, want [%s]", i, cid, gotChild, compA.String())
		}
	}
}

// TestEpicCreate_PerChildComponentOverride asserts that
// childTemplates[i].componentIds wins over the epic's set —
// non-overridden siblings still inherit, overridden siblings carry
// only the per-child UUIDs.
func TestEpicCreate_PerChildComponentOverride(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "epco")
	compA := seedComponent(t, pool, pid, userID, "alpha", "")
	compB := seedComponent(t, pool, pid, userID, "beta", "")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "Epic with override children",
		"kind":         "directed",
		"componentIds": []any{compA.String()},
		"childTemplates": []any{
			// child[0] overrides with B
			map[string]any{"title": "override child", "componentIds": []any{compB.String()}},
			// child[1] omits — inherits A
			map[string]any{"title": "inherit child"},
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	epic, _ := body["epic"].(map[string]any)
	epicID, _ := epic["id"].(string)

	if got := readComponentIDsFromDB(t, pool, epicID); len(got) != 1 || got[0] != compA.String() {
		t.Errorf("epic components = %v, want [%s]", got, compA.String())
	}

	children, _ := body["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	overrideID, _ := children[0].(map[string]any)["id"].(string)
	inheritID, _ := children[1].(map[string]any)["id"].(string)

	if got := readComponentIDsFromDB(t, pool, overrideID); len(got) != 1 || got[0] != compB.String() {
		t.Errorf("override child components = %v, want [%s]", got, compB.String())
	}
	if got := readComponentIDsFromDB(t, pool, inheritID); len(got) != 1 || got[0] != compA.String() {
		t.Errorf("inherit child components = %v, want [%s]", got, compA.String())
	}
}

// TestEpicCreate_InvalidComponentUUID rejects a malformed UUID at
// both the top-level and per-child positions with validation_error.
func TestEpicCreate_InvalidComponentUUID(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "eivu")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	t.Run("top-level", func(t *testing.T) {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project":      pid.String(),
			"title":        "Bad top-level uuid",
			"kind":         "directed",
			"componentIds": []any{"not-a-uuid"},
		}
		res, err := handler(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected isError=true; got: %+v", res)
		}
		body := extractToolJSON(t, res)
		if got, _ := body["error"].(string); got != ErrCodeValidationError {
			t.Errorf("error = %v, want %s", got, ErrCodeValidationError)
		}
	})

	t.Run("per-child", func(t *testing.T) {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project": pid.String(),
			"title":   "Bad per-child uuid",
			"kind":    "directed",
			"childTemplates": []any{
				map[string]any{"title": "ok"},
				map[string]any{"title": "bad", "componentIds": []any{"still-not-a-uuid"}},
			},
		}
		res, err := handler(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected isError=true; got: %+v", res)
		}
		body := extractToolJSON(t, res)
		if got, _ := body["error"].(string); got != ErrCodeValidationError {
			t.Errorf("error = %v, want %s", got, ErrCodeValidationError)
		}
		// childIndex should be present in the details so the agent
		// knows which template to fix.
		details, _ := body["details"].(map[string]any)
		if details == nil {
			// Some envelope shapes inline details into the top-level
			// map. Fall back to the top-level "childIndex" probe.
			details = body
		}
		if got, _ := details["childIndex"].(float64); got != 1 {
			t.Errorf("details.childIndex = %v, want 1", details["childIndex"])
		}
	})
}

// TestEpicCreate_ComponentFromOtherProject rejects a component UUID
// that belongs to a different project, even though it parses cleanly
// as a UUID. The store's assertComponentsInProject is the gate; the
// tool layer must surface it as `components_wrong_project` and leave
// no rows behind in the target project.
func TestEpicCreate_ComponentFromOtherProject(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	p1, _ := seedProject(t, pool, userID, "ecfopA")
	p2, _ := seedProject(t, pool, userID, "ecfopB")
	// Component lives in p2; epic is in p1.
	compB := seedComponent(t, pool, p2, userID, "beta", "")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      p1.String(),
		"title":        "Cross-project component epic",
		"kind":         "directed",
		"componentIds": []any{compB.String()},
		"childTemplates": []any{
			map[string]any{"title": "rolled-back child"},
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true; got: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got, _ := body["error"].(string); got != ErrCodeComponentsWrongProject {
		t.Errorf("error = %v, want %s", got, ErrCodeComponentsWrongProject)
	}

	// No rows in p1 (full rollback).
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, p1).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 issues in p1 after rollback, got %d", n)
	}
}

// TestEpicCreate_AtomicRollbackOnComponentError verifies the full
// batch (epic + every child) rolls back when one child references a
// component UUID outside the project. Belt-and-braces over
// TestEpicCreate_ComponentFromOtherProject: that test fails at the
// epic insert; this one fails partway through the children to
// exercise the SERIALIZABLE-tx rollback through ErrEpicChildFailed.
func TestEpicCreate_AtomicRollbackOnComponentError(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	p1, _ := seedProject(t, pool, userID, "earcA")
	p2, _ := seedProject(t, pool, userID, "earcB")
	// Good component in p1, bad component (out-of-project) in p2.
	compA := seedComponent(t, pool, p1, userID, "alpha", "")
	compB := seedComponent(t, pool, p2, userID, "beta", "")

	issuesStore := issues.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleEpicCreate(pool, issuesStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      p1.String(),
		"title":        "Atomic-rollback-via-child epic",
		"kind":         "directed",
		"componentIds": []any{compA.String()},
		"childTemplates": []any{
			map[string]any{"title": "c0 inherits A"},
			map[string]any{"title": "c1 inherits A"},
			map[string]any{"title": "c2 inherits A"},
			// c3 explicitly references a foreign-project component
			// → store returns ErrComponentsWrongProject inside the
			// per-child step → ErrEpicChildFailed{Index:3} → whole
			// batch rolled back.
			map[string]any{"title": "c3 bad", "componentIds": []any{compB.String()}},
			map[string]any{"title": "c4 inherits A"},
		},
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true; got: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got, _ := body["error"].(string); got != ErrCodeComponentsWrongProject {
		t.Errorf("error = %v, want %s", got, ErrCodeComponentsWrongProject)
	}

	// Full rollback — neither the epic nor any of the preceding
	// children survive.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, p1).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 issues in p1 after rollback, got %d", n)
	}
}

// TestEpicCreate_SC015_TwentyChildrenFitTheBudget is matrix row 86.
//
// "SC-015: a group epic create with a couple of dozen children is under
// 3 000 characters while keeping the identifier and version of each
// child."
//
// Hostile fixture: every child template carries a full set of prose —
// this tool used to be the heaviest echo in the API precisely because
// each of those bodies came back. A twenty-child call with bare titles
// would fit the budget even if the response still echoed the cards.
func TestEpicCreate_SC015_TwentyChildrenFitTheBudget(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "sc015")

	handler := handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool))
	ctx := withCtx(t, userID, "admin")

	const childProse = "SENTINEL child body: a realistic decomposition note that the caller " +
		"is already holding, several hundred characters long so that echoing twenty of " +
		"them could not possibly hide inside a 3 000-character budget."
	templates := make([]any, 0, 20)
	for i := range 20 {
		templates = append(templates, map[string]any{
			"title":              fmt.Sprintf("SENTINEL child %02d of the decomposition", i+1),
			"description":        childProse,
			"design":             childProse,
			"acceptanceCriteria": childProse,
			"notes":              childProse,
		})
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":        pid.String(),
		"title":          "SENTINEL epic title for the twenty-child decomposition",
		"kind":           "directed",
		"description":    childProse,
		"childTemplates": templates,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	raw := toolText(t, res)
	body := extractToolJSON(t, res)

	epic, ok := body["epic"].(map[string]any)
	if !ok {
		t.Fatalf("epic missing: %s", raw)
	}
	assertAcknowledgementShape(t, epic)
	children, ok := body["children"].([]any)
	if !ok || len(children) != 20 {
		t.Fatalf("children = %d rows, want 20", len(children))
	}
	// Every child keeps its identifier AND its version — that is the
	// half of SC-015 a size check alone would let through.
	for i, raw := range children {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("children[%d] is %T, want an object", i, raw)
		}
		assertAcknowledgementShape(t, row)
		if v, _ := row["version"].(float64); int(v) != 1 {
			t.Errorf("children[%d].version = %v, want 1", i, row["version"])
		}
	}
	if strings.Contains(raw, "SENTINEL") {
		t.Errorf("the response echoes text the caller just supplied:\n%s", raw)
	}
	// SC-015. Literal threshold — see the SC-006 test for why.
	if n := len(raw); n >= 3000 {
		t.Errorf("epic-create response is %d characters, SC-015 budget is < 3000:\n%s", n, raw)
	}
	t.Logf("SC-015: epic + 20 children is %d characters", len(raw))

	// INV-3: the prose is in the system and reachable by a reading tool.
	epicID, _ := epic["id"].(string)
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT description FROM issues WHERE id = $1`, epicID+".1").Scan(&stored); err != nil {
		t.Fatalf("read back child description: %v", err)
	}
	if stored != childProse {
		t.Errorf("stored child description = %q, want the value as sent", stored)
	}
}

// TestMapEpicCreateError_SerializationRetry pins the sk-303.17 coderev
// fix at the MCP boundary: CreateEpicWithChildren exhausting its
// epicCreateMaxAttempts retry budget now returns
// issues.ErrSerializationRetry from the store (neither the epic nor any
// child was ever committed), and mapEpicCreateError must turn that into
// `serialization_retry` — NOT fall through to the ErrCodeInternalError
// default (the pre-fix behaviour, since the sentinel didn't exist at this
// call site's branch list at all), and NOT be misread as a per-child
// failure (childIndex is meaningless for a whole-transaction abort).
//
// Red on the pre-fix mapEpicCreateError (no issues.ErrSerializationRetry
// branch): reports ErrCodeInternalError instead.
func TestMapEpicCreateError_SerializationRetry(t *testing.T) {
	userID := uuid.New()
	projectID := uuid.New()

	cases := []struct {
		name string
		err  error
	}{
		{"bare sentinel", issues.ErrSerializationRetry},
		{"wrapped sentinel", fmt.Errorf("creating epic: %w", issues.ErrSerializationRetry)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mapEpicCreateError(context.Background(), tc.err, userID, projectID)
			if res == nil {
				t.Fatalf("mapEpicCreateError returned nil")
			}
			if !res.IsError {
				t.Fatalf("mapEpicCreateError(%v) produced a non-error result", tc.err)
			}
			body := extractToolJSON(t, res)
			if got := body["error"]; got != ErrCodeSerializationRetry {
				t.Fatalf("error = %v, want %s", got, ErrCodeSerializationRetry)
			}
			if _, has := body["childIndex"]; has {
				t.Fatalf("serialization_retry response carries childIndex %v — "+
					"a whole-transaction abort has no meaningful per-child index", body["childIndex"])
			}
		})
	}
}

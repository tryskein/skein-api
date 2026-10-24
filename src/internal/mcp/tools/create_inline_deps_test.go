package tools

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// TestCreate_InlineDeps_HappyPath creates a target issue, then
// creates a second issue with a `blocks` edge to the target in the
// same skein_create call. Asserts the new issue exists, the edge is
// in the dependencies table, and a dep.created event was written.
func TestCreate_InlineDeps_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cide")
	targetID := prefix + "-1"
	seedIssue(t, pool, pid, userID, targetID, "target issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "blocked by target",
		"type":    "feature",
		"kind":    "directed",
		"dependencies": []any{
			map[string]any{"toId": targetID, "type": "blocks"},
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	if body["created"] != true {
		t.Fatalf("created = %v, want true; body=%+v", body["created"], body)
	}
	issueObj, _ := body["issue"].(map[string]any)
	newID, _ := issueObj["id"].(string)
	if newID == "" {
		t.Fatalf("missing issue.id in body=%+v", body)
	}

	// Edge present?
	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'blocks'`,
		newID, targetID).Scan(&cnt); err != nil {
		t.Fatalf("verify edge: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 blocks edge from %s to %s, got %d", newID, targetID, cnt)
	}

	// dep.created event written?
	var evtCnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM events
 WHERE type = 'dependency.created'
   AND entity_type = 'dep'
   AND entity_id LIKE $1`,
		newID+"::%").Scan(&evtCnt); err != nil {
		t.Fatalf("verify event: %v", err)
	}
	if evtCnt != 1 {
		t.Fatalf("expected 1 dependency.created event, got %d", evtCnt)
	}
}

// TestCreate_InlineDeps_MissingTarget_RollsBack passes a dependencies[]
// entry referring to a nonexistent toId. Whole skein_create must roll
// back — the new issue must NOT be persisted.
func TestCreate_InlineDeps_MissingTarget_RollsBack(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cide_miss")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "should not persist",
		"type":    "task",
		"kind":    "directed",
		"dependencies": []any{
			map[string]any{"toId": "ghost-9999", "type": "blocks"},
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError=true for missing target; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["error"] != "issue_not_found" {
		t.Fatalf("error = %v, want issue_not_found; body=%+v", body["error"], body)
	}
	// dependencyIndex is flat at the top level of the error body (the
	// ToolError envelope merges the details map into the root object).
	if _, ok := body["dependencyIndex"]; !ok {
		t.Fatalf("dependencyIndex missing in error body=%+v", body)
	}

	// Rollback: zero issues in this project.
	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 issues after rollback, got %d", cnt)
	}
}

// TestCreate_InlineDeps_CycleRejected creates A→B blocks, then tries
// to create a new issue C with both blocks→B and a cycle-inducing
// inline edge. We pre-seed A, B, then create C with [blocks A] is
// fine. To force a cycle, seed B blocks A first, then on create of
// (id auto-allocated) try to add blocks → B AND another blocks-edge
// such that the create's own from is already on B's blocker chain.
// Simpler: seed B blocks A, then create newIssue with blocks → B
// (no cycle yet), and a second skein_create attempt with newIssue
// already existing — actually the inline-dep tx wouldn't help here.
//
// Easier scenario: seed a parent-child cycle attempt via deps. Use
// `parent-child` type: seed P, then C as parent-child of P (C→P);
// then create N with parent-child P→? — actually that's not a cycle
// on a brand new issue. The cleanest cycle scenario: seed A, then
// create B with [{ toId: A, type: blocks }], success; then create
// C with [{ toId: B, type: blocks }, { toId: A, type: blocks }] —
// still no cycle. A real cycle requires the new issue to already
// be on the target's blocker chain — impossible for a freshly-
// allocated id.
//
// Instead we cover the "self-reference" cycle: skein_create where
// dependencies[].toId references the parent id (which equals the
// new id is impossible). We cover validation_error for an unknown
// dep type instead — exercising the validation path through the
// inline-deps parser without requiring a graph cycle.
func TestCreate_InlineDeps_InvalidTypeRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cide_inv")
	targetID := prefix + "-1"
	seedIssue(t, pool, pid, userID, targetID, "target", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "invalid dep type",
		"type":    "task",
		"kind":    "directed",
		"dependencies": []any{
			map[string]any{"toId": targetID, "type": "depends_on"}, // not in the enum
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError=true; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error; body=%+v", body["error"], body)
	}

	// Rollback: only the seeded target remains.
	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 issue (seeded target only), got %d", cnt)
	}
}

// TestCreate_InlineDeps_CapEnforced — sending 51 deps should fail
// with validation_error before any DB write.
func TestCreate_InlineDeps_CapEnforced(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cide_cap")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	deps := make([]any, 51)
	for i := range deps {
		deps[i] = map[string]any{"toId": "ghost-1", "type": "blocks"}
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "too many deps",
		"type":         "task",
		"kind":         "directed",
		"dependencies": deps,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError for 51 deps; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error; body=%+v", body["error"], body)
	}

	// Nothing persisted.
	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 issues, got %d", cnt)
	}
}

// TestCreate_InlineDeps_MultipleEdges — two valid edges (blocks +
// related) both land in one call.
func TestCreate_InlineDeps_MultipleEdges(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cide_multi")
	t1 := prefix + "-1"
	t2 := prefix + "-2"
	seedIssue(t, pool, pid, userID, t1, "target one", "open", 2)
	seedIssue(t, pool, pid, userID, t2, "target two", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "new issue with two edges",
		"type":    "feature",
		"kind":    "directed",
		"dependencies": []any{
			map[string]any{"toId": t1, "type": "blocks"},
			map[string]any{"toId": t2, "type": "related"},
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	issueObj := body["issue"].(map[string]any)
	newID, _ := issueObj["id"].(string)
	if newID == "" {
		t.Fatalf("missing issue.id")
	}

	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM dependencies
 WHERE from_id = $1 AND type IN ('blocks', 'related')`,
		newID).Scan(&cnt); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("expected 2 edges, got %d", cnt)
	}
}

// TestCreate_InlineDeps_BadShape_RollsBack — dependencies[i] that is
// not an object (e.g. a string) → validation_error before any DB
// write.
func TestCreate_InlineDeps_BadShape_RollsBack(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cide_shape")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "bad shape",
		"type":    "task",
		"kind":    "directed",
		"dependencies": []any{
			"not-an-object",
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error; body=%+v", body["error"], body)
	}

	// Nothing persisted.
	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 issues, got %d", cnt)
	}
}

// TestCreate_InlineDeps_BadToIDShape — `toId: "42"` (bare numeric)
// should be rejected by the hierarchical-id pre-check.
func TestCreate_InlineDeps_BadToIDShape(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cide_idshape")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "bad toId",
		"type":    "task",
		"kind":    "directed",
		"dependencies": []any{
			map[string]any{"toId": "42", "type": "blocks"},
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error; body=%+v", body["error"], body)
	}
}

// TestCreate_InlineDeps_EmptyArray_NoOp — passing
// `dependencies: []` is treated the same as omitting the field: the
// issue is still created and no edges are added.
func TestCreate_InlineDeps_EmptyArray_NoOp(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cide_empty")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "empty deps",
		"type":         "task",
		"kind":         "directed",
		"dependencies": []any{},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["created"] != true {
		// JSON unmarshal would unmarshal `true` as bool — but a stringly
		//-typed comparison fails fast if a future regression turns this
		// into a string. Use json.Compact pretty-print just for the err.
		b, _ := json.MarshalIndent(body, "", "  ")
		t.Fatalf("created = %v, want true; body=\n%s", body["created"], string(b))
	}
	issueObj := body["issue"].(map[string]any)
	newID, _ := issueObj["id"].(string)
	if newID == "" {
		t.Fatalf("missing issue.id")
	}

	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM dependencies WHERE from_id = $1`, newID).Scan(&cnt); err != nil {
		t.Fatalf("count deps: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 edges, got %d", cnt)
	}
}

// TestCreateInlineDeps_BlockerTargetStanding_TopLevelCode — sk-118 F2f.
// An inline `blocks` / `waits-for` edge pointing at a STANDING target is
// banned (a standing blocker never closes, so it would gate the new issue's
// readiness forever). The ban fires inside dependencies.CreateInTx, so the
// whole skein_create rolls back. This test pins the ERROR ENVELOPE: the
// top-level `error` must be the actionable `blocker_target_standing` code
// (matching skein_dep_add / skein_dep_add_many), NOT a generic
// `validation_error` — AND the new issue must NOT have been inserted.
func TestCreateInlineDeps_BlockerTargetStanding_TopLevelCode(t *testing.T) {
	for _, depType := range []string{"blocks", "waits-for"} {
		depType := depType
		t.Run(depType, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, prefix := seedProject(t, pool, userID, "cide_blkstand_"+depMutTok(depType))
			standingTarget := prefix + "-1"
			seedIssue(t, pool, pid, userID, standingTarget, "standing blocker", "standing", 2)

			issuesStore := issues.NewStore(pool)
			depsStore := dependencies.NewStore(pool)
			projectsStore := projects.NewStore(pool)

			handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
			ctx := withCtx(t, userID, "admin")

			dep := map[string]any{"toId": standingTarget, "type": depType}
			// waits-for requires a gate.
			if depType == "waits-for" {
				dep["gate"] = "all-children"
			}

			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"project":      pid.String(),
				"title":        "blocked by a standing target",
				"type":         "feature",
				"kind":         "directed",
				"dependencies": []any{dep},
			}
			res, err := handler(ctx, req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError {
				body := extractToolJSON(t, res)
				t.Fatalf("expected isError=true for standing blocker target; body=%+v", body)
			}
			body := extractToolJSON(t, res)
			if body["error"] != "blocker_target_standing" {
				t.Fatalf("error = %v, want blocker_target_standing; body=%+v", body["error"], body)
			}
			// Inline-dep details ride along in the flattened envelope.
			if _, ok := body["dependencyIndex"]; !ok {
				t.Fatalf("dependencyIndex missing in error body=%+v", body)
			}
			if got := body["toId"]; got != standingTarget {
				t.Fatalf("toId = %v, want %s; body=%+v", got, standingTarget, body)
			}
			if got := body["type"]; got != depType {
				t.Fatalf("type = %v, want %s; body=%+v", got, depType, body)
			}

			// Rollback: only the seeded standing target remains; the new
			// issue was NOT inserted.
			var cnt int
			if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
				t.Fatalf("count issues: %v", err)
			}
			if cnt != 1 {
				t.Fatalf("expected 1 issue (seeded standing target only — new issue rolled back), got %d", cnt)
			}
			var newCnt int
			if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1 AND title = $2`,
				pid, "blocked by a standing target").Scan(&newCnt); err != nil {
				t.Fatalf("count new issue: %v", err)
			}
			if newCnt != 0 {
				t.Fatalf("new issue must not exist after rollback, got %d rows", newCnt)
			}
		})
	}
}

// TestCreateInlineDeps_NestedStandingForbidden_TopLevelCode — sk-118 F2e/F2i.
//
// The nested-standing ban (parent-child edge where BOTH ends are standing)
// fires inside dependencies.CreateInTx only when from.Status == "standing" AND
// to.Status == "standing". F2i (create-as-standing) now makes the FROM side
// reachable: skein_create(standing:true) inserts the brand-new issue as
// standing, then runs the inline `parent-child` edge with from=<new standing
// issue> → to=<existing standing parent>. Both ends standing → the ban fires,
// the whole skein_create rolls back, and the error envelope's top-level
// `error` is the actionable `nested_standing_forbidden` code (promoted by
// mapInlineDepError, mirroring blocker_target_standing).
//
// Un-skipped under F2i (previously skipped: the FROM side could not be standing
// until create-as-standing existed).
func TestCreateInlineDeps_NestedStandingForbidden_TopLevelCode(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cide_nest")
	standingParent := prefix + "-1"
	seedIssue(t, pool, pid, userID, standingParent, "standing parent", "standing", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	// Inline parent-child edge from the new (standing) issue to the existing
	// standing parent. standing:true makes the new issue's FROM side standing.
	dep := map[string]any{"toId": standingParent, "type": "parent-child"}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "standing child via inline parent-child edge",
		"type":         "task",
		"kind":         "directed",
		"standing":     true,
		"dependencies": []any{dep},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected isError=true for nested-standing inline edge; body=%+v", body)
	}
	body := extractToolJSON(t, res)
	if body["error"] != "nested_standing_forbidden" {
		t.Fatalf("error = %v, want nested_standing_forbidden; body=%+v", body["error"], body)
	}
	if _, ok := body["dependencyIndex"]; !ok {
		t.Fatalf("dependencyIndex missing in error body=%+v", body)
	}

	// Rollback: only the seeded standing parent remains; the new issue was NOT
	// inserted.
	var cnt int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 issue (standing parent only — new issue rolled back), got %d", cnt)
	}
}

// depMutTok turns a dep type into a project-id-prefix-safe token (the id
// prefix forbids hyphens beyond the first segment).
func depMutTok(depType string) string {
	if depType == "waits-for" {
		return "waitsfor"
	}
	return depType
}

// Sanity import guards — these vars exist so importing
// `projects` and `dependencies` packages aren't flagged unused
// when the tests above are commented for local debugging.
var _ = projects.NewStore
var _ = dependencies.DepBlocks

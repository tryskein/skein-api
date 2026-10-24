package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// TestCreate_HappyPath_NoDuplicates is the simplest path: create an
// issue in a fresh project where no fuzzy-title collisions exist.
// Asserts created=true, decisionRequired=false, and verifies the
// resulting issue row carries source='agent_directed' (Phase 1
// ui-redesign — agent caller with kind="directed").
func TestCreate_HappyPath_NoDuplicates(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ctc")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "shiny new agent issue",
		"type":    "task",
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
	if body["created"] != true {
		t.Fatalf("created = %v, want true", body["created"])
	}
	if body["decisionRequired"] != false {
		t.Fatalf("decisionRequired = %v, want false", body["decisionRequired"])
	}
	issueObj, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue not present or wrong type: %T", body["issue"])
	}
	issueID, _ := issueObj["id"].(string)
	if issueID == "" {
		t.Fatalf("issue.id missing")
	}

	// Verify source='agent_directed' on the actual row.
	var src string
	if err := pool.QueryRow(context.Background(),
		`SELECT source FROM issues WHERE id = $1`, issueID).Scan(&src); err != nil {
		t.Fatalf("verify source: %v", err)
	}
	if src != "agent_directed" {
		t.Fatalf("source = %q, want %q", src, "agent_directed")
	}

	// Verify the events row has actor_type='agent'.
	var actorType, actorSession string
	if err := pool.QueryRow(context.Background(),
		`SELECT actor_type, COALESCE(actor_session_id, '')
		   FROM events
		  WHERE entity_type = 'issue' AND entity_id = $1
		    AND type = 'issue.created'
		  LIMIT 1`, issueID).Scan(&actorType, &actorSession); err != nil {
		t.Fatalf("verify event: %v", err)
	}
	if actorType != "agent" {
		t.Fatalf("event actor_type = %q, want %q", actorType, "agent")
	}
	if actorSession == "" {
		t.Fatalf("event actor_session_id should be populated from ctx")
	}
}

// TestCreate_KindDirected re-asserts that an agent caller passing
// kind="directed" produces source="agent_directed" on the wire and in
// the row. Phase 1 ui-redesign replaces the old "always agent" rule
// with the directed/autonomous split.
func TestCreate_KindDirected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "csrc")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "user-asked issue",
		"type":    "feature",
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
	issueObj := body["issue"].(map[string]any)
	// sk-282 T022: `issue` is the acknowledgement, so `source` — card
	// data, and something the caller supplied as `kind` — is verified on
	// the row rather than in the echo.
	if _, echoed := issueObj["source"]; echoed {
		t.Fatalf("the create acknowledgement echoes source = %v", issueObj["source"])
	}
	issueID, _ := issueObj["id"].(string)
	if issueID == "" {
		t.Fatalf("issue.id missing: %+v", issueObj)
	}
	var src string
	if err := pool.QueryRow(context.Background(),
		`SELECT source FROM issues WHERE id = $1`, issueID).Scan(&src); err != nil {
		t.Fatalf("verify source: %v", err)
	}
	if src != "agent_directed" {
		t.Fatalf("source = %q, want %q", src, "agent_directed")
	}
}

// TestCreate_DuplicateGate creates a "alpha bravo" issue, then attempts
// to create another with the same title — the gate should fire.
func TestCreate_DuplicateGate(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cdup")
	seedIssue(t, pool, pid, userID, prefix+"-1", "Alpha Bravo charlie delta", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Alpha Bravo charlie delta",
		"type":    "task",
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
	if body["created"] != false {
		t.Fatalf("created = %v, want false (duplicate gate)", body["created"])
	}
	if body["decisionRequired"] != true {
		t.Fatalf("decisionRequired = %v, want true", body["decisionRequired"])
	}
	if body["issue"] != nil {
		t.Fatalf("issue should be null when decisionRequired, got %v", body["issue"])
	}
	dups, ok := body["duplicates"].([]any)
	if !ok || len(dups) == 0 {
		t.Fatalf("duplicates should be a non-empty array, got %v", body["duplicates"])
	}
}

// TestCreate_ForceBypassesGate asserts force=true overrides the
// duplicate gate.
func TestCreate_ForceBypassesGate(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cforce")
	seedIssue(t, pool, pid, userID, prefix+"-1", "yankee zulu charlie", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "yankee zulu charlie",
		"type":    "task",
		"force":   true,
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
	if body["created"] != true {
		t.Fatalf("created = %v, want true (force=true)", body["created"])
	}
	if body["decisionRequired"] != false {
		t.Fatalf("decisionRequired = %v, want false", body["decisionRequired"])
	}
}

// TestCreate_DuplicateOfSkipsGateAndAddsEdge asserts that supplying
// duplicateOf both bypasses the gate AND auto-creates the dependency
// edge from the new issue to the supplied one.
func TestCreate_DuplicateOfSkipsGateAndAddsEdge(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cdof")
	originalID := prefix + "-1"
	seedIssue(t, pool, pid, userID, originalID, "tango uniform victor", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":     pid.String(),
		"title":       "tango uniform victor",
		"type":        "task",
		"duplicateOf": originalID,
		"kind":        "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	if body["created"] != true {
		t.Fatalf("created = %v, want true", body["created"])
	}
	issueObj := body["issue"].(map[string]any)
	newID, _ := issueObj["id"].(string)
	if newID == "" {
		t.Fatalf("missing new issue id")
	}

	// Verify a duplicates dep edge exists from newID → originalID.
	var depCount int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'duplicates'`,
		newID, originalID).Scan(&depCount); err != nil {
		t.Fatalf("query dep: %v", err)
	}
	if depCount != 1 {
		t.Fatalf("expected one duplicates edge from %s to %s, got %d", newID, originalID, depCount)
	}
}

// TestCreate_ParentClosedRejected creates an issue, closes it, then
// attempts to add a child — expects parent_closed.
func TestCreate_ParentClosedRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cpcl")
	parentID := prefix + "-1"
	seedIssue(t, pool, pid, userID, parentID, "epic parent", "closed", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"title":    "child of closed",
		"type":     "task",
		"parentId": parentID,
		"kind":     "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for closed parent")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeParentClosed {
		t.Fatalf("error = %v, want %s", got, ErrCodeParentClosed)
	}
}

// TestCreate_CrossProjectParent — sk-35 user decision #1. The new
// issue's project_id points to the caller's currentProj, but the new
// id inherits the parent's prefix. Admin sees both projects so this
// succeeds.
func TestCreate_CrossProjectParent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid1, prefix1 := seedProject(t, pool, userID, "cpwpA")
	pid2, _ := seedProject(t, pool, userID, "cpwpB")
	parentInOther := prefix1 + "-1"
	seedIssue(t, pool, pid1, userID, parentInOther, "other-project parent", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid2.String(),
		"title":    "cross-project child",
		"type":     "task",
		"parentId": parentInOther,
		"kind":     "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("expected success for cross-project parent (sk-35 allows it); body=%+v", body)
	}
	body := extractToolJSON(t, res)
	// Child id should inherit the parent's prefix even though
	// project_id points to pid2.
	issueMap, _ := body["issue"].(map[string]any)
	if issueMap == nil {
		t.Fatalf("issue missing from response: %+v", body)
	}
	gotID, _ := issueMap["id"].(string)
	if !strings.HasPrefix(gotID, parentInOther+".") {
		t.Errorf("child id = %q, want prefix %q.*", gotID, parentInOther)
	}
	// sk-282 T022: projectId is card data — read it off the row.
	var gotProj string
	if err := pool.QueryRow(context.Background(),
		`SELECT project_id FROM issues WHERE id = $1`, gotID).Scan(&gotProj); err != nil {
		t.Fatalf("read back project_id: %v", err)
	}
	if gotProj != pid2.String() {
		t.Errorf("child projectId = %q, want %q (caller's currentProj)", gotProj, pid2.String())
	}
}

// TestCreate_ComponentsWrongProject rejects components belonging to a
// different project.
func TestCreate_ComponentsWrongProject(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid1, _ := seedProject(t, pool, userID, "ccwpA")
	pid2, _ := seedProject(t, pool, userID, "ccwpB")
	// Seed a component in pid1.
	cid := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, 'wrong-comp', '', '[]'::jsonb, NULL, $3, now(), now())`,
		cid, pid1, userID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":      pid2.String(),
		"title":        "issue with wrong-project component",
		"type":         "task",
		"componentIds": []string{cid.String()},
		"kind":         "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for wrong-project component")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeComponentsWrongProject {
		t.Fatalf("error = %v, want %s", got, ErrCodeComponentsWrongProject)
	}
}

// TestCreate_IDTakenInCustomMode triggers an id collision in custom
// mode.
func TestCreate_IDTakenInCustomMode(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cidtkn")
	// Seed an issue with id = prefix-fixed-001 so the next attempt collides.
	taken := prefix + "-fixed-001"
	seedIssue(t, pool, pid, userID, taken, "first", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "second-collision",
		"type":    "task",
		"idMode":  "custom",
		"id":      taken,
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for id collision")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIDTaken {
		t.Fatalf("error = %v, want %s", got, ErrCodeIDTaken)
	}
}

// TestCreate_ProjectNotVisible exercises the visibility branch — a
// non-member sees project_not_found.
func TestCreate_ProjectNotVisible(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "cvis")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "stranger should not get here",
		"type":    "task",
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for invisible project")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

// TestCreate_DuplicateOfNonexistent asserts that when duplicateOf points
// at an id that does not currently exist, the tool returns a
// validation_error WITHOUT persisting the new issue. The earlier
// behaviour committed the new issue first, then failed the dep create —
// leaving partial state. The pre-check fixed in W3 closes that gap.
//
// We seed a target issue, capture its id, delete it, then call
// skein_create with duplicateOf pointing at the now-gone id. The expected
// outcome: validation_error AND no new issue rows for this project.
func TestCreate_DuplicateOfNonexistent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cdofx")

	// Seed a real target, then delete it so the id is well-formed but
	// no longer exists. Captures the genuine "agent has a stale id"
	// scenario from a prior skein_show.
	targetID := prefix + "-1"
	seedIssue(t, pool, pid, userID, targetID, "soon-to-be-deleted", "open", 2)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM issues WHERE id = $1`, targetID); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	// Snapshot the row count for this project before invoking the tool.
	var beforeCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, pid).Scan(&beforeCount); err != nil {
		t.Fatalf("count issues before: %v", err)
	}

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":     pid.String(),
		"title":       "child of nothing",
		"type":        "task",
		"duplicateOf": targetID,
		"kind":        "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for nonexistent duplicateOf, got: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}

	// Atomicity assertion: row count must be unchanged. If the tool had
	// fallen through to CreateIssue before failing the edge, the count
	// would be beforeCount+1.
	var afterCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, pid).Scan(&afterCount); err != nil {
		t.Fatalf("count issues after: %v", err)
	}
	if afterCount != beforeCount {
		t.Fatalf("issue persisted on validation failure: before=%d after=%d", beforeCount, afterCount)
	}
}

// TestCreate_ValidationRejectsMissingTitle ensures the explicit
// validation_error path fires for the canonical missing-input cases.
func TestCreate_ValidationRejectsMissingTitle(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cval")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		// missing title
		"type": "task",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing title")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestCreate_KindAutonomous covers the side-quest path: agent decided
// to create this issue mid-task. Resulting source must land as
// 'agent_autonomous' on the row.
func TestCreate_KindAutonomous(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ckaut")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "agent self-spawned side quest",
		"type":    "chore",
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
	issueObj := body["issue"].(map[string]any)
	if _, echoed := issueObj["source"]; echoed {
		t.Fatalf("the create acknowledgement echoes source = %v", issueObj["source"])
	}
	issueID := issueObj["id"].(string)
	var src string
	if err := pool.QueryRow(t.Context(),
		`SELECT source FROM issues WHERE id = $1`, issueID).Scan(&src); err != nil {
		t.Fatalf("verify source: %v", err)
	}
	if src != "agent_autonomous" {
		t.Fatalf("DB source = %q, want %q", src, "agent_autonomous")
	}
}

// TestCreate_KindMissing — agent caller without `kind` must fail-closed
// with validation_error and persist no row (Q-5 = a).
func TestCreate_KindMissing(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ckmiss")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	var beforeCount int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, pid).Scan(&beforeCount); err != nil {
		t.Fatalf("count issues before: %v", err)
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "no-kind agent issue",
		"type":    "task",
		// kind deliberately absent
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing kind, got: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
	var afterCount int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, pid).Scan(&afterCount); err != nil {
		t.Fatalf("count issues after: %v", err)
	}
	if afterCount != beforeCount {
		t.Fatalf("issue persisted on missing kind: before=%d after=%d", beforeCount, afterCount)
	}
}

// TestCreate_InheritsComponentFromParent_SurfacesWarning asserts the
// MCP-level end-to-end behaviour: when the caller creates a subtask
// via parentId AND omits componentIds, the store inherits the
// parent's components AND the tool wraps a human-readable warning in
// response.warnings[] so the agent surfaces it in chat.
func TestCreate_InheritsComponentFromParent_SurfacesWarning(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cich")
	compID := seedComponent(t, pool, pid, userID, "alpha-comp", "first component")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	// Parent: explicit componentId pin so the child has something to inherit.
	parentReq := mcpgo.CallToolRequest{}
	parentReq.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "parent with component",
		"type":         "feature",
		"kind":         "directed",
		"componentIds": []any{compID.String()},
	}
	parentRes, err := handler(ctx, parentReq)
	if err != nil {
		t.Fatalf("parent create error: %v", err)
	}
	if parentRes.IsError {
		t.Fatalf("parent create unexpectedly errored: %+v", parentRes)
	}
	parentBody := extractToolJSON(t, parentRes)
	parentIssue, _ := parentBody["issue"].(map[string]any)
	parentID, _ := parentIssue["id"].(string)
	if parentID == "" {
		t.Fatalf("parent.id missing: %+v", parentBody)
	}
	_ = prefix

	// Child: parentId set, componentIds key OMITTED — should inherit.
	childReq := mcpgo.CallToolRequest{}
	childReq.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"title":    "subtask without componentIds",
		"type":     "task",
		"kind":     "directed",
		"parentId": parentID,
	}
	childRes, err := handler(ctx, childReq)
	if err != nil {
		t.Fatalf("child create error: %v", err)
	}
	if childRes.IsError {
		t.Fatalf("child create errored: %+v", childRes)
	}
	childBody := extractToolJSON(t, childRes)

	// Warnings array must mention the inherited component by name.
	rawWarnings, ok := childBody["warnings"].([]any)
	if !ok {
		t.Fatalf("warnings missing or wrong type: %T", childBody["warnings"])
	}
	if len(rawWarnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one entry", rawWarnings)
	}
	gotWarning, _ := rawWarnings[0].(string)
	if !strings.Contains(gotWarning, "alpha-comp") {
		t.Errorf("warning %q does not mention component name 'alpha-comp'", gotWarning)
	}
	if !strings.Contains(gotWarning, "inherited from the parent") {
		t.Errorf("warning %q missing 'inherited from the parent' substring", gotWarning)
	}
	if !strings.Contains(gotWarning, "skein_update") {
		t.Errorf("warning %q missing 'skein_update' override hint", gotWarning)
	}

	// Matrix row 83 (sk-282 T022): the warning survives the switch to the
	// acknowledgement because it lives in the ENVELOPE, not on the card —
	// asserted here so a future flattening of the envelope into the
	// acknowledgement cannot take it out silently.
	childIssue, ok := childBody["issue"].(map[string]any)
	if !ok {
		t.Fatalf("child issue missing or not an object: %+v", childBody["issue"])
	}
	assertAcknowledgementShape(t, childIssue)
	if _, echoed := childIssue["componentIds"]; echoed {
		t.Errorf("the create acknowledgement echoes componentIds = %v — the inherited set "+
			"is reported by the warning and read back with skein_show",
			childIssue["componentIds"])
	}

	// Child issue must carry the component (real DB state, not just response).
	childID, _ := childIssue["id"].(string)
	var compCount int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM issue_components WHERE issue_id = $1 AND component_id = $2`,
		childID, compID).Scan(&compCount); err != nil {
		t.Fatalf("verify child component: %v", err)
	}
	if compCount != 1 {
		t.Fatalf("child %s not pinned to component %s (count=%d)", childID, compID, compCount)
	}
}

// TestCreate_ExplicitEmptyComponentIds_SuppressesInheritance asserts
// that passing `componentIds: []` (explicit empty array) overrides the
// parent-inheritance default — the child lands with no components and
// no warning is emitted.
func TestCreate_ExplicitEmptyComponentIds_SuppressesInheritance(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ceei")
	compID := seedComponent(t, pool, pid, userID, "would-inherit", "")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	parentReq := mcpgo.CallToolRequest{}
	parentReq.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "parent",
		"type":         "feature",
		"kind":         "directed",
		"componentIds": []any{compID.String()},
	}
	parentRes, _ := handler(ctx, parentReq)
	parentBody := extractToolJSON(t, parentRes)
	parentIssue, _ := parentBody["issue"].(map[string]any)
	parentID, _ := parentIssue["id"].(string)

	// Child: parentId + explicit empty componentIds.
	childReq := mcpgo.CallToolRequest{}
	childReq.Params.Arguments = map[string]any{
		"project":      pid.String(),
		"title":        "explicit empty",
		"type":         "task",
		"kind":         "directed",
		"parentId":     parentID,
		"componentIds": []any{},
	}
	childRes, err := handler(ctx, childReq)
	if err != nil {
		t.Fatalf("child create error: %v", err)
	}
	if childRes.IsError {
		t.Fatalf("child create errored: %+v", childRes)
	}
	childBody := extractToolJSON(t, childRes)

	rawWarnings, _ := childBody["warnings"].([]any)
	if len(rawWarnings) != 0 {
		t.Errorf("warnings = %v, want empty (explicit override suppresses)", rawWarnings)
	}

	childIssue, _ := childBody["issue"].(map[string]any)
	childID, _ := childIssue["id"].(string)
	var compCount int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM issue_components WHERE issue_id = $1`, childID).Scan(&compCount); err != nil {
		t.Fatalf("count child components: %v", err)
	}
	if compCount != 0 {
		t.Fatalf("child has %d components, want 0 (explicit empty override)", compCount)
	}
}

// TestCreate_KindUnknown — agent caller with an unrecognised kind must
// fail-closed with validation_error.
//
// mcp-go's enum schema validation runs server-side at the SDK layer; in
// unit tests we bypass it by calling the handler directly, so we still
// hit the handler-level guard. Pinning the guard explicitly keeps the
// fail-closed contract regardless of upstream schema enforcement.
func TestCreate_KindUnknown(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ckbad")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "bogus kind issue",
		"type":    "task",
		"kind":    "foobar",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for unknown kind, got: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// --- sk-118 (F2i): create-as-standing via skein_create ---

// TestCreate_StandingFlag_NoOwnerlessWarning verifies that skein_create with
// standing:true and NO assignee produces a clean response with NO ownerless /
// unassigned warning. There is no ownerless warning on the create path today
// (the ownerless nudge lives on skein_close, sk-71), so the control — an
// ordinary create with no assignee — likewise carries no ownerless warning.
// This pins that a standing create surfaces no claim/owner-discipline warning
// (a perpetual obligation with no assignee is normal, not work waiting on
// someone) and lands the row as status=standing.
func TestCreate_StandingFlag_NoOwnerlessWarning(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ctstand")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	// Standing create, no assignee.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"title":    "perpetual standing obligation",
		"type":     "task",
		"kind":     "directed",
		"standing": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	if body["created"] != true {
		t.Fatalf("created = %v, want true", body["created"])
	}
	issueObj, _ := body["issue"].(map[string]any)
	if got := issueObj["status"]; got != "standing" {
		t.Fatalf("issue status = %v, want standing", got)
	}
	assertNoOwnerlessWarning(t, body)

	// Control: ordinary (open) create, no assignee — also no ownerless warning.
	// sk-565 (US-1): without `assignee` the creator's «assign to me» default
	// would now assign the control; `unassigned: true` keeps it the
	// unassigned issue this control is about.
	req2 := mcpgo.CallToolRequest{}
	req2.Params.Arguments = map[string]any{
		"project":    pid.String(),
		"title":      "ordinary unassigned issue",
		"type":       "task",
		"kind":       "directed",
		"unassigned": true,
	}
	res2, err := handler(ctx, req2)
	if err != nil {
		t.Fatalf("handler error (control): %v", err)
	}
	if res2.IsError {
		t.Fatalf("unexpected isError=true (control): %+v", res2)
	}
	assertNoOwnerlessWarning(t, extractToolJSON(t, res2))
}

// assertNoOwnerlessWarning fails if any warning string in the create response
// reads like an ownerless / unassigned / claim-discipline nudge.
func assertNoOwnerlessWarning(t *testing.T, body map[string]any) {
	t.Helper()
	raw, ok := body["warnings"].([]any)
	if !ok {
		// warnings is always a (possibly empty) array; nil/absent is fine too.
		return
	}
	for _, w := range raw {
		s, _ := w.(string)
		ls := strings.ToLower(s)
		if strings.Contains(ls, "no owner") || strings.Contains(ls, "ownerless") ||
			strings.Contains(ls, "no assignee") || strings.Contains(ls, "unassigned") ||
			strings.Contains(ls, "skein_claim") {
			t.Fatalf("unexpected ownerless/unassigned warning on create: %q", s)
		}
	}
}

// TestCreate_StandingUnderStandingParent_Rejected drives exactly
// skein_create(parentId=<standing>, standing:true) and asserts the top-level
// error is nested_standing_forbidden, with the child NOT inserted (only the
// seeded standing parent remains).
func TestCreate_StandingUnderStandingParent_Rejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ctnest")

	standingParent := prefix + "-1"
	seedIssue(t, pool, pid, userID, standingParent, "standing parent", "standing", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"title":    "standing child under standing parent",
		"type":     "task",
		"kind":     "directed",
		"parentId": standingParent,
		"standing": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for standing-under-standing, got: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeNestedStandingForbidden {
		t.Fatalf("error = %v, want %s; body=%+v", got, ErrCodeNestedStandingForbidden, body)
	}

	// Child NOT inserted: only the seeded standing parent exists.
	var cnt int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 issue (standing parent only, child rolled back), got %d", cnt)
	}
}

// TestCreate_NonStandingUnderStandingParent_Allowed is the control: an ordinary
// (non-standing) child under a standing parent is fine.
func TestCreate_NonStandingUnderStandingParent_Allowed(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ctnestok")

	standingParent := prefix + "-1"
	seedIssue(t, pool, pid, userID, standingParent, "standing parent", "standing", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)

	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"title":    "ordinary child under standing parent",
		"type":     "task",
		"kind":     "directed",
		"parentId": standingParent,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true for non-standing child under standing parent: %+v", res)
	}
	resBody := extractToolJSON(t, res)
	if resBody["created"] != true {
		t.Fatalf("created = %v, want true", resBody["created"])
	}
	issueObj, _ := resBody["issue"].(map[string]any)
	if got := issueObj["status"]; got != "open" {
		t.Fatalf("child status = %v, want open", got)
	}
}

// TestCreate_SC007_AcknowledgementFitsTheBudget is matrix row 81.
//
// "SC-007: a create without a duplicate hit is under 250 characters and carries
// the created issue's version (INV-7)."
//
// The fixture is hostile in the direction that matters for a create: the
// call supplies EVERY free-text field the tool accepts, including
// `sourceContext`, which US-5 names as one of the two echoes measured on
// this surface. A create with a bare title would pass this gate no
// matter what the response carried.
func TestCreate_SC007_AcknowledgementFitsTheBudget(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "sc007")

	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool),
		projects.NewStore(pool))
	ctx := withCtx(t, userID, "admin")

	const long = "SENTINEL body supplied by the caller on this very call, long enough that " +
		"echoing it back would blow a 250-character budget several times over."
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":            pid.String(),
		"title":              "SENTINEL title of a genuinely new issue nobody has filed before",
		"type":               "task",
		"kind":               "autonomous",
		"description":        long,
		"design":             long,
		"acceptanceCriteria": long,
		"notes":              long,
		"sourceContext":      "SENTINEL source context: while implementing sk-282.18.4.5",
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

	if body["created"] != true || body["decisionRequired"] != false {
		t.Fatalf("this fixture must create without firing the gate: %s", raw)
	}
	issueObj, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue missing or not an object: %s", raw)
	}
	assertAcknowledgementShape(t, issueObj)
	// INV-7 spelled out: the version is present AND correct, because a
	// create is where the optimistic cycle usually starts.
	if v, _ := issueObj["version"].(float64); int(v) != 1 {
		t.Errorf("issue.version = %v, want 1 — without it the caller must re-read before "+
			"its first conditional update", issueObj["version"])
	}
	if strings.Contains(raw, "SENTINEL") {
		t.Errorf("create response echoes text the caller just supplied:\n%s", raw)
	}
	// SC-007. Literal threshold on purpose — see the SC-006 test.
	if n := len(raw); n >= 250 {
		t.Errorf("create response is %d characters, SC-007 budget is < 250:\n%s", n, raw)
	}
	t.Logf("SC-007: create response is %d characters: %s", len(raw), raw)

	// INV-3: every field is in the system and reachable by a reading tool.
	issueID, _ := issueObj["id"].(string)
	var description, design, acceptance, notes, sourceContext string
	if err := pool.QueryRow(context.Background(), `
SELECT description, design, acceptance_criteria, notes, coalesce(source_context, '')
  FROM issues WHERE id = $1`, issueID).
		Scan(&description, &design, &acceptance, &notes, &sourceContext); err != nil {
		t.Fatalf("read back the prose: %v", err)
	}
	for name, got := range map[string]string{
		"description": description, "design": design,
		"acceptanceCriteria": acceptance, "notes": notes,
	} {
		if got != long {
			t.Errorf("stored %s = %q, want the value as sent", name, got)
		}
	}
	if !strings.HasPrefix(sourceContext, "SENTINEL source context") {
		t.Errorf("stored sourceContext = %q, want the value as sent", sourceContext)
	}
}

// TestCreate_INV6_DuplicateMachineryIntactAcrossAllThreeBranches is
// matrix row 82.
//
// "INV-6: on a duplicate hit the candidate set, the decision-required
// flag and the three-way fork are preserved in full; length over the
// threshold is legal."
//
// All three branches of the rubric are walked on ONE fixture in ONE
// test, because the invariant is about the rubric as a whole: three
// separate tests can each pass while the branches stop agreeing about
// which issue is the original.
func TestCreate_INV6_DuplicateMachineryIntactAcrossAllThreeBranches(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "inv6")
	originalID := prefix + "-1"
	seedIssue(t, pool, pid, userID, originalID, "Alpha Bravo charlie delta", "open", 2)

	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool),
		projects.NewStore(pool))
	ctx := withCtx(t, userID, "admin")

	call := func(t *testing.T, extra map[string]any) (map[string]any, string) {
		t.Helper()
		args := map[string]any{
			"project": pid.String(),
			"title":   "Alpha Bravo charlie delta",
			"type":    "task",
			"kind":    "directed",
		}
		for k, v := range extra {
			args[k] = v
		}
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handler(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		return extractToolJSON(t, res), toolText(t, res)
	}

	// ---- branch 1: stop and ask ----
	gate, gateRaw := call(t, nil)
	if gate["created"] != false {
		t.Fatalf("created = %v, want false on the duplicate gate", gate["created"])
	}
	if gate["decisionRequired"] != true {
		t.Fatalf("decisionRequired = %v, want true", gate["decisionRequired"])
	}
	if gate["issue"] != nil {
		t.Errorf("issue = %v, want null — nothing was created, so there is no post-state",
			gate["issue"])
	}
	candidates, ok := gate["duplicates"].([]any)
	if !ok || len(candidates) == 0 {
		t.Fatalf("duplicates missing or empty: %s", gateRaw)
	}
	// The candidate must keep its FULL declared field set. The expected
	// keys are derived from dependencies.DuplicateCandidate's tags, not
	// transcribed: a hand-written list would agree with the code today
	// and lie the moment a field is added or renamed.
	wantKeys := jsonTagNames(t, reflect.TypeOf(dependencies.DuplicateCandidate{}))
	first, ok := candidates[0].(map[string]any)
	if !ok {
		t.Fatalf("duplicates[0] is %T, want an object", candidates[0])
	}
	for k := range first {
		if !wantKeys[k] {
			t.Errorf("duplicate candidate carries an undeclared key %q", k)
		}
	}
	// Everything except the optional semantic score must be present —
	// dropping any of them would weaken the hint the agent decides on.
	for _, must := range []string{"id", "title", "status", "type", "similarity"} {
		if _, present := first[must]; !present {
			t.Errorf("duplicate candidate has no %q: %v", must, first)
		}
	}
	if first["id"] != originalID {
		t.Errorf("duplicates[0].id = %v, want the original %s", first["id"], originalID)
	}
	// Edge, explicitly: this response is allowed to exceed SC-007. It
	// carries the candidate set, which is "what the next step hangs
	// on" and not an echo.
	t.Logf("decision-required response is %d characters (legitimately over SC-007's 250)",
		len(gateRaw))

	// ---- branch 2: force ----
	forced, forcedRaw := call(t, map[string]any{"force": true})
	if forced["created"] != true || forced["decisionRequired"] != false {
		t.Fatalf("force branch did not create: %s", forcedRaw)
	}
	forcedIssue, ok := forced["issue"].(map[string]any)
	if !ok {
		t.Fatalf("force branch issue missing: %s", forcedRaw)
	}
	assertAcknowledgementShape(t, forcedIssue)

	// ---- branch 3: duplicateOf ----
	linked, linkedRaw := call(t, map[string]any{"duplicateOf": originalID})
	if linked["created"] != true || linked["decisionRequired"] != false {
		t.Fatalf("duplicateOf branch did not create: %s", linkedRaw)
	}
	linkedIssue, ok := linked["issue"].(map[string]any)
	if !ok {
		t.Fatalf("duplicateOf branch issue missing: %s", linkedRaw)
	}
	assertAcknowledgementShape(t, linkedIssue)
	linkedID, _ := linkedIssue["id"].(string)
	// The edge is the whole point of this branch and it is NOT visible in
	// the acknowledgement — so it is verified in the graph.
	var edges int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'duplicates'`,
		linkedID, originalID).Scan(&edges); err != nil {
		t.Fatalf("count duplicates edge: %v", err)
	}
	if edges != 1 {
		t.Errorf("duplicateOf produced %d duplicates edges from %s to %s, want 1",
			edges, linkedID, originalID)
	}
}

// TestCreate_TwoDistinctIssuesBackToBack is the sk-228 acceptance
// regression at the tool boundary: two issues that share nothing but a
// language, created one immediately after the other — well inside the
// window where the first has no embedding yet — must both be created.
//
// The reported symptom was the second create coming back
// `decisionRequired:true` with `similarity: 1` against the first. Root
// cause was pg_trgm scoring the two titles' surviving RESIDUES rather
// than the titles (a ctype=C database discards every Cyrillic
// character), so the gate is what this test aims at: the second call
// must return created=true.
func TestCreate_TwoDistinctIssuesBackToBack(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cb2b")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	// Two pairs. The first reproduces the reported shape (Russian
	// titles, one shared bracket prefix), but it only goes red on a
	// ctype=C database, and CI's image reads Cyrillic fine. The second
	// collides the same way on EVERY ctype, because dashes and brackets
	// are non-alphanumeric everywhere: with the guard removed pg_trgm
	// scores those two at exactly 1.0. Keeping both means this test fails
	// on either cluster flavour if the fix regresses, instead of passing
	// for a reason unrelated to the bug.
	titles := []string{
		"[feat] The filter list grows without bound — fold rare ones into a separate menu",
		"[feat] A hotkey hint next to the search field",
		"—— Outbox ——————————————————————",
		"«»(),.;: Outbox !!! ??? /// +++",
	}
	for i, title := range titles {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project": pid.String(),
			"title":   title,
			"type":    "task",
			"kind":    "directed",
		}
		res, err := handler(ctx, req)
		if err != nil {
			t.Fatalf("create #%d: handler error: %v", i+1, err)
		}
		if res.IsError {
			t.Fatalf("create #%d: unexpected isError=true: %+v", i+1, res)
		}
		body := extractToolJSON(t, res)
		if body["created"] != true {
			t.Fatalf("create #%d (%q) was blocked as a duplicate: %+v",
				i+1, title, body)
		}
	}
}

// TestCreate_ResponseAlwaysCarriesDegraded pins the sk-228 wire shape:
// `degraded` is present on every skein_create outcome, and every
// duplicate row carries the same key set — `similarity`,
// `trigramSimilarity`, `semanticSimilarity`, `matchedBy` — with `null`
// standing for "this layer did not score this row".
//
// Before sk-228 `semanticSimilarity` was omitempty, so its ABSENCE was
// the only signal that the trigram layer had decided alone. A consumer
// reading provenance off which keys exist cannot tell "the vector layer
// scored this at the same value" from "the vector layer never ran" —
// and that ambiguity is what let a fabricated 1.0 look like consensus.
func TestCreate_ResponseAlwaysCarriesDegraded(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cdeg")
	seedIssue(t, pool, pid, userID, prefix+"-1", "Alpha Bravo charlie delta", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	handler := handleCreate(pool, issuesStore, depsStore, projectsStore)
	ctx := withCtx(t, userID, "admin")

	// (a) blocked outcome — the duplicate row must be fully described.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Alpha Bravo charlie delta",
		"type":    "task",
		"kind":    "directed",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if _, ok := body["degraded"]; !ok {
		t.Fatalf("`degraded` missing from the decision-required response: %+v", body)
	}
	dups, ok := body["duplicates"].([]any)
	if !ok || len(dups) == 0 {
		t.Fatalf("expected a duplicate row, got %v", body["duplicates"])
	}
	row, ok := dups[0].(map[string]any)
	if !ok {
		t.Fatalf("duplicate row is not an object: %v", dups[0])
	}
	for _, key := range []string{
		"similarity", "trigramSimilarity", "semanticSimilarity", "matchedBy",
	} {
		if _, present := row[key]; !present {
			t.Fatalf("duplicate row is missing %q — the key set must be uniform "+
				"across branches so provenance is read, not guessed: %+v", key, row)
		}
	}
	if row["matchedBy"] != "trigram" {
		t.Fatalf("matchedBy = %v, want \"trigram\" (no embedder wired in)", row["matchedBy"])
	}
	if row["semanticSimilarity"] != nil {
		t.Fatalf("semanticSimilarity must be null, not absent or scored, got %v",
			row["semanticSimilarity"])
	}

	// (b) created outcome — same field, still present.
	req2 := mcpgo.CallToolRequest{}
	req2.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "Something completely unrelated about invoices",
		"type":    "task",
		"kind":    "directed",
	}
	res2, err := handler(ctx, req2)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body2 := extractToolJSON(t, res2)
	if body2["created"] != true {
		t.Fatalf("expected created=true, got %+v", body2)
	}
	if _, ok := body2["degraded"]; !ok {
		t.Fatalf("`degraded` missing from the created response: %+v", body2)
	}
	if body2["degraded"] != true {
		t.Fatalf("degraded = %v, want true — no embedder is wired in, so the "+
			"check ran on pg_trgm alone and the response must say so",
			body2["degraded"])
	}
}

// TestMapCreateError_SerializationRetry pins the sk-350.3 fix at the MCP
// boundary, DB-free: CreateIssue exhausting createMaxAttempts against a
// genuine SERIALIZABLE abort now returns issues.ErrSerializationRetry (see
// Store.CreateIssue), and mapCreateError must turn that into
// `serialization_retry` — NOT fall through to the ErrCodeInternalError
// default, which is what happened before this fix (the sentinel had no
// branch at this call site at all; see
// TestCreateIssue_SerializationFailure_Exhausted in the issues package for
// the store-level, DB-backed half of this pin).
//
// Checked against ErrCodeVersionConflict too, same spirit as
// TestMapCommentWriteError_SerializationRetryIsNotVersionConflict /
// TestMapClaimError_SerializationRetryIsNotAlreadyClaimed: the issue was
// never created, so there is no version a re-read could quote, and a
// caller told "version stale, re-read" would have nothing to re-read.
func TestMapCreateError_SerializationRetry(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bare sentinel", issues.ErrSerializationRetry},
		{"wrapped sentinel", fmt.Errorf("creating issue: %w", issues.ErrSerializationRetry)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mapCreateError(context.Background(), tc.err, uuid.New(), uuid.New())
			if res == nil {
				t.Fatalf("mapCreateError returned nil")
			}
			if !res.IsError {
				t.Fatalf("mapCreateError(%v) produced a non-error result", tc.err)
			}
			body := extractToolJSON(t, res)
			if got := body["error"]; got != ErrCodeSerializationRetry {
				t.Fatalf("error = %v, want %s", got, ErrCodeSerializationRetry)
			}
			if got := body["error"]; got == ErrCodeVersionConflict {
				t.Fatalf("a lost serialization race reported as %s — the issue was never created", ErrCodeVersionConflict)
			}
			if _, has := body["currentVersion"]; has {
				t.Fatalf("serialization_retry response carries currentVersion %v — there is no version to report", body["currentVersion"])
			}
		})
	}
}

// TestMapCreateError_UnknownErrorIsInternal is the baseline the retry
// branch above must not swallow: an error that is NOT
// issues.ErrSerializationRetry still falls through to
// ErrCodeInternalError, same as before this fix.
func TestMapCreateError_UnknownErrorIsInternal(t *testing.T) {
	res := mapCreateError(context.Background(), errors.New("connection reset"), uuid.New(), uuid.New())
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeInternalError {
		t.Fatalf("error = %v, want %s", got, ErrCodeInternalError)
	}
}

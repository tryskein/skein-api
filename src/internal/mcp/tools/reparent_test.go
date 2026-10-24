package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// setIssueType flips a seeded issue's type column. seedIssue always
// inserts type='task'; epic rollup (IssueDetail.Epic) only populates for
// type='epic', so the reparent rollup tests promote the epic + waves.
func setIssueType(t *testing.T, pool *pgxpool.Pool, id, typ string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET type = $2 WHERE id = $1`, id, typ); err != nil {
		t.Fatalf("set issue %s type=%s: %v", id, typ, err)
	}
}

// seedParentChild inserts a canonical parent-child edge (from=child,
// to=parent) directly. Used to build the initial hierarchy before a
// reparent test moves edges around.
func seedParentChild(t *testing.T, pool *pgxpool.Pool, child, parent string, creator uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`,
		child, parent, creator); err != nil {
		t.Fatalf("seed parent-child %s->%s: %v", child, parent, err)
	}
}

// issueVersion reads the live version of an issue — handy for supplying
// expectedVersion to the reparent handler after seeding.
func issueVersion(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version for %s: %v", id, err)
	}
	return v
}

// TestReparent_MovesLeavesUnderWaves is the headline integration test for
// sk-43. It builds an epic with two "wave" tasks and three leaf tasks (all
// direct children of the epic), then reparents the leaves under the waves
// and asserts:
//
//   - each moved leaf's parentIdCurrent == its wave
//   - the epic's rollup totalChildren now counts only the waves (the
//     grandchildren no longer count, since the rollup is direct-children-only)
//   - a wave's rollup counts its newly-acquired children
func TestReparent_MovesLeavesUnderWaves(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rpw")

	epic := seedIssue(t, pool, pid, userID, prefix+"-1", "epic", "open", 2)
	waveA := seedIssue(t, pool, pid, userID, prefix+"-2", "wave A", "open", 2)
	waveB := seedIssue(t, pool, pid, userID, prefix+"-3", "wave B", "open", 2)
	leaf1 := seedIssue(t, pool, pid, userID, prefix+"-4", "leaf 1", "open", 2)
	leaf2 := seedIssue(t, pool, pid, userID, prefix+"-5", "leaf 2", "open", 2)
	leaf3 := seedIssue(t, pool, pid, userID, prefix+"-6", "leaf 3", "open", 2)

	// Epic + waves are epic-typed so GetIssue populates the rollup.
	setIssueType(t, pool, epic, "epic")
	setIssueType(t, pool, waveA, "epic")
	setIssueType(t, pool, waveB, "epic")

	// Initial hierarchy: epic is parent of everything (2 waves + 3 leaves).
	for _, child := range []string{waveA, waveB, leaf1, leaf2, leaf3} {
		seedParentChild(t, pool, child, epic, userID)
	}

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleReparent(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	// Move leaf1, leaf2 under waveA; leaf3 under waveB.
	moves := []struct {
		leaf      string
		newParent string
	}{
		{leaf1, waveA},
		{leaf2, waveA},
		{leaf3, waveB},
	}
	for _, m := range moves {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id":              m.leaf,
			"newParentId":     m.newParent,
			"expectedVersion": issueVersion(t, pool, m.leaf),
		}
		res, err := handler(ctx, req)
		if err != nil {
			t.Fatalf("reparent %s -> %s: handler err: %v", m.leaf, m.newParent, err)
		}
		if res.IsError {
			body := extractToolJSON(t, res)
			t.Fatalf("reparent %s -> %s unexpected isError: %+v", m.leaf, m.newParent, body)
		}
		body := extractToolJSON(t, res)
		// sk-282 T020: the tool acknowledges rather than returning the
		// card, so the new parent is verified where it lives — on the
		// edge — and the acknowledgement is checked for the version the
		// caller continues its optimistic cycle with (INV-7).
		if got, _ := body["version"].(float64); int(got) < 2 {
			t.Fatalf("leaf %s ack version = %v, want >= 2 after a move", m.leaf, body["version"])
		}
		assertParentEdge(t, pool, m.leaf, m.newParent)
	}

	// Epic rollup: only the two waves remain as direct children.
	epicDetail, err := issuesStore.GetIssue(ctx, epic)
	if err != nil {
		t.Fatalf("get epic: %v", err)
	}
	if epicDetail.Epic == nil {
		t.Fatalf("epic rollup nil; expected populated for type=epic")
	}
	if epicDetail.Epic.TotalChildren != 2 {
		t.Fatalf("epic totalChildren = %d, want 2 (waves only)", epicDetail.Epic.TotalChildren)
	}

	// waveA rollup: two leaves now under it.
	waveADetail, err := issuesStore.GetIssue(ctx, waveA)
	if err != nil {
		t.Fatalf("get waveA: %v", err)
	}
	if waveADetail.Epic == nil {
		t.Fatalf("waveA rollup nil; expected populated for type=epic")
	}
	if waveADetail.Epic.TotalChildren != 2 {
		t.Fatalf("waveA totalChildren = %d, want 2", waveADetail.Epic.TotalChildren)
	}

	// waveB rollup: one leaf under it.
	waveBDetail, err := issuesStore.GetIssue(ctx, waveB)
	if err != nil {
		t.Fatalf("get waveB: %v", err)
	}
	if waveBDetail.Epic == nil {
		t.Fatalf("waveB rollup nil; expected populated for type=epic")
	}
	if waveBDetail.Epic.TotalChildren != 1 {
		t.Fatalf("waveB totalChildren = %d, want 1", waveBDetail.Epic.TotalChildren)
	}
}

// TestReparent_NotifiesWatcher — a re-parent fans out an issue.reparented
// notification to a watcher who is not the actor (sk-43). Proves the
// notifications.FanOut wiring inside Store.Reparent + the "Tree move"
// registry entry actually materialise an inbox row.
func TestReparent_NotifiesWatcher(t *testing.T) {
	pool := newTestPool(t)
	actor, _ := seedUser(t, pool, "admin")
	watcher, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, actor, "rpnw")

	// Watcher must be a project member — FanOut rechecks membership before
	// delivering to an issue_watchers row (no cross-boundary leak).
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT (project_id, user_id) DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, watcher, actor); err != nil {
		t.Fatalf("add watcher as member: %v", err)
	}

	parent := seedIssue(t, pool, pid, actor, prefix+"-1", "parent", "open", 2)
	child := seedIssue(t, pool, pid, actor, prefix+"-2", "child", "open", 2)

	// Watcher subscribes to the child issue.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual'], now())
ON CONFLICT (issue_id, user_id) DO NOTHING`, child, watcher); err != nil {
		t.Fatalf("seed issue watcher: %v", err)
	}

	// sk-303.13 / T007: "Tree move" is off in the narrow default, and
	// the fan-out filters at write time. This test is about the FanOut wiring
	// inside Reparent, so the reason it depends on is switched on explicitly
	// rather than inherited from a default it does not control.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'reparent', true, false)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true`, watcher); err != nil {
		t.Fatalf("enable reparent reason: %v", err)
	}
	// Same reasoning for the agent-action filter, which since sk-343 is ON by
	// default: an MCP call is an AGENT action, so the default would take this
	// watcher out of the subscription pools and the row under test would never
	// be written. That default is the subject of its own tests
	// (internal/notifications/fanout_agent_suppression_test.go); here it is a
	// precondition this test does not control, so it is stated out loud.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, updated_at)
VALUES ($1, false, now())
ON CONFLICT (user_id) DO UPDATE SET suppress_agent_actions = false`, watcher); err != nil {
		t.Fatalf("clear agent-action suppression for the watcher: %v", err)
	}

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleReparent(pool, depsStore, issuesStore)
	ctx := withCtx(t, actor, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              child,
		"newParentId":     parent,
		"expectedVersion": issueVersion(t, pool, child),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}

	// The watcher must have an issue.reparented notification; the actor
	// must NOT (self-action exclusion).
	var watcherRows int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1 AND e.type = 'issue.reparented' AND e.entity_id = $2`,
		watcher, child).Scan(&watcherRows); err != nil {
		t.Fatalf("count watcher notifications: %v", err)
	}
	if watcherRows != 1 {
		t.Fatalf("watcher issue.reparented notifications = %d, want 1", watcherRows)
	}

	var actorRows int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1 AND e.type = 'issue.reparented' AND e.entity_id = $2`,
		actor, child).Scan(&actorRows); err != nil {
		t.Fatalf("count actor notifications: %v", err)
	}
	if actorRows != 0 {
		t.Fatalf("actor issue.reparented notifications = %d, want 0 (self-action excluded)", actorRows)
	}
}

// TestReparent_VersionConflict — supplying a stale expectedVersion returns
// version_conflict with the live currentVersion echoed.
func TestReparent_VersionConflict(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rpvc")
	parent := seedIssue(t, pool, pid, userID, prefix+"-1", "parent", "open", 2)
	child := seedIssue(t, pool, pid, userID, prefix+"-2", "child", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleReparent(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              child,
		"newParentId":     parent,
		"expectedVersion": 999, // stale
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for version conflict")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeVersionConflict {
		t.Fatalf("error = %v, want %s", got, ErrCodeVersionConflict)
	}
	if _, ok := body["currentVersion"]; !ok {
		t.Fatalf("version_conflict body missing currentVersion: %+v", body)
	}
}

// TestReparent_Self — id == newParentId returns reparent_self.
func TestReparent_Self(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rps")
	a := seedIssue(t, pool, pid, userID, prefix+"-1", "a", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleReparent(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              a,
		"newParentId":     a,
		"expectedVersion": issueVersion(t, pool, a),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for self-parent")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeReparentSelf {
		t.Fatalf("error = %v, want %s", got, ErrCodeReparentSelf)
	}
}

// TestReparent_ReplacesExistingParent — an issue already parented under X
// can be moved under Y via reparent (the existing edge is replaced, not
// duplicated). Asserts parentIdCurrent flips and no parent_already_set
// surfaces (that guard is on skein_dep_add, not reparent).
func TestReparent_ReplacesExistingParent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rprep")
	parentX := seedIssue(t, pool, pid, userID, prefix+"-1", "parent X", "open", 2)
	parentY := seedIssue(t, pool, pid, userID, prefix+"-2", "parent Y", "open", 2)
	child := seedIssue(t, pool, pid, userID, prefix+"-3", "child", "open", 2)

	// Child starts under parentX.
	seedParentChild(t, pool, child, parentX, userID)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleReparent(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              child,
		"newParentId":     parentY,
		"expectedVersion": issueVersion(t, pool, child),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError replacing parent: %+v", body)
	}
	body := extractToolJSON(t, res)
	if got, _ := body["version"].(float64); int(got) < 2 {
		t.Fatalf("ack version = %v, want >= 2 after a move", body["version"])
	}
	assertParentEdge(t, pool, child, parentY)

	// Exactly one parent-child edge should remain (the X edge was removed).
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies WHERE from_id = $1 AND type = 'parent-child'`,
		child).Scan(&n); err != nil {
		t.Fatalf("count parent edges: %v", err)
	}
	if n != 1 {
		t.Fatalf("parent-child edge count = %d, want 1 (old edge replaced)", n)
	}
}

// TestReparent_CrossProjectRejected — the new parent in a different
// project returns reparent_cross_project.
func TestReparent_CrossProjectRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pidA, prefA := seedProject(t, pool, userID, "rpcpa")
	pidB, prefB := seedProject(t, pool, userID, "rpcpb")
	child := seedIssue(t, pool, pidA, userID, prefA+"-1", "child", "open", 2)
	foreignParent := seedIssue(t, pool, pidB, userID, prefB+"-1", "foreign", "open", 2)

	depsStore := dependencies.NewStore(pool)
	issuesStore := issues.NewStore(pool)
	handler := handleReparent(pool, depsStore, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              child,
		"newParentId":     foreignParent,
		"expectedVersion": issueVersion(t, pool, child),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for cross-project reparent")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeReparentCrossProject {
		t.Fatalf("error = %v, want %s", got, ErrCodeReparentCrossProject)
	}
}

// assertParentEdge proves the move landed by reading the single
// parent-child edge out of the graph.
//
// It replaces an assertion on `parentIdCurrent` in the tool's response:
// that key is card data, and since sk-282 T020 skein_reparent answers
// with an acknowledgement. Reading the edge is also the stricter oracle
// — parentIdCurrent is a derived projection of exactly this row.
func assertParentEdge(t *testing.T, pool *pgxpool.Pool, child, wantParent string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT to_id FROM dependencies WHERE from_id = $1 AND type = 'parent-child'`, child)
	if err != nil {
		t.Fatalf("read parent edges of %s: %v", child, err)
	}
	defer rows.Close()
	var parents []string
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			t.Fatalf("scan parent edge: %v", err)
		}
		parents = append(parents, to)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate parent edges: %v", err)
	}
	if len(parents) != 1 || parents[0] != wantParent {
		t.Fatalf("parent edges of %s = %v, want exactly [%s]", child, parents, wantParent)
	}
}

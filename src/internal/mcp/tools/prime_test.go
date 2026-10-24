package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// seedAssignedIssue inserts an issues row assigned to `username`, with
// the provided status. Mirrors the testhelper `seedIssue` shape, but
// stores the assignee directly so the prime tool's username-based
// filter has rows to find.
func seedAssignedIssue(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, creator uuid.UUID, username, idStr, title, status string, priority int) {
	t.Helper()
	ctx := context.Background()

	// sk-31: 'blocked' is no longer a writable status — derive it from
	// the dep graph by inserting an 'open' row + a typed-deps edge to a
	// fresh open companion blocker (also in this project for simplicity;
	// the prime test only counts via the loadAssignedIssues fan-out
	// path that passes isBlocked=true, which doesn't double-count
	// companions because they have no assignee match).
	needsBlocker := status == "blocked"
	insertStatus := status
	if needsBlocker {
		insertStatus = "open"
	}

	closedAt := "NULL"
	closedReason := "NULL"
	if insertStatus == "closed" {
		closedAt = "now()"
		closedReason = "''"
	}

	sql := `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', $4, $5, false, 'human', NULL, $6,
        '', '', '', NULL, NULL, NULL,
        NULL, ` + closedAt + `, ` + closedReason + `,
        NULL, $7, now(), now(), '{}'::jsonb)`
	_, err := pool.Exec(ctx, sql,
		idStr, projectID, title, priority, insertStatus, username, creator)
	if err != nil {
		t.Fatalf("seed assigned issue %s: %v", idStr, err)
	}

	if needsBlocker {
		// Add a companion open blocker WITHIN this project + a 'blocks'
		// edge. The blocker has no assignee, so loadAssignedIssues
		// (filters by assignee=username) doesn't pick it up — only the
		// original issue is counted as blocked.
		blockerID := idStr + "-blkr"
		_, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, 'blocker', '', 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $3, now(), now(), '{}'::jsonb)`,
			blockerID, projectID, creator)
		if err != nil {
			t.Fatalf("seed blocker for %s: %v", idStr, err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`,
			idStr, blockerID, creator); err != nil {
			t.Fatalf("seed blocks edge for %s: %v", idStr, err)
		}
	}

	if dash := strings.LastIndex(idStr, "-"); dash > 0 {
		suffix := idStr[dash+1:]
		var n int
		if _, perr := fmt.Sscanf(suffix, "%d", &n); perr == nil && n > 0 {
			_, _ = pool.Exec(ctx, `
UPDATE project_counters
   SET counter = GREATEST(counter, $2)
 WHERE project_id = $1`, projectID, n)
		}
	}
}

// seedEventForUser inserts a synthetic event row attributed to the given
// userID acting on the given issue. The events table is append-only and
// not exposed via a public Store helper for non-actor-driven seeding —
// tests reach in directly because the alternative is racing the real
// CreateIssue path which is heavier and slower.
func seedEventForUser(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, userID uuid.UUID, username, issueID, eventType string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, $2, $3, 'human', 'issue', $4, $5, '{}'::jsonb)`,
		eventType, username, userID, issueID, projectID)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

// TestPrime_HappyPath seeds 2 in-progress + 3 ready + 1 blocked + 5
// events for a user in a single project and verifies the response has
// the expected counts and the deterministic summary string.
func TestPrime_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, username := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "prm")

	// 2 in-progress
	for i := 1; i <= 2; i++ {
		seedAssignedIssue(t, pool, pid, userID, username,
			prefix+"-"+strconv.Itoa(i), "in-progress "+strconv.Itoa(i), "in_progress", 2)
	}
	// 3 open + assigned (ready, no blockers)
	for i := 3; i <= 5; i++ {
		seedAssignedIssue(t, pool, pid, userID, username,
			prefix+"-"+strconv.Itoa(i), "ready "+strconv.Itoa(i), "open", 2)
	}
	// 1 blocked
	seedAssignedIssue(t, pool, pid, userID, username,
		prefix+"-6", "blocked one", "blocked", 2)

	// 5 events on the in-progress and ready issues.
	for i := 1; i <= 5; i++ {
		seedEventForUser(t, pool, pid, userID, username,
			prefix+"-"+strconv.Itoa(i), "issue.updated")
	}

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handlePrime(pool, issuesStore, depsStore, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)

	// User block.
	user, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("user block missing")
	}
	if user["username"] != username {
		t.Errorf("user.username = %v, want %s", user["username"], username)
	}
	if user["isAdmin"] != false {
		t.Errorf("user.isAdmin = %v, want false", user["isAdmin"])
	}

	// Section counts.
	if got := len(body["myInProgress"].([]any)); got != 2 {
		t.Errorf("myInProgress count = %d, want 2", got)
	}
	if got := len(body["readyForMe"].([]any)); got != 3 {
		t.Errorf("readyForMe count = %d, want 3", got)
	}
	if got := len(body["blocked"].([]any)); got != 1 {
		t.Errorf("blocked count = %d, want 1", got)
	}
	if got := len(body["recentActivity"].([]any)); got != 5 {
		t.Errorf("recentActivity count = %d, want 5", got)
	}

	// Summary contains the counts.
	summary, _ := body["summary"].(string)
	if !strings.Contains(summary, "2 in-progress") {
		t.Errorf("summary missing in-progress count: %s", summary)
	}
	if !strings.Contains(summary, "3 ready") {
		t.Errorf("summary missing ready count: %s", summary)
	}
	if !strings.Contains(summary, "1 blocked") {
		t.Errorf("summary missing blocked count: %s", summary)
	}
}

// TestPrime_EmptyState — a user with no issues + no events sees every
// section as `[]` and a "No recent activity" summary.
func TestPrime_EmptyState(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	// Project so the user has a visibility footprint, but no issues.
	_, _ = seedProject(t, pool, userID, "prmpty")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handlePrime(pool, issuesStore, depsStore, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)

	if got := len(body["myInProgress"].([]any)); got != 0 {
		t.Errorf("myInProgress empty? got %d", got)
	}
	if got := len(body["readyForMe"].([]any)); got != 0 {
		t.Errorf("readyForMe empty? got %d", got)
	}
	if got := len(body["blocked"].([]any)); got != 0 {
		t.Errorf("blocked empty? got %d", got)
	}
	if got := len(body["recentActivity"].([]any)); got != 0 {
		t.Errorf("recentActivity empty? got %d", got)
	}

	summary, _ := body["summary"].(string)
	if !strings.Contains(summary, "0 in-progress") {
		t.Errorf("summary missing zero counts: %s", summary)
	}
	if !strings.Contains(summary, "No recent activity") {
		t.Errorf("summary missing 'No recent activity': %s", summary)
	}
}

// TestPrime_ProjectScope verifies the `project` arg narrows every
// section to one project — issues from other projects are dropped.
func TestPrime_ProjectScope(t *testing.T) {
	pool := newTestPool(t)
	userID, username := seedUser(t, pool, "user")
	pidA, prefixA := seedProject(t, pool, userID, "prma")
	pidB, prefixB := seedProject(t, pool, userID, "prmb")

	// 1 in-progress in each project.
	seedAssignedIssue(t, pool, pidA, userID, username, prefixA+"-1", "A in-progress", "in_progress", 2)
	seedAssignedIssue(t, pool, pidB, userID, username, prefixB+"-1", "B in-progress", "in_progress", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handlePrime(pool, issuesStore, depsStore, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pidA.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)

	if got := len(body["myInProgress"].([]any)); got != 1 {
		t.Fatalf("myInProgress count with project scope = %d, want 1", got)
	}
	scope, _ := body["scope"].(map[string]any)
	if scope == nil {
		t.Fatalf("scope block missing")
	}
	proj, _ := scope["project"].(map[string]any)
	if proj == nil || proj["id"] != pidA.String() {
		t.Errorf("scope.project mismatch: %v", scope)
	}
}

// TestPrime_ComponentForbidden verifies a component on a project the
// caller can't see returns ErrCodeForbidden.
func TestPrime_ComponentForbidden(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	otherUserID, _ := seedUser(t, pool, "user")
	otherPid, _ := seedProject(t, pool, otherUserID, "prmcompo")

	// Create a component owned by the other user's project.
	cid := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, 'isolated', '', '[]'::jsonb, NULL, $3, now(), now())`,
		cid, otherPid, otherUserID)
	if err != nil {
		t.Fatalf("seed component: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM components WHERE id = $1`, cid)
	})

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handlePrime(pool, issuesStore, depsStore, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"component": cid.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for forbidden component")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeForbidden {
		t.Fatalf("error code = %v, want %s", got, ErrCodeForbidden)
	}
}

// TestPrime_AlwaysLoadMetaSet verifies the registered tool carries the
// `anthropic/alwaysLoad: true` _meta marker. This is the contract Claude
// Code 2.1.121+ relies on to surface the tool upfront.
func TestPrime_AlwaysLoadMetaSet(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	// Stores are not exercised for the meta check — we only need
	// registration to land. Pool can be nil because Register* doesn't
	// call into it.
	RegisterPrime(s, nil, nil, nil, nil)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var primeTool *mcpgo.Tool
	for i := range listRes.Tools {
		if listRes.Tools[i].Name == "skein_prime" {
			primeTool = &listRes.Tools[i]
			break
		}
	}
	if primeTool == nil {
		t.Fatalf("skein_prime not registered")
	}
	if primeTool.Meta == nil {
		t.Fatalf("_meta missing on skein_prime")
	}
	v, ok := primeTool.Meta.AdditionalFields["anthropic/alwaysLoad"]
	if !ok {
		t.Fatalf("_meta missing anthropic/alwaysLoad")
	}
	if got, _ := v.(bool); !got {
		t.Fatalf("anthropic/alwaysLoad = %v, want true", v)
	}
}

// TestPrime_RecentEventsForUser_Smoke is a thin direct test on the
// store method backing recentActivity. Verifies the SQL compiles and
// scopes by visibility.
func TestPrime_RecentEventsForUser_Smoke(t *testing.T) {
	pool := newTestPool(t)
	userID, username := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "evup")

	seedAssignedIssue(t, pool, pid, userID, username, prefix+"-1", "x", "open", 2)
	seedEventForUser(t, pool, pid, userID, username, prefix+"-1", "issue.updated")

	store := issues.NewStore(pool)
	out, err := store.RecentEventsForUser(context.Background(), userID, authz.ForUser(userID), nil, 10)
	if err != nil {
		t.Fatalf("RecentEventsForUser: %v", err)
	}
	if len(out) < 1 {
		t.Fatalf("expected at least 1 event, got %d", len(out))
	}
	if out[0].IssueID != prefix+"-1" {
		t.Errorf("event.issueId = %s, want %s-1", out[0].IssueID, prefix)
	}
	if out[0].Type != "issue.updated" {
		t.Errorf("event.type = %s, want issue.updated", out[0].Type)
	}
	if out[0].OccurredAt.Location() != time.UTC {
		t.Errorf("event.at not in UTC: %v", out[0].OccurredAt.Location())
	}
}

// ---------------------------------------------------------------------
// sk-282 T018 — the three priming lists (test-plan rows 68, 69, 70).
// ---------------------------------------------------------------------

// primeRowsFixture seeds one issue into each of the three sections for a
// single caller: one in progress (claimed), one ready to take (open,
// assigned, unblocked) and one blocked.
type primeRowsFixture struct {
	userID   uuid.UUID
	username string
	project  uuid.UUID
	inProg   string
	ready    string
	blocked  string
}

func seedPrimeRowsFixture(t *testing.T, pool *pgxpool.Pool, hint string) primeRowsFixture {
	t.Helper()
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, hint)

	fx := primeRowsFixture{userID: userID, username: uname, project: pid}

	fx.inProg = seedIssue(t, pool, pid, userID, prefix+"-1", "in progress row", "in_progress", 1)
	testsupport.SetAssignee(t, pool, fx.inProg, uname)

	fx.ready = seedIssue(t, pool, pid, userID, prefix+"-2", "ready row", "open", 1)
	testsupport.SetAssignee(t, pool, fx.ready, uname)

	fx.blocked = seedIssue(t, pool, pid, userID, prefix+"-3", "blocked row", "open", 1)
	testsupport.SetAssignee(t, pool, fx.blocked, uname)
	blocker := seedIssue(t, pool, pid, userID, prefix+"-4", "the blocker", "open", 1)
	testsupport.SeedDep(t, pool, fx.blocked, blocker, dependencies.DepBlocks, userID)

	// Labels and a component on the ready issue, so the exception is
	// tested against data that really exists.
	componentID := testsupport.SeedComponent(t, pool, pid, userID, "primecomp", "")
	for _, id := range []string{fx.inProg, fx.ready} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'backend')`, id); err != nil {
			t.Fatalf("seed label: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
			id, componentID); err != nil {
			t.Fatalf("seed component: %v", err)
		}
	}
	return fx
}

func callPrime(t *testing.T, pool *pgxpool.Pool, fx primeRowsFixture) map[string]any {
	t.Helper()
	handler := handlePrime(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(withCtx(t, fx.userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

func primeSection(t *testing.T, body map[string]any, name string) []map[string]any {
	t.Helper()
	raw, ok := body[name].([]any)
	if !ok {
		t.Fatalf("prime section %q missing or not an array: %#v", name, body[name])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

// row 68 — SC-014: every row of all three sections fits SC-002's budget,
// counted against the CROSS-PROJECT bill.
func TestPrime_AllThreeSectionsFitTheCrossProjectRowBudget(t *testing.T) {
	pool := newTestPool(t)
	fx := seedPrimeRowsFixture(t, pool, "prm68")
	body := callPrime(t, pool, fx)

	exercised := 0
	for _, section := range []string{"myInProgress", "readyForMe", "blocked"} {
		rows := primeSection(t, body, section)
		if len(rows) == 0 {
			t.Errorf("prime section %q is empty — the fixture does not exercise it, and an "+
				"empty section satisfies any budget", section)
			continue
		}
		exercised++
		for _, row := range rows {
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatalf("marshal row: %v", err)
			}
			total := len([]rune(string(encoded)))
			// SC-002's budget excludes the title, and SC-014 says the
			// priming rows are billed CROSS-PROJECT, so the project
			// identifier comes out too — the same shift rule, applied to
			// the same two pieces of unbounded text.
			title, _ := row["title"].(string)
			projectID, _ := row["projectId"].(string)
			overhead := total - len([]rune(title)) - len([]rune(projectID)) - 2
			t.Logf("SC-014 %s row: %d chars total, %d overhead (budget %d)",
				section, total, overhead, sc002BudgetChars)
			if overhead > sc002BudgetChars {
				t.Errorf("%s row overhead is %d characters, SC-002's budget is %d:\n%s",
					section, overhead, sc002BudgetChars, encoded)
			}
		}
	}
	if exercised != 3 {
		t.Fatalf("only %d of the three sections carried rows", exercised)
	}
}

// row 69 — the US-12 exception: labels and components are absent AS KEYS
// in «ready to take» and PRESENT in the other two.
func TestPrime_ReadySectionOmitsLabelKeysOthersKeepThem(t *testing.T) {
	pool := newTestPool(t)
	fx := seedPrimeRowsFixture(t, pool, "prm69")
	body := callPrime(t, pool, fx)

	ready := primeSection(t, body, "readyForMe")
	if len(ready) == 0 {
		t.Fatal("readyForMe is empty — the exception cannot be tested")
	}
	for _, row := range ready {
		for _, key := range []string{"labels", "componentIds"} {
			if v, ok := row[key]; ok {
				t.Errorf("readyForMe row carries %q = %#v — this carrier does not load it, so "+
					"an empty array asserts «none» about an issue that demonstrably HAS one; "+
					"US-12 removes the key, not the value", key, v)
			}
		}
		// isBlocked, by contrast, is PRESENT and false. The carrier has no
		// such column, but the query does the work: the ready predicate is
		// `NOT EXISTS(blocks|waits-for edge whose target still gates
		// readiness)` — the negation of the issues store's isBlockedExpr,
		// same edge types, same status.GatesReadiness. So false is what
		// the query already computed, not a claim invented in the
		// projection, and US-12's exception stays exactly two names long.
		blocked, ok := row["isBlocked"]
		if !ok {
			t.Error("readyForMe row has no isBlocked — the readiness predicate is the exact " +
				"negation of the blocked predicate, so this list knows the answer; declaring " +
				"it not-provided widens the US-12 exception from two names to three and makes " +
				"an agent re-derive from the dependency graph what the query already checked")
			continue
		}
		if blocked != false {
			t.Errorf("readyForMe row has isBlocked = %#v, want false — every row here passed "+
				"the readiness predicate", blocked)
		}
	}

	// The other two sections follow the plain US-2 rule: labels ARE part
	// of the named default set, and their carrier loads them.
	inProg := primeSection(t, body, "myInProgress")
	if len(inProg) == 0 {
		t.Fatal("myInProgress is empty — the control half cannot be tested")
	}
	for _, row := range inProg {
		labels, ok := row["labels"].([]any)
		if !ok {
			t.Errorf("myInProgress row has no labels key — US-2 names labels in the default "+
				"set, and this carrier loads them: %#v", row["labels"])
			continue
		}
		if len(labels) == 0 {
			t.Error("myInProgress row has an empty labels array although the fixture put one " +
				"on the issue — the control is not controlling anything")
		}
	}
}

// row 69, second half — the counters are on all three sections, from the
// same fragment.
func TestPrime_AllSectionsCarryChildCounters(t *testing.T) {
	pool := newTestPool(t)
	fx := seedPrimeRowsFixture(t, pool, "prm69c")
	body := callPrime(t, pool, fx)

	for _, section := range []string{"myInProgress", "readyForMe", "blocked"} {
		for _, row := range primeSection(t, body, section) {
			for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
				got, ok := row[key]
				if !ok {
					t.Errorf("%s row has no %q — a zero has to read as «counted, and there are "+
						"none»", section, key)
					continue
				}
				if got != float64(0) {
					t.Errorf("%s row %q = %v, want 0 on a childless fixture", section, key, got)
				}
			}
		}
	}
}

// row 70 — the recent-activity section is untouched.
func TestPrime_RecentActivityUnchanged(t *testing.T) {
	pool := newTestPool(t)
	fx := seedPrimeRowsFixture(t, pool, "prm70")

	// Produce an event the caller is involved in, the same way the
	// package's existing activity test does — the section reads the
	// events table, not the comment itself.
	seedEventForUser(t, pool, fx.project, fx.userID, fx.username, fx.inProg, "issue.updated")

	body := callPrime(t, pool, fx)
	raw, ok := body["recentActivity"].([]any)
	if !ok {
		t.Fatalf("recentActivity missing or not an array: %#v", body["recentActivity"])
	}
	if len(raw) == 0 {
		t.Fatal("recentActivity is empty — the fixture does not exercise the section, and " +
			"«unchanged» would hold vacuously")
	}

	// Its shape is the events DTO, not a projected issue row: US-12
	// declares it Unchanged, and the T018 projection must not have
	// reached it. Comparing against the DTO's own json tags is what makes
	// «unchanged» a check rather than a hope.
	declared := map[string]bool{}
	rt := reflect.TypeOf(issues.UserEventDTO{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || name == "" {
			continue
		}
		declared[name] = true
	}
	for _, item := range raw {
		for key := range item.(map[string]any) {
			if !declared[key] {
				t.Errorf("recentActivity entry carries %q, which issues.UserEventDTO does not "+
					"declare — the row projection reached a section US-12 declares Unchanged", key)
			}
		}
	}
	for _, banned := range []string{"childrenCount", "unclosedChildrenCount", "isBlocked"} {
		for _, item := range raw {
			if v, ok := item.(map[string]any)[banned]; ok {
				t.Errorf("recentActivity entry carries %q = %v — that is an issue-row field", banned, v)
			}
		}
	}
}

// TestPrime_SectionsCoverTheWholeVisibleSet — the task
// sections of an unscoped prime span every project the caller sees, however
// many. The caller is a member of 101 projects and its tasks — one in
// progress, one ready, one blocked — sit only in the oldest of them, the
// one a cut to the 100 newest projects would leave out. Each section
// carries its task.
func TestPrime_SectionsCoverTheWholeVisibleSet(t *testing.T) {
	pool := newTestPool(t)
	userID, username := seedUser(t, pool, "user")
	oldest, prefix := seedProject(t, pool, userID, "prwide")
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET created_at = now() - interval '1 day' WHERE id = $1`, oldest); err != nil {
		t.Fatalf("backdate the oldest project: %v", err)
	}
	for i := 0; i < 100; i++ {
		seedProject(t, pool, userID, "prwide")
	}
	seedAssignedIssue(t, pool, oldest, userID, username, prefix+"-1", "wide in progress", "in_progress", 2)
	seedAssignedIssue(t, pool, oldest, userID, username, prefix+"-2", "wide ready", "open", 2)
	seedAssignedIssue(t, pool, oldest, userID, username, prefix+"-3", "wide blocked", "blocked", 2)

	handler := handlePrime(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(withCtx(t, userID, "user"), req)
	if err != nil || res.IsError {
		t.Fatalf("prime: err=%v result=%+v", err, res)
	}
	body := extractToolJSON(t, res)
	for section, want := range map[string]string{
		"myInProgress": prefix + "-1",
		"readyForMe":   prefix + "-2",
		"blocked":      prefix + "-3",
	} {
		var got []string
		for _, row := range body[section].([]any) {
			got = append(got, row.(map[string]any)["id"].(string))
		}
		if !slices.Contains(got, want) {
			t.Errorf("%s = %v, want it to carry %s from the oldest of 101 visible projects", section, got, want)
		}
	}
}

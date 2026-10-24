package tools

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// seedListIssueWithFields inserts an issues row with explicit source,
// assignee, accepted_at, and (optional) created_by override. Used by
// the Phase 2 ui-redesign filter tests where the basic seedIssue helper
// would force defaults that break the test setup. acceptedAt nil keeps
// accepted_at NULL (Inbox surface).
func seedListIssueWithFields(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, creator uuid.UUID, idStr, source, assignee string, acceptedAt *time.Time) {
	t.Helper()
	var assigneeArg any
	if assignee != "" {
		assigneeArg = assignee
	}
	var acceptedAtArg, acceptedByArg any
	if acceptedAt != nil {
		acceptedAtArg = *acceptedAt
		acceptedByArg = creator
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, 'open', false, $3, NULL, $4::varchar,
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
        NULL, $5, now(), now(), $6::timestamptz, $7::uuid, '{}'::jsonb)`,
		idStr, projectID, source, assigneeArg, creator, acceptedAtArg, acceptedByArg); err != nil {
		t.Fatalf("seed list-fields issue %s: %v", idStr, err)
	}
}

// listItemIDs extracts the items[].id slice from a parsed tool response.
func listItemIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items not an array: %T", body["items"])
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		row := raw.(map[string]any)
		out = append(out, row["id"].(string))
	}
	return out
}

func TestList_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "lst")
	for i := 1; i <= 3; i++ {
		seedIssue(t, pool, pid, userID, prefix+"-"+strconv.Itoa(i), "issue "+strconv.Itoa(i), "open", 2)
	}

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
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
	if len(items) < 3 {
		t.Fatalf("expected at least 3 items, got %d", len(items))
	}
}

func TestList_ProjectNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": "definitely-not-a-project-12345"}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing project")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error code = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

func TestList_ValidationRejectsBadStatus(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "lst")
	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"statuses": []any{"banana"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for invalid status")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

func TestList_ForbiddenForNonMember(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "lst")
	seedIssue(t, pool, pid, owner, prefix+"-1", "x", "open", 2)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pid.String()}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-member")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error code = %v, want %s (no leak via forbidden)", got, ErrCodeProjectNotFound)
	}
}

// ----------------- Phase 2 ui-redesign — B5 filter tests -----------------

// TestList_AssigneeMe_AcceptedAtNull mirrors the REST test: the headline
// Inbox query (assignee=me + acceptedAt=null + status=open) returns
// only the unaccepted, open, assigned-to-caller issues.
func TestList_AssigneeMe_AcceptedAtNull(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "lmn")

	now := time.Now().UTC()
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-1", "human", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-2", "human", ownerName, &now)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":    pid.String(),
		"assignee":   "me",
		"acceptedAt": "null",
		"statuses":   []any{"open"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	ids := listItemIDs(t, body)
	if len(ids) != 1 || ids[0] != prefix+"-1" {
		t.Fatalf("ids = %v, want [%s-1]", ids, prefix)
	}
}

// TestList_CreatedByMe_Outbox: createdBy=me restricts to caller-authored.
func TestList_CreatedByMe_Outbox(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "lcb")
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`,
		pid, other, owner); err != nil {
		t.Fatalf("seed other member: %v", err)
	}
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-1", "human", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, other, prefix+"-2", "human", ownerName, nil)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":   pid.String(),
		"createdBy": "me",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	ids := listItemIDs(t, body)
	if len(ids) != 1 || ids[0] != prefix+"-1" {
		t.Fatalf("ids = %v, want [%s-1]", ids, prefix)
	}
}

// TestList_OriginHumanDriven_ExcludesAutonomous: origin=human-driven
// returns rows with source IN ('human','agent_directed') only.
func TestList_OriginHumanDriven_ExcludesAutonomous(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "loh")

	seedListIssueWithFields(t, pool, pid, owner, prefix+"-1", "human", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-2", "agent_directed", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-3", "agent_autonomous", ownerName, nil)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"origin":  "human-driven",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	ids := listItemIDs(t, body)
	want := map[string]bool{prefix + "-1": true, prefix + "-2": true}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 rows (-1 and -2)", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %s in human-driven page", id)
		}
	}
}

// TestList_OriginAgentAutonomous_OnlyAutonomous: only agent_autonomous.
func TestList_OriginAgentAutonomous_OnlyAutonomous(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "loa")
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-1", "human", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-2", "agent_autonomous", ownerName, nil)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"origin":  "agent-autonomous",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	ids := listItemIDs(t, body)
	if len(ids) != 1 || ids[0] != prefix+"-2" {
		t.Fatalf("ids = %v, want [%s-2]", ids, prefix)
	}
}

// TestList_IAssigned_Computed verifies the iAssigned summary flag.
func TestList_IAssigned_Computed(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	other, otherName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "lia")
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`,
		pid, other, owner); err != nil {
		t.Fatalf("seed other member: %v", err)
	}

	// 1. caller created + delegated → iAssigned=true
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-1", "human", otherName, nil)
	// 2. caller created + self-assigned → iAssigned=false
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-2", "human", ownerName, nil)
	// 3. other created + assigned to caller → iAssigned=false
	seedListIssueWithFields(t, pool, pid, other, prefix+"-3", "human", ownerName, nil)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	// sk-282 T014: `iAssigned` is named in US-2's "Not included" list, so the
	// DEFAULT row no longer carries it. The derivation is unchanged and
	// the field is still reachable — through `fields`, which is the INV-4
	// exit from the narrowed default. Asking for it explicitly is what
	// keeps this test about the derivation rather than about the default
	// set, and it proves the exit works on a field the default omits.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"fields":  []any{"iAssigned"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	items, _ := body["items"].([]any)
	got := map[string]bool{}
	for _, raw := range items {
		row := raw.(map[string]any)
		flag, _ := row["iAssigned"].(bool)
		got[row["id"].(string)] = flag
	}

	// …and the default row does NOT carry it, which is the half that
	// would otherwise silently regress.
	defReq := mcpgo.CallToolRequest{}
	defReq.Params.Arguments = map[string]any{"project": pid.String()}
	defRes, err := handler(ctx, defReq)
	if err != nil {
		t.Fatalf("handler error (default): %v", err)
	}
	for _, raw := range extractToolJSON(t, defRes)["items"].([]any) {
		if v, ok := raw.(map[string]any)["iAssigned"]; ok {
			t.Errorf("the default row carries iAssigned = %v — US-2 names it among the fields "+
				"that do not enter the row", v)
		}
	}
	if got[prefix+"-1"] != true {
		t.Fatalf("issue 1 iAssigned=%v, want true", got[prefix+"-1"])
	}
	if got[prefix+"-2"] != false {
		t.Fatalf("issue 2 iAssigned=%v, want false", got[prefix+"-2"])
	}
	if got[prefix+"-3"] != false {
		t.Fatalf("issue 3 iAssigned=%v, want false", got[prefix+"-3"])
	}
}

// TestList_StatusesStandingFilter seeds one standing issue and one
// non-standing (open) issue; skein_list(statuses=["standing"]) must pass
// the wire-level enum schema validation and return ONLY the standing one.
func TestList_StatusesStandingFilter(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "lstand")
	seedIssue(t, pool, pid, userID, prefix+"-1", "perpetual", "standing", 2)
	seedIssue(t, pool, pid, userID, prefix+"-2", "ordinary", "open", 2)

	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"statuses": []any{"standing"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true (statuses=standing should validate): %+v",
			extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	ids := listItemIDs(t, body)
	if len(ids) != 1 || ids[0] != prefix+"-1" {
		t.Fatalf("ids = %v, want [%s-1] (only the standing issue)", ids, prefix)
	}
}

// TestList_InvalidAcceptedAt_ValidationError: only "null" allowed.
func TestList_InvalidAcceptedAt_ValidationError(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "lia2")
	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":    pid.String(),
		"acceptedAt": "foo",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bad acceptedAt")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
}

// TestList_InvalidOrigin_ValidationError: bad origin token rejected.
func TestList_InvalidOrigin_ValidationError(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "lor")
	store := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleList(pool, store, pStore, nil)
	ctx := withCtx(t, owner, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"origin":  "banana",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bad origin")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
}

// ---------------------------------------------------------------------
// sk-282 T014 — the thin list row (test-plan rows 55, 56, 57, 58).
// Row 59 is the REST side and lives with the REST handler.
// ---------------------------------------------------------------------

// seedListPage hangs n issues in one project and returns (projectID,
// prefix, owner).
func seedListPage(t *testing.T, pool *pgxpool.Pool, hint string, n int) (uuid.UUID, string, uuid.UUID) {
	t.Helper()
	owner, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, owner, hint)
	for i := 1; i <= n; i++ {
		seedIssue(t, pool, pid, owner, fmt.Sprintf("%s-%d", prefix, i),
			fmt.Sprintf("row %03d", i), "open", 2)
	}
	return pid, prefix, owner
}

func callListTool(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, args map[string]any) map[string]any {
	t.Helper()
	handler := handleList(pool, issues.NewStore(pool), projects.NewStore(pool), nil)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(withCtx(t, owner, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// row 55 — SC-003: no page size named → exactly the default, and an
// honest «there is more».
//
// The expectation is the LITERAL 25, not ListLimitDefault. Written against
// the constant, the test moved with it: changing the constant to 7 changed
// both the fixture and the expectation, the assertion compared the value
// with itself and the package stayed green while SC-003 ("exactly 25")
// silently stopped holding. A criterion that names a number is checked
// against that number.
func TestList_DefaultPageIsTwentyFiveWithHonestHasMore(t *testing.T) {
	const sc003DefaultPage = 25

	pool := newTestPool(t)
	pid, _, owner := seedListPage(t, pool, "lst55", sc003DefaultPage+7)

	body := callListTool(t, pool, owner, map[string]any{"project": pid.String()})

	items := body["items"].([]any)
	if len(items) != sc003DefaultPage {
		t.Errorf("default page returned %d rows, want exactly %d — SC-003 is stated on this "+
			"call, and before T014 it returned up to five hundred", len(items), sc003DefaultPage)
	}
	if got, ok := body["hasMore"].(bool); !ok || !got {
		t.Errorf("hasMore = %v, want true — the header has to be honest about the rows it did "+
			"not send, or paging is guesswork", body["hasMore"])
	}
	if _, ok := body["nextCursor"].(string); !ok {
		t.Errorf("nextCursor missing on a truncated page: %#v", body["nextCursor"])
	}
}

// row 56 — a request above the ceiling is clamped to it, and the ceiling
// is the one the schema declares (the INV-5 guard checks the other half).
func TestList_AboveCeilingIsClampedToTheDeclaredCeiling(t *testing.T) {
	// The literal, for the same reason row 55 uses one: written against
	// ListLimitMax the assertion compares the constant with itself.
	const declaredCeiling = 100

	pool := newTestPool(t)
	pid, _, owner := seedListPage(t, pool, "lst56", declaredCeiling+5)

	body := callListTool(t, pool, owner, map[string]any{
		"project": pid.String(),
		"limit":   500,
	})
	items := body["items"].([]any)
	if len(items) != declaredCeiling {
		t.Errorf("limit=500 returned %d rows, want the ceiling of %d — the ceiling used to be "+
			"declared and not applied, which is the INV-5 lie T014 closes",
			len(items), declaredCeiling)
	}
}

// row 57 — total and hasMore describe the QUERY, not the projection.
func TestList_TotalAndHasMoreDoNotDependOnFields(t *testing.T) {
	pool := newTestPool(t)
	pid, _, owner := seedListPage(t, pool, "lst57", ListLimitDefault+3)

	full := callListTool(t, pool, owner, map[string]any{"project": pid.String()})
	narrow := callListTool(t, pool, owner, map[string]any{
		"project": pid.String(),
		"fields":  []any{"status"},
	})
	empty := callListTool(t, pool, owner, map[string]any{
		"project": pid.String(),
		"fields":  []any{},
	})

	for _, name := range []string{"total", "hasMore"} {
		if full[name] != narrow[name] || full[name] != empty[name] {
			t.Errorf("%s differs by field selection: default=%v narrow=%v empty=%v — a caller "+
				"narrowing fields to save bytes must still be able to page, and a total that "+
				"moved with the selection would answer a different question",
				name, full[name], narrow[name], empty[name])
		}
	}
	// And the narrowing really happened, or the equality above is trivial.
	narrowRow := narrow["items"].([]any)[0].(map[string]any)
	if len(narrowRow) != 2 {
		t.Errorf("narrow row has %d keys (%v), want id + status", len(narrowRow), narrowRow)
	}
	emptyRow := empty["items"].([]any)[0].(map[string]any)
	if len(emptyRow) != 1 {
		t.Errorf("empty selection returned %d keys (%v), want the identifier alone",
			len(emptyRow), emptyRow)
	}
}

// row 57, negative half — an unknown field name is refused by name.
func TestList_UnknownFieldIsRefusedByName(t *testing.T) {
	pool := newTestPool(t)
	pid, _, owner := seedListPage(t, pool, "lst57n", 2)

	handler := handleList(pool, issues.NewStore(pool), projects.NewStore(pool), nil)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"fields":  []any{"stauts"},
	}
	res, err := handler(withCtx(t, owner, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a misspelled field was accepted: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Errorf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
	if body["unknownField"] != "stauts" {
		t.Errorf("the refusal does not name the offending field: %#v", body)
	}
}

// row 57, negative half on the input the first one cannot reach — an
// unknown field name is refused when the page is EMPTY.
//
// The refusal used to be a by-product of projecting a row: validation ran
// inside the per-row loop, so a page with no rows validated nothing and
// `fields: ["totallyBogusFieldName"]` came back isError=false with an
// empty page. US-2 forbids exactly that outcome — a silently ignored name
// reads as «the field exists and there is nothing under it» — and an empty
// page is not an edge: it is every filter that found nothing. Both list
// paths are checked, because both projected through the same loop.
func TestList_UnknownFieldIsRefusedOnAnEmptyPage(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	// A project with no issues at all: the page is empty for a reason the
	// caller cannot influence with a filter.
	pid, _ := seedProject(t, pool, owner, "lst57e")

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "project-scoped",
			args: map[string]any{
				"project": pid.String(),
				"fields":  []any{"totallyBogusFieldName"},
			},
		},
		{
			name: "cross-project",
			args: map[string]any{
				"crossProject": true,
				"fields":       []any{"totallyBogusFieldName"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := handleList(pool, issues.NewStore(pool), projects.NewStore(pool), nil)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args
			res, err := handler(withCtx(t, owner, "user"), req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			body := extractToolJSON(t, res)
			if !res.IsError {
				t.Fatalf("an unknown field was accepted because the page happened to be "+
					"empty: %+v", body)
			}
			if body["error"] != ErrCodeValidationError {
				t.Errorf("error = %v, want %s", body["error"], ErrCodeValidationError)
			}
			if body["unknownField"] != "totallyBogusFieldName" {
				t.Errorf("the refusal does not name the offending field: %#v", body)
			}
		})
	}
}

// row 58 — cross-project mode adds the project identifier, project mode
// does not.
func TestList_CrossProjectRowCarriesProjectID(t *testing.T) {
	pool := newTestPool(t)
	pid, _, owner := seedListPage(t, pool, "lst58", 3)

	project := callListTool(t, pool, owner, map[string]any{"project": pid.String()})
	if _, ok := project["items"].([]any)[0].(map[string]any)["projectId"]; ok {
		t.Error("project-mode row carries projectId — US-2 names it among the fields that do " +
			"not enter the row in project mode; every row would repeat the same value")
	}

	cross := callListTool(t, pool, owner, map[string]any{"crossProject": true})
	items := cross["items"].([]any)
	if len(items) == 0 {
		t.Fatal("cross-project list returned nothing — the fixture does not exercise this mode")
	}
	found := false
	for _, raw := range items {
		row := raw.(map[string]any)
		if row["projectId"] == pid.String() {
			found = true
		}
		if _, ok := row["projectId"]; !ok {
			t.Errorf("cross-project row %v has no projectId — without it the listing is unreadable", row["id"])
		}
	}
	if !found {
		t.Error("none of the cross-project rows carries the fixture's project id")
	}
}

// row 55, second half — the US-3 counters are on the row, at zero too.
func TestList_RowCarriesChildCountersIncludingZero(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, owner, "lst55c")
	parent := seedIssue(t, pool, pid, owner, prefix+"-1", "has children", "open", 2)
	childless := seedIssue(t, pool, pid, owner, prefix+"-2", "no children", "open", 2)
	for i, st := range []string{"open", "closed", "standing"} {
		child := seedIssue(t, pool, pid, owner,
			fmt.Sprintf("%s-1.%d", prefix, i+1), "child", st, 2)
		testsupport.SeedDep(t, pool, child, parent, dependencies.DepParentChild, owner)
	}

	body := callListTool(t, pool, owner, map[string]any{"project": pid.String()})
	rows := map[string]map[string]any{}
	for _, raw := range body["items"].([]any) {
		row := raw.(map[string]any)
		rows[row["id"].(string)] = row
	}

	if got := rows[parent]["childrenCount"]; got != float64(2) {
		t.Errorf("childrenCount = %v, want 2 (the standing child is not work)", got)
	}
	if got := rows[parent]["unclosedChildrenCount"]; got != float64(1) {
		t.Errorf("unclosedChildrenCount = %v, want 1", got)
	}
	for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
		got, present := rows[childless][key]
		if !present {
			t.Errorf("%s absent on a childless row — a zero has to read as «counted, and there "+
				"are none»", key)
			continue
		}
		if got != float64(0) {
			t.Errorf("%s = %v on a childless row, want 0", key, got)
		}
	}
}

// TestList_ComponentIdsAreReachableOnlyWhenNamed is the gate under the
// read-back route three descriptions name.
//
// skein_ready and the agent guide both tell an agent where
// to go for the `componentIds` a ready row does not carry. They used to
// send it to skein_list, full stop — and the default row does not carry
// the key either, so the agent arrived at a second absence with no
// explanation. The route is only true with the field NAMED, and a route
// that is true only in one of its two spellings has to be pinned to the
// spelling that works.
//
// So this checks both halves on one fixture whose issue really HAS a
// component: the default row does not carry the key (or the descriptions
// would be over-promising), and `fields: ["componentIds"]` returns it with
// the seeded value (or the route they now name is fiction).
func TestList_ComponentIdsAreReachableOnlyWhenNamed(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, owner, "lstcid")
	issueID := seedIssue(t, pool, pid, owner, prefix+"-1", "has a component", "open", 2)
	componentID := testsupport.SeedComponent(t, pool, pid, owner, "lstcidcomp", "")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
		issueID, componentID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	def := callListTool(t, pool, owner, map[string]any{"project": pid.String()})
	defRow := def["items"].([]any)[0].(map[string]any)
	if v, present := defRow["componentIds"]; present {
		t.Errorf("the default row carries componentIds = %#v — the descriptions say the key "+
			"is outside the named set and has to be asked for; if that changed, they are the "+
			"ones to change", v)
	}
	// `labels` is the asymmetry the descriptions now spell out: it IS in
	// the default row, which is why «read them with skein_list» was half
	// true and therefore misleading.
	if _, present := defRow["labels"]; !present {
		t.Error("the default row has no labels key — the descriptions name skein_list as the " +
			"labels route precisely because it is in the default set")
	}

	named := callListTool(t, pool, owner, map[string]any{
		"project": pid.String(),
		"fields":  []any{"componentIds"},
	})
	namedRow := named["items"].([]any)[0].(map[string]any)
	raw, present := namedRow["componentIds"]
	if !present {
		t.Fatalf("fields:[\"componentIds\"] returned a row without the key (%#v) — this is the "+
			"route skein_ready and the guide send an agent down for a field a "+
			"ready row does not carry; if it does not answer, they name a dead end", namedRow)
	}
	got, ok := raw.([]any)
	if !ok || len(got) != 1 || got[0] != componentID.String() {
		t.Errorf("componentIds = %#v, want [%s] — an empty or absent value here is the same "+
			"dead end, one step later", raw, componentID)
	}
}

// sk-674.18 T032 — `space` in cross-project mode narrows to the issues of
// that space's projects, the same filter as REST /issues?space= (SC-002);
// with a project handle it is refused, the project already names its space.
//
// The reference is the fixture legend: uAB sees A.space and B.space; an
// issue's space is the space of its project.
func TestList_CrossProjectSpaceFilter(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := testsupport.SeedTenancy(t, pool)
	both := f.User(t, testsupport.ActorBoth)
	shortOf := func(id uuid.UUID) string {
		var short string
		if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&short); err != nil {
			t.Fatalf("short id of space %s: %v", id, err)
		}
		return short
	}
	shortA := shortOf(f.SpaceA)
	call := func(args map[string]any) *mcpgo.CallToolResult {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handleList(pool, issues.NewStore(pool), projects.NewStore(pool), nil)(withCtx(t, both.ID, both.Role), req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		return res
	}

	res := call(map[string]any{"crossProject": true, "space": shortA, "limit": 100,
		"fields": []any{"id", "spaceId"}})
	if res.IsError {
		t.Fatalf("crossProject + space: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	ids := map[string]bool{}
	for _, raw := range body["items"].([]any) {
		row := raw.(map[string]any)
		ids[row["id"].(string)] = true
		if row["spaceId"] != shortA {
			t.Errorf("space=%s lists %v of space %v", shortA, row["id"], row["spaceId"])
		}
	}
	if !ids[f.ASpace.IssueID] {
		t.Errorf("space=%s misses %s of A.space", shortA, f.ASpace.IssueID)
	}
	if ids[f.BSpace.IssueID] {
		t.Errorf("space=%s lists %s of B.space", shortA, f.BSpace.IssueID)
	}

	refused := call(map[string]any{"project": f.ASpace.ID.String(), "space": shortA})
	if !refused.IsError {
		t.Fatalf("project + space accepted: %+v", extractToolJSON(t, refused))
	}
	if code := extractToolJSON(t, refused)["error"]; code != ErrCodeValidationError {
		t.Errorf("project + space: error %v, want %s", code, ErrCodeValidationError)
	}
}

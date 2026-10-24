package handler

// issues_list_rest_contract_test.go — sk-282 T013/T014, test-plan row 59.
//
// "INV-2: REST issue lists — key set, their JSON types,
// representative values AND THE PREVIOUS PAGE SIZE. BOTH REST paths
// that serialise an issue row are covered: the project list and the
// cross-project one. … the cross-project REST path today has not a
// single test — a clamp or side-channel leak there is not caught by a
// row about the project list."
//
// Two vectors, both real here:
//
//   - T013 hangs a child-counter side channel off the same store methods
//     these endpoints call. A counter that leaked onto the shared row type
//     would appear here as an additive key, which INV-2 forbids outright.
//   - T014 drops the MCP page size to 25/100. The clamp is applied in the
//     MCP handler precisely so REST keeps its own 500 — but "precisely so"
//     is a claim, and this is where it is checked.
//
// The key set is EXACT, not an upper bound: an allowlist passes when a
// field is REMOVED, and removal breaks the web UI exactly as an addition
// breaks a parser. That lesson comes from review.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// restIssueRowKeys is the REST list row's EXACT field set, with JSON
// types. Written by hand — a test that derived it from issues.IssueSummary
// would agree with any change to that struct.
var restIssueRowKeys = map[string]string{
	"id": "string", "projectId": "string", "version": "number",
	"title": "string", "type": "string", "priority": "number",
	"status": "string", "pinned": "bool", "createdBy": "string",
	"componentIds": "array", "labels": "array",
	"updatedAt": "string", "createdAt": "string", "isBlocked": "bool",
}

// restIssueRowOptionalKeys carry omitempty and appear only on rows that
// have the value.
var restIssueRowOptionalKeys = map[string]string{
	"parentIdCurrent": "string", "assignee": "string", "assigneeUserId": "string",
	"createdByUsername": "string", "acceptedAt": "string",
	"iAssigned": "bool", "epicProgress": "object",

	// sk-489 T001 (US-1, INV-1, INV-3) — extending the key set as a DELIBERATE
	// ACT: the row gets the close outcome, with the same key and type as the
	// card. Optional by meaning — INV-1: only a closed row has an outcome,
	// any other row has no such key at all.
	"closeOutcome": "string",

	// sk-622.21 T026 (US-15, K16) — extending the key set as a DELIBERATE ACT:
	// the row gets a freshness order, by which the phase 2 frontend decides
	// whether to substitute the row from an event (plan, T026: "rowSeq in all
	// REST issue rows and the card"). Optional by tag (omitempty,
	// T009 contract: zero means "order unknown"); a row from issues
	// always has it — the column is NOT NULL since migration 000064.
	"rowSeq": "number",

	// sk-674.18 T032 (US-3, US-13) — extending the key set as a DELIBERATE ACT:
	// short id of the space of the issue's project (not the id prefix). Optional by
	// tag (omitempty); an issue from issues always has it — projects.space_id
	// is NOT NULL since migration 000080.
	"spaceId": "string",
}

// assertRESTIssueRow checks one row against the exact key set.
func assertRESTIssueRow(t *testing.T, where string, row map[string]any) {
	t.Helper()
	for key, value := range row {
		wantKind, known := restIssueRowKeys[key]
		if !known {
			wantKind, known = restIssueRowOptionalKeys[key]
		}
		if !known {
			t.Errorf("%s: REST row carries an unclassified key %q = %#v — INV-2 covers "+
				"additive keys, and the child-counter side channel is the live "+
				"vector for one", where, key, value)
			continue
		}
		assertJSONKind(t, where, key, value, wantKind)
	}
	for key := range restIssueRowKeys {
		if _, ok := row[key]; !ok {
			t.Errorf("%s: REST row is MISSING %q — the set is exact, not an upper bound: "+
				"an allowlist passes on a removal, and a removal breaks the web UI just as "+
				"an addition breaks a parser", where, key)
		}
	}
	for _, banned := range []string{"childrenCount", "unclosedChildrenCount"} {
		if v, ok := row[banned]; ok {
			t.Errorf("%s: REST row carries %q = %v — the US-3 counters are a side value the "+
				"MCP path asks for; reaching REST means they were hung on the shared type",
				where, banned, v)
		}
	}
}

// TestIssuesListREST_ProjectPathUnchanged pins the project-scoped list.
func TestIssuesListREST_ProjectPathUnchanged(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restlist")

	// More rows than the MCP page size, so a clamp that leaked here would
	// be visible as a short page.
	const rows = 40
	for i := 1; i <= rows; i++ {
		id := testsupport.SeedIssue(t, pool, pid, owner,
			fmt.Sprintf("%s-%d", prefix, i), fmt.Sprintf("row %02d", i), "open", 2)
		if i == 1 {
			testsupport.SetAssignee(t, pool, id, ownerName)
			child := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1.1", "child", "open", 2)
			testsupport.SeedDep(t, pool, child, id, dependencies.DepParentChild, owner)
		}
	}

	body := callRESTIssueList(t, pool, owner, "user", pid, "")
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %#v", body["items"])
	}
	// The fixture holds `rows` top-level issues plus one child, and the
	// project list returns them all — the number is asserted as «at least
	// rows» rather than exactly, because the child is a legitimate row and
	// pinning the total here would make this test about the fixture. What
	// matters is that the page is far larger than the MCP page size: a
	// clamp leaking from T014 would show up as a short page.
	if len(items) < rows {
		t.Errorf("REST project list returned %d rows, expected at least %d — its page size is "+
			"its own and INV-2 freezes it; the MCP page size is applied in the MCP handler "+
			"precisely so it cannot reach here", len(items), rows)
	}
	for _, raw := range items {
		assertRESTIssueRow(t, "project list", raw.(map[string]any))
	}
	// Representative values on the row that has them.
	for _, raw := range items {
		row := raw.(map[string]any)
		if row["id"] != prefix+"-1" {
			continue
		}
		if row["assignee"] != ownerName {
			t.Errorf("assignee = %v, want %q", row["assignee"], ownerName)
		}
		if row["status"] != "open" || row["type"] != "task" {
			t.Errorf("status/type = %v/%v, want open/task", row["status"], row["type"])
		}
	}
	// The envelope, too.
	for key, want := range map[string]string{"items": "array", "total": "number", "hasMore": "bool"} {
		if got := jsonKind(body[key]); got != want {
			t.Errorf("REST list envelope key %q is %s, want %s", key, got, want)
		}
	}
	for key := range body {
		if key != "items" && key != "total" && key != "hasMore" && key != "nextCursor" {
			t.Errorf("REST list envelope carries an unclassified key %q", key)
		}
	}
}

// TestIssuesListREST_CrossProjectPathUnchanged pins the cross-project
// list, which had no test at all before this row.
func TestIssuesListREST_CrossProjectPathUnchanged(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restall")
	const rows = 40
	for i := 1; i <= rows; i++ {
		testsupport.SeedIssue(t, pool, pid, owner,
			fmt.Sprintf("%s-%d", prefix, i), fmt.Sprintf("row %02d", i), "open", 2)
	}

	h := NewIssuesAllHandler(pool, issues.NewStore(pool), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	ctx := auth.WithUserID(req.Context(), owner)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /issues = %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %#v", body["items"])
	}
	if len(items) < rows {
		t.Errorf("cross-project REST list returned %d rows of at least %d — its own page size "+
			"must be unchanged", len(items), rows)
	}
	mine := 0
	for _, raw := range items {
		row := raw.(map[string]any)
		if row["projectId"] == pid.String() {
			mine++
		}
		assertRESTIssueRow(t, "cross-project list", row)
	}
	if mine != rows {
		t.Errorf("only %d of the fixture's %d rows came back", mine, rows)
	}
}

// callRESTIssueList invokes the project-scoped list handler.
func callRESTIssueList(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, role string, projectID uuid.UUID, query string) map[string]any {
	t.Helper()
	h := NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool))
	url := "/api/v1/projects/" + projectID.String() + "/issues"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, caller)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.List(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET issues = %d: %v", res.StatusCode, problem)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

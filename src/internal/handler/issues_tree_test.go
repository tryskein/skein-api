package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// sk-36 handler-level tests for /projects/{pid}/issues/tree and the
// cross-project /issues/tree. All tests are DB-backed and skip when
// TEST_DATABASE_URL is unset (mirrors the listTestEnv convention).

// treeSeedDep inserts a typed-deps row directly — bypasses the
// dependencies.Store on purpose (tests own their fixtures).
func treeSeedDep(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string, creator uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`,
		fromID, toID, depType, creator)
	if err != nil {
		t.Fatalf("seed dep %s -%s-> %s: %v", fromID, depType, toID, err)
	}
}

// callProjectTree builds a GET on /projects/{pid}/issues/tree, attaches
// the chi RouteContext, auth ctx, and invokes the Tree handler.
func callProjectTree(t *testing.T, env listTestEnv, userID uuid.UUID, role, query string) (*http.Response, map[string]any) {
	t.Helper()
	url := "/api/v1/projects/" + env.projectID.String() + "/issues/tree"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", env.projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	if userID != uuid.Nil {
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, role)
	}
	// As the production router does (authz.RequestCache).
	ctx = authz.WithRequestCache(ctx)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	env.handler.Tree(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// treeItems unwraps body["items"] into a typed slice of {id, depth,
// edgeType, isReference, firstSeenAt} so tests can assert on the wire
// shape without rebuilding the parser per test.
type treeRow struct {
	ID          string
	Depth       int
	ParentID    string
	EdgeType    string
	IsReference bool
	FirstSeenAt string
	Title       string
}

func parseTreeRows(t *testing.T, body map[string]any) []treeRow {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing or wrong type: %T (body=%v)", body["items"], body)
	}
	out := make([]treeRow, 0, len(items))
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item not an object: %T", raw)
		}
		item, _ := row["item"].(map[string]any)
		r := treeRow{
			ID:    asString(item["id"]),
			Title: asString(item["title"]),
		}
		if d, ok := row["depth"].(float64); ok {
			r.Depth = int(d)
		}
		if ref, ok := row["isReference"].(bool); ok {
			r.IsReference = ref
		}
		if fsa, ok := row["firstSeenAt"].(string); ok {
			r.FirstSeenAt = fsa
		}
		if pr, ok := row["parentRef"].(map[string]any); ok {
			r.ParentID = asString(pr["id"])
			r.EdgeType = asString(pr["edgeType"])
		}
		out = append(out, r)
	}
	return out
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestTree_EmptyTypes_DefaultsToParentChild verifies that an empty
// types[] param defaults to parent-child only.
func TestTree_EmptyTypes_DefaultsToParentChild(t *testing.T) {
	env := newListEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"human", env.ownerName, nil)
	// from=child, to=parent (parent-child convention)
	treeSeedDep(t, env.pool, env.prefix+"-2", env.prefix+"-1", "parent-child", env.owner)

	res, body := callProjectTree(t, env, env.owner, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	rows := parseTreeRows(t, body)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2; %+v", len(rows), rows)
	}
	// Pre-order: -1 (depth 0) then -2 (depth 1).
	if rows[0].ID != env.prefix+"-1" || rows[0].Depth != 0 {
		t.Fatalf("row[0] = {%s, depth=%d}, want {%s-1, depth=0}", rows[0].ID, rows[0].Depth, env.prefix)
	}
	if rows[1].ID != env.prefix+"-2" || rows[1].Depth != 1 || rows[1].EdgeType != "parent-child" {
		t.Fatalf("row[1] = %+v, want -2 depth=1 edgeType=parent-child", rows[1])
	}
}

// TestTree_UnknownType_400 verifies invalid types[] surface a 400.
func TestTree_UnknownType_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callProjectTree(t, env, env.owner, "user", "types=garbage")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, body)
	}
	if code, _ := body["error"].(string); code != "invalid_dep_type" {
		t.Fatalf("error = %v, want invalid_dep_type", body["error"])
	}
}

// TestTree_Cycle_BackEdgeSuppressed: A is the parent-child parent of B,
// and B also carries a `blocks` edge whose normalised walk direction
// points B→A. Together these form a 2-cycle in the normalised edge graph
// (parent-child walks A→B; blocks walks B→A). The recursive CTE's
// path-based cycle guard (`NOT (e.next_id = ANY(w.path))`) drops the B→A
// hop because A is already B's ancestor in the walk path — so the
// back-edge is SUPPRESSED, not rendered as a reference. A appears exactly
// once (canonical, depth 0); B appears once (depth 1). A reference row is
// only emitted when a node is re-reached via a DIFFERENT, non-ancestor
// path; a cycle back to an ancestor is removed outright. Total = 2 rows.
func TestTree_Cycle_BackEdgeSuppressed(t *testing.T) {
	env := newListEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"human", env.ownerName, nil)
	// B is child of A (parent-child: from=child)
	treeSeedDep(t, env.pool, env.prefix+"-2", env.prefix+"-1", "parent-child", env.owner)
	// B blocks A (blocks: from=blocked, to=blocker — so B is blocked by A)
	treeSeedDep(t, env.pool, env.prefix+"-2", env.prefix+"-1", "blocks", env.owner)

	res, body := callProjectTree(t, env, env.owner, "user",
		"types=parent-child&types=blocks")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	rows := parseTreeRows(t, body)

	// Expect exactly:
	//   A (canonical, depth 0)
	//   B (parent-child child of A, depth 1)
	// The B→A blocks back-edge closes a cycle and is suppressed.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2; got=%+v", len(rows), rows)
	}
	var canonA, bRow *treeRow
	for i := range rows {
		switch rows[i].ID {
		case env.prefix + "-1":
			canonA = &rows[i]
		case env.prefix + "-2":
			bRow = &rows[i]
		}
	}
	if canonA == nil {
		t.Fatalf("missing canonical A row; rows=%+v", rows)
	}
	if canonA.IsReference || canonA.Depth != 0 {
		t.Fatalf("A row = %+v, want canonical depth 0", canonA)
	}
	if bRow == nil {
		t.Fatalf("missing B row; rows=%+v", rows)
	}
	if bRow.Depth != 1 || bRow.EdgeType != "parent-child" || bRow.ParentID != env.prefix+"-1" {
		t.Fatalf("B row = %+v, want depth 1, parent-child under A", bRow)
	}
	// No reference row for A — the cycle back-edge was dropped.
	for i := range rows {
		if rows[i].IsReference {
			t.Fatalf("unexpected reference row %+v; back-edge should be suppressed", rows[i])
		}
	}
}

// TestTree_TooLarge_422 verifies the node cap trips ErrTreeTooLarge.
// Drives the store directly so MaxNodes can be set below the chain
// length (the handler always uses the default 1000 cap). A non-admin
// store call MUST carry VisibleProjects — the store short-circuits to an
// empty forest otherwise (same contract the handler satisfies by passing
// the single authorized project), and an empty forest would never reach
// the node-cap check.
func TestTree_TooLarge_422(t *testing.T) {
	env := newListEnv(t)
	// Seed a 5-node parent-child chain: -1 (root) → -2 → -3 → -4 → -5.
	for i := 1; i <= 5; i++ {
		seedListIssue(t, env.pool, env.projectID, env.owner,
			env.prefix+"-"+intToStr(i), "human", env.ownerName, nil)
	}
	// Chain them all parent→child (from=child, to=parent) so the walk
	// descends -1 → -2 → -3 → -4 → -5 and emits 5 nodes.
	for i := 1; i <= 4; i++ {
		treeSeedDep(t, env.pool, env.prefix+"-"+intToStr(i+1),
			env.prefix+"-"+intToStr(i), "parent-child", env.owner)
	}
	store := issues.NewStore(env.pool)
	_, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:     authz.ForUser(env.owner),
		Caller:    env.owner,
		ProjectID: &env.projectID,
		Types:     []string{dependencies.DepParentChild},
		MaxNodes:  2, // smaller than the 5-node chain → should trip
	})
	if err == nil {
		t.Fatalf("expected ErrTreeTooLarge; got nil")
	}
	if _, ok := err.(*issues.ErrTreeTooLarge); !ok {
		t.Fatalf("got %T %v; want *issues.ErrTreeTooLarge", err, err)
	}
}

// TestTree_AdminSeesAll: admin caller bypasses VisibleProjects, sees
// rows in every project.
func TestTree_AdminSeesAll(t *testing.T) {
	env := newListEnv(t)
	admin, _ := seedListUser(t, env.pool, "admin")
	// Issue exists only in env.projectID, but admin still gets it.
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	res, body := callProjectTree(t, env, admin, "admin", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	rows := parseTreeRows(t, body)
	if len(rows) != 1 || rows[0].ID != env.prefix+"-1" {
		t.Fatalf("admin tree rows = %+v, want [%s-1]", rows, env.prefix)
	}
}

// TestTree_HappyPath_ThreeLevels covers a multi-type, multi-level
// tree: A → B (parent-child) → C (parent-child), B blocks D.
func TestTree_HappyPath_ThreeLevels(t *testing.T) {
	env := newListEnv(t)
	// Seed A B C D
	for _, n := range []string{"-1", "-2", "-3", "-4"} {
		seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+n,
			"human", env.ownerName, nil)
	}
	// A=-1 (root), B=-2 child of A, C=-3 child of B, D=-4 (blocked by B)
	treeSeedDep(t, env.pool, env.prefix+"-2", env.prefix+"-1", "parent-child", env.owner)
	treeSeedDep(t, env.pool, env.prefix+"-3", env.prefix+"-2", "parent-child", env.owner)
	treeSeedDep(t, env.pool, env.prefix+"-2", env.prefix+"-4", "blocks", env.owner)

	// types=parent-child only → A→B→C plus D as its OWN root. D has no
	// outgoing parent-child edge (it is only reachable via the `blocks`
	// edge, which is filtered out here), so in parent-child-only forest
	// mode D is a root in its own right. Correct count is 4 (A,B,C,D).
	res, body := callProjectTree(t, env, env.owner, "user", "types=parent-child")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	rows := parseTreeRows(t, body)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4; ids=%v", len(rows), ids)
	}

	// types=parent-child+blocks → D now surfaces a SECOND time as a
	// reference under B via the `blocks` edge, in addition to still
	// being its own canonical root (D has no parent-child parent). So
	// the forest is A→B→C, D (canonical root), and D again (reference
	// under B). Correct count is 5: A,B,C,D + D-reference.
	res2, body2 := callProjectTree(t, env, env.owner, "user",
		"types=parent-child&types=blocks")
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res2.StatusCode, body2)
	}
	rows2 := parseTreeRows(t, body2)
	if len(rows2) != 5 {
		ids2 := make([]string, len(rows2))
		for i, r := range rows2 {
			ids2[i] = r.ID
		}
		t.Fatalf("rows = %d, want 5; ids=%v", len(rows2), ids2)
	}
	// D appears twice: once as a canonical root (depth 0, no parent)
	// and once as a REFERENCE under B via the blocks edge. The
	// reference row is the one carrying the blocks edge + parent B.
	var dCanonical, dRef *treeRow
	for i := range rows2 {
		if rows2[i].ID == env.prefix+"-4" {
			if rows2[i].IsReference {
				dRef = &rows2[i]
			} else {
				dCanonical = &rows2[i]
			}
		}
	}
	if dCanonical == nil {
		t.Fatalf("missing canonical D root in %+v", rows2)
	}
	if dRef == nil {
		t.Fatalf("missing D reference row in %+v", rows2)
	}
	if dRef.EdgeType != "blocks" {
		t.Fatalf("D reference edgeType = %q, want blocks", dRef.EdgeType)
	}
	if dRef.ParentID != env.prefix+"-2" {
		t.Fatalf("D reference parentId = %q, want %s-2", dRef.ParentID, env.prefix)
	}
}

// TestTreeAll_CrossProjectHidden: an admin and non-admin caller see
// different visibility on cross-project tree. The non-admin gets
// `<hidden>` for issues in projects they can't access; admin sees the
// real titles.
func TestTreeAll_CrossProjectHidden(t *testing.T) {
	pool := newListTestPool(t)

	// Two projects with different owners. otherUser cannot see
	// env's project; env.owner cannot see other's project.
	owner1, owner1Name := seedListUser(t, pool, "user")
	owner2, owner2Name := seedListUser(t, pool, "user")
	_ = owner1Name
	pid1, prefix1 := seedListProject(t, pool, owner1, "lh")
	pid2, prefix2 := seedListProject(t, pool, owner2, "lh")
	_ = pid2

	// Cross-project parent-child: owner2's issue is parent, owner1's
	// issue is child. owner1 can see only pid1, so when the tree is
	// rooted at the child, the parent surfaces as `<hidden>`.
	seedListIssue(t, pool, pid1, owner1, prefix1+"-1", "human", owner1Name, nil)
	seedListIssue(t, pool, pid2, owner2, prefix2+"-1", "human", owner2Name, nil)
	// child = pid1 issue, parent = pid2 issue
	treeSeedDep(t, pool, prefix1+"-1", prefix2+"-1", "parent-child", owner1)

	store := issues.NewStore(pool)
	// owner1 — non-admin, anchored at the child to force walking
	// up through the cross-project edge.
	root := prefix1 + "-1"
	res, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.ForUser(owner1),
		Caller: owner1,
		RootID: &root,
		Types:  []string{dependencies.DepParentChild},
	})
	if err != nil {
		t.Fatalf("IssueTree (non-admin): %v", err)
	}
	// One row only — the child. parent is not reachable via
	// outgoing parent-child from a child (canonical direction is
	// from=child, to=parent). The cross-project hidden ancestor
	// test is most visible when we anchor at the PARENT and walk
	// down. Reframe:
	if len(res.Items) != 1 {
		t.Fatalf("non-admin tree rows = %d, want 1; items=%+v", len(res.Items), res.Items)
	}
	// Now reframe: anchor at the visible side and verify the
	// invisible PROJECT issue would surface as <hidden>. We need a
	// cross-project edge in the walk direction (from→to) where the
	// from is visible and the to is hidden. Use parent-child:
	// from=child, to=parent; child=visible, parent=hidden.
	// Verify that adding the "related" edge SURFACES the hidden
	// side (parent-child alone wouldn't expose it on a downward
	// walk):
	treeSeedDep(t, pool, prefix1+"-1", prefix2+"-1", "related", owner1)
	res3, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.ForUser(owner1),
		Caller: owner1,
		Types:  []string{dependencies.DepParentChild, dependencies.DepRelated},
		RootID: &root,
	})
	if err != nil {
		t.Fatalf("IssueTree (related): %v", err)
	}
	// Find the stub of prefix2-1 in res3.Items — `<hidden>`, under a
	// synthetic id (sk-674.18 T009: no real id of a hidden issue).
	var hidden *issues.IssueTreeNode
	for i := range res3.Items {
		if res3.Items[i].Item.ID == prefix2+"-1" {
			t.Fatalf("the hidden issue's real id %s is on the tree", prefix2+"-1")
		}
		if res3.Items[i].Item.ID == "hidden-1" {
			hidden = &res3.Items[i]
		}
	}
	if hidden == nil {
		t.Fatalf("expected hidden ancestor in res; got %+v", res3.Items)
	}
	if hidden.Item.Title != "<hidden>" {
		t.Fatalf("hidden.Title = %q, want <hidden>", hidden.Item.Title)
	}
	if !hidden.IsReference {
		t.Fatalf("hidden row should be IsReference=true")
	}

	// Admin sees the real title.
	admin, _ := seedListUser(t, pool, "admin")
	res4, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.Unrestricted(),
		Caller: admin,
		Types:  []string{dependencies.DepParentChild, dependencies.DepRelated},
		RootID: &root,
	})
	if err != nil {
		t.Fatalf("IssueTree (admin): %v", err)
	}
	var visible *issues.IssueTreeNode
	for i := range res4.Items {
		if res4.Items[i].Item.ID == prefix2+"-1" {
			visible = &res4.Items[i]
			break
		}
	}
	if visible == nil {
		t.Fatalf("admin should see prefix2-1; got %+v", res4.Items)
	}
	if visible.Item.Title == "<hidden>" {
		t.Fatalf("admin row Title = <hidden>; want real title")
	}
}

// TestTreeAll_ExternalHidden_HardRedact is the sk-163.19 security
// regression: an EXTERNAL caller reaching /issues/tree must not receive the
// real hierarchical id (which embeds the project prefix) nor the projectId of
// a cross-project ancestor living in a project they cannot see. Internal
// non-admin callers keep the id + projectId (the accepted war-room SOFT
// contract — only the title is blanked); external callers get a prefix-free
// synthetic stub id and a zeroed projectId (HARD).
func TestTreeAll_ExternalHidden_HardRedact(t *testing.T) {
	pool := newListTestPool(t)

	owner1, owner1Name := seedListUser(t, pool, "user")
	owner2, owner2Name := seedListUser(t, pool, "user")
	pid1, prefix1 := seedListProject(t, pool, owner1, "lh")
	pid2, prefix2 := seedListProject(t, pool, owner2, "lh")

	// Visible root in pid1, hidden ancestor in pid2, surfaced via a
	// cross-project related edge (from=visible, to=hidden) so a downward walk
	// rooted at the visible issue reaches the hidden one.
	seedListIssue(t, pool, pid1, owner1, prefix1+"-1", "human", owner1Name, nil)
	seedListIssue(t, pool, pid2, owner2, prefix2+"-1", "human", owner2Name, nil)
	treeSeedDep(t, pool, prefix1+"-1", prefix2+"-1", "related", owner1)

	store := issues.NewStore(pool)
	root := prefix1 + "-1"
	treeTypes := []string{dependencies.DepParentChild, dependencies.DepRelated}

	// ── External caller — Hard redaction ──
	extRes, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.ForUser(owner1),
		Caller: owner1,
		Types:  treeTypes,
		RootID: &root,
	})
	if err != nil {
		t.Fatalf("IssueTree (external): %v", err)
	}

	var extHidden *issues.IssueTreeNode
	for i := range extRes.Items {
		if extRes.Items[i].Item.Title == "<hidden>" {
			extHidden = &extRes.Items[i]
			break
		}
	}
	if extHidden == nil {
		t.Fatalf("expected a <hidden> stub for external; items=%+v", extRes.Items)
	}
	if !extHidden.IsReference {
		t.Errorf("external hidden stub must be IsReference=true")
	}
	// FirstSeenAt must stay nil so the frontend's non-clickable guard
	// (isReference && firstSeenAt===null && title==="<hidden>") holds.
	if extHidden.FirstSeenAt != nil {
		t.Errorf("external hidden stub FirstSeenAt must be nil, got %q", *extHidden.FirstSeenAt)
	}
	if !strings.HasPrefix(extHidden.Item.ID, "hidden-") {
		t.Errorf("external hidden stub id = %q, want a prefix-free hidden-<n> synthetic token", extHidden.Item.ID)
	}
	if extHidden.Item.ProjectID != uuid.Nil {
		t.Errorf("external hidden stub projectId = %s, want zero uuid", extHidden.Item.ProjectID)
	}

	// Airtight leak assertion: the marshaled tree JSON carries NO substring
	// of the invisible project's prefix, the hidden issue id, or the hidden
	// projectId uuid.
	raw, err := json.Marshal(extRes)
	if err != nil {
		t.Fatalf("marshal external tree: %v", err)
	}
	blob := string(raw)
	for _, secret := range []string{prefix2, prefix2 + "-1", pid2.String()} {
		if strings.Contains(blob, secret) {
			t.Errorf("external tree JSON leaks %q\nJSON: %s", secret, blob)
		}
	}

	// ── Internal non-admin — the same Hard stub (sk-674.18 T009) ──
	intRes, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.ForUser(owner1),
		Caller: owner1,
		Types:  treeTypes,
		RootID: &root,
	})
	if err != nil {
		t.Fatalf("IssueTree (internal): %v", err)
	}
	var intHidden *issues.IssueTreeNode
	for i := range intRes.Items {
		if intRes.Items[i].Item.Title == "<hidden>" {
			intHidden = &intRes.Items[i]
			break
		}
	}
	if intHidden == nil {
		t.Fatalf("expected a <hidden> stub for internal; items=%+v", intRes.Items)
	}
	if intHidden.Item.ID != "hidden-1" {
		t.Errorf("internal hidden stub id = %q, want hidden-1 (no real id of %s)", intHidden.Item.ID, prefix2+"-1")
	}
	if intHidden.Item.ProjectID != uuid.Nil {
		t.Errorf("internal hidden stub projectId = %s, want the nil uuid (not %s)", intHidden.Item.ProjectID, pid2)
	}
}

// TestTreeAll_ExternalHidden_ParentIdCurrent_HardRedact — sk-163.19 follow-up
// (found in review). The original test used a `related` edge, which
// populates ParentRef but NOT IssueSummary.ParentID (parentIdCurrent). A row
// whose parent-child parent lives in an invisible project must not leak that
// parent's real hierarchical id (embeds the prefix) via parentIdCurrent on the
// external tree — the Hard path now runs sum.ParentID through wireID for EVERY
// row.
func TestTreeAll_ExternalHidden_ParentIdCurrent_HardRedact(t *testing.T) {
	pool := newListTestPool(t)

	owner1, owner1Name := seedListUser(t, pool, "user")
	owner2, owner2Name := seedListUser(t, pool, "user")
	pid1, prefix1 := seedListProject(t, pool, owner1, "lh")
	pid2, prefix2 := seedListProject(t, pool, owner2, "lh")

	// Visible child in pid1 whose parent-child parent lives in the invisible
	// pid2 → the child's parentIdCurrent points at the hidden parent id.
	seedListIssue(t, pool, pid1, owner1, prefix1+"-1", "human", owner1Name, nil)
	seedListIssue(t, pool, pid2, owner2, prefix2+"-1", "human", owner2Name, nil)
	treeSeedDep(t, pool, prefix1+"-1", prefix2+"-1", dependencies.DepParentChild, owner1)

	store := issues.NewStore(pool)
	root := prefix1 + "-1"
	extRes, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.ForUser(owner1),
		Caller: owner1,
		Types:  []string{dependencies.DepParentChild, dependencies.DepRelated},
		RootID: &root,
	})
	if err != nil {
		t.Fatalf("IssueTree (external): %v", err)
	}

	// The visible child's parentIdCurrent must be the synthetic token, never the
	// real hidden parent id.
	var child *issues.IssueTreeNode
	for i := range extRes.Items {
		if extRes.Items[i].Item.ID == prefix1+"-1" {
			child = &extRes.Items[i]
			break
		}
	}
	if child == nil {
		t.Fatalf("visible child not in tree; items=%+v", extRes.Items)
	}
	if child.Item.ParentID != nil && !strings.HasPrefix(*child.Item.ParentID, "hidden-") {
		t.Errorf("visible child parentIdCurrent = %q, want a synthetic hidden-<n> token (leaks the hidden parent)", *child.Item.ParentID)
	}

	// Airtight: the marshaled external tree carries no substring of the hidden
	// project's prefix, the hidden parent id, or its projectId uuid.
	raw, err := json.Marshal(extRes)
	if err != nil {
		t.Fatalf("marshal external tree: %v", err)
	}
	blob := string(raw)
	for _, secret := range []string{prefix2, prefix2 + "-1", pid2.String()} {
		if strings.Contains(blob, secret) {
			t.Errorf("external tree JSON leaks %q (parentIdCurrent path)\nJSON: %s", secret, blob)
		}
	}
}

// intToStr is a tiny helper to avoid pulling strconv into a test file
// that doesn't otherwise need it. Used to build hierarchical issue ids.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestTree_RootIdOutsideVisible_NoLeak is the sk-115 security regression:
// a caller must not be able to probe an issue in a project they cannot see
// by passing its id as rootId. Before the seed-scoping fix, an out-of-scope
// rootId seeded the row directly and the visibility gate redacted it to a
// "<hidden>" stub — leaking the issue's existence, project, type and status
// (ids are enumerable as prefix-N). The seed must now be constrained to the
// caller's project/visible-projects so an out-of-scope rootId yields an
// EMPTY forest, on both the project-scoped (ProjectID) and global
// (VisibleProjects) shapes.
func TestTree_RootIdOutsideVisible_NoLeak(t *testing.T) {
	pool := newListTestPool(t)

	owner1, owner1Name := seedListUser(t, pool, "user")
	owner2, owner2Name := seedListUser(t, pool, "user")
	pid1, prefix1 := seedListProject(t, pool, owner1, "lh")
	pid2, prefix2 := seedListProject(t, pool, owner2, "lh")
	_ = prefix1
	// owner1 is a member of pid1 only; owner2's issue lives in pid2.
	seedListIssue(t, pool, pid1, owner1, prefix1+"-1", "human", owner1Name, nil)
	seedListIssue(t, pool, pid2, owner2, prefix2+"-1", "human", owner2Name, nil)

	store := issues.NewStore(pool)
	root := prefix2 + "-1" // an issue owner1 cannot see

	// Project-scoped shape (what /projects/{pid}/issues/tree issues).
	res, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:     authz.ForUser(owner1),
		Caller:    owner1,
		ProjectID: &pid1,
		RootID:    &root,
		Types:     []string{dependencies.DepParentChild},
	})
	if err != nil {
		t.Fatalf("IssueTree (project-scoped): %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("project-scoped rootId leak: got %d row(s), want 0: %+v", len(res.Items), res.Items)
	}

	// Global shape (ProjectID nil, scoped by VisibleProjects).
	res2, err := store.IssueTree(t.Context(), issues.IssueTreeArgs{
		Scope:  authz.ForUser(owner1),
		Caller: owner1,
		RootID: &root,
		Types:  []string{dependencies.DepParentChild},
	})
	if err != nil {
		t.Fatalf("IssueTree (visible-projects): %v", err)
	}
	if len(res2.Items) != 0 {
		t.Fatalf("visible-projects rootId leak: got %d row(s), want 0: %+v", len(res2.Items), res2.Items)
	}
}

// Sanity guard: dependencies.AllowedTypes / issues.AllowedTreeTypes
// must agree so the handler-side whitelist stays in sync with the
// store-side walk.
func TestTreeAllowedTypes_InSyncWithDependencies(t *testing.T) {
	want := map[string]struct{}{
		dependencies.DepBlocks:         {},
		dependencies.DepRelated:        {},
		dependencies.DepDiscoveredFrom: {},
		dependencies.DepSupersedes:     {},
		dependencies.DepDuplicates:     {},
		dependencies.DepWaitsFor:       {},
		dependencies.DepParentChild:    {},
	}
	got := issues.AllowedTreeTypes()
	if len(got) != len(want) {
		t.Fatalf("AllowedTreeTypes() length = %d, want %d", len(got), len(want))
	}
	for _, t1 := range got {
		if _, ok := want[t1]; !ok {
			t.Fatalf("unexpected type %q in AllowedTreeTypes()", t1)
		}
		if !issues.IsAllowedTreeType(t1) {
			t.Fatalf("IsAllowedTreeType(%q) = false; want true", t1)
		}
	}
}

// Ensure the unused imports don't trip vet. (strings + projects are
// referenced indirectly through listTestEnv setup.)
var _ = strings.TrimSpace
var _ = projects.EnsureProjectVisible

// ─────────────── sk-421.2 T008: acceptance of INV-2, order and depth ───────────────

// callProjectTreeAs — like callProjectTree, but lets the caller be declared
// an EXTERNAL member. The previous helper could not do this, so the Hard edition
// of the project variant was checked by no test (sk-421.2, round 2).
func callProjectTreeAs(t *testing.T, env listTestEnv, userID uuid.UUID, role string, isExternal bool, query string) (*http.Response, map[string]any) {
	t.Helper()
	url := "/api/v1/projects/" + env.projectID.String() + "/issues/tree"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", env.projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	if userID != uuid.Nil {
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, role)
		ctx = auth.WithIsExternal(ctx, isExternal)
	}
	// As the production router does (authz.RequestCache).
	ctx = authz.WithRequestCache(ctx)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	env.handler.Tree(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// callAllTree hits the CROSS-PROJECT variant through the handler. Until sk-421.2
// IssuesAllHandler.Tree was never called in the tree tests — it was checked
// through the store, which is a different serialisation path (the handler has an
// items wrapper). The literal reference must be compared with the JSON the client sees.
func callAllTree(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, role string, isExternal bool, query string) (*http.Response, map[string]any) {
	t.Helper()
	h := NewIssuesAllHandler(pool, issues.NewStore(pool), nil)
	url := "/api/v1/issues/tree"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := req.Context()
	if userID != uuid.Nil {
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, role)
		ctx = auth.WithIsExternal(ctx, isExternal)
	}
	// As the production router does (authz.RequestCache).
	ctx = authz.WithRequestCache(ctx)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Tree(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// stubItemLiteral — the HANDWRITTEN REFERENCE of the nested item of a stub row.
//
// Why a literal and not "the set of non-empty fields": in IssueSummary some
// fields are declared without omitempty, and the stub carries 0, false,
// the zero UUID and the zero time on the wire. On a naive reading that is "non-empty", and
// the check "all other fields are empty" would be red forever or degenerate
// into a list of exceptions, i.e. it would stop being a gate.
//
// The INV-2 whitelist is the id, project, stub title, reference mark,
// depth and parent reference. Everything else must be empty, and
// it is asserted BY NAME right here, isBlocked included: the blocking
// mark is named by the invariant as inadmissible, and today it does not leak only
// because the stub literal in tree.go does not set it.
func stubItemLiteral(wantID, wantProjectID string) map[string]any {
	return map[string]any{
		"id":           wantID,
		"projectId":    wantProjectID,
		"title":        "<hidden>",
		"type":         "",
		"status":       "",
		"version":      float64(0),
		"priority":     float64(0),
		"pinned":       false,
		"isBlocked":    false,
		"createdBy":    "00000000-0000-0000-0000-000000000000",
		"componentIds": []any{},
		"labels":       []any{},
		"updatedAt":    "0001-01-01T00:00:00Z",
		"createdAt":    "0001-01-01T00:00:00Z",
	}
}

// findStubRow returns the raw stub row from the response body.
func findStubRow(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing or not an array: %T (body=%v)", body["items"], body)
	}
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		item, _ := row["item"].(map[string]any)
		if item != nil && item["title"] == "<hidden>" {
			return row
		}
	}
	t.Fatalf("stub row not found; items=%v", items)
	return nil
}

// assertStubRow checks the stub row against the reference IN FULL: both the nested
// item (all keys and values) and the top level of the row.
func assertStubRow(t *testing.T, row map[string]any, wantID, wantProjectID string, wantDepth float64) {
	t.Helper()

	item, ok := row["item"].(map[string]any)
	if !ok {
		t.Fatalf("item is not an object: %T", row["item"])
	}
	want := stubItemLiteral(wantID, wantProjectID)
	if !reflect.DeepEqual(item, want) {
		for k, wv := range want {
			if gv, present := item[k]; !present {
				t.Errorf("item.%s is absent on the wire (want %#v)", k, wv)
			} else if !reflect.DeepEqual(gv, wv) {
				t.Errorf("item.%s = %#v, reference %#v", k, gv, wv)
			}
		}
		for k, gv := range item {
			if _, expected := want[k]; !expected {
				t.Errorf("stub row carries an EXTRA field item.%s = %#v — the INV-2 "+
					"whitelist does not allow it; if the field appeared deliberately, it must "+
					"be added to the reference in the same commit", k, gv)
			}
		}
	}

	if got := row["depth"]; got != wantDepth {
		t.Errorf("depth = %#v, want %v", got, wantDepth)
	}
	if row["isReference"] != true {
		t.Errorf("isReference = %#v, want true", row["isReference"])
	}
	if _, present := row["firstSeenAt"]; present {
		t.Errorf("firstSeenAt = %#v is present on the stub; the fixture must make it the "+
			"CANONICAL first occurrence, otherwise the literal is red because of the fixture, not the change",
			row["firstSeenAt"])
	}
	// sk-421.2.8 INV-3: a stub does not get the blockers field even under the
	// includeBlockers flag — even when the inaccessible issue is blocked.
	if got, present := row["blockers"]; present {
		t.Errorf("blockers = %#v is present on the stub — INV-3: a stub gets no blockers field", got)
	}
}

// TestTree_StubRowMatchesLiteral — INV-2 / SC-003: the row of an inaccessible issue
// equals the handwritten reference IN FULL, on BOTH request variants, under an
// internal and an external member, with and without the depth parameter.
func TestTree_StubRowMatchesLiteral(t *testing.T) {
	pool := newListTestPool(t)

	owner1, owner1Name := seedListUser(t, pool, "user")
	owner2, owner2Name := seedListUser(t, pool, "user")
	pid1, prefix1 := seedListProject(t, pool, owner1, "lit")
	pid2, prefix2 := seedListProject(t, pool, owner2, "lit")

	// A visible root in pid1, an inaccessible CHILD in pid2. The parent-child edge
	// is written from the child to the parent; the traversal flips it and goes down.
	// The stub is met first and canonically — firstSeenAt will not appear.
	root := prefix1 + "-1"
	hidden := prefix2 + "-1"
	seedListIssue(t, pool, pid1, owner1, root, "human", owner1Name, nil)
	seedListIssue(t, pool, pid2, owner2, hidden, "human", owner2Name, nil)
	treeSeedDep(t, pool, hidden, root, dependencies.DepParentChild, owner1)

	// sk-421.2.8, row 11: the inaccessible issue is BLOCKED by an unclosed
	// blocker — otherwise the absence of blockers on the stub under the flag would be proven
	// trivially.
	hiddenBlocker := prefix2 + "-2"
	seedListIssue(t, pool, pid2, owner2, hiddenBlocker, "human", owner2Name, nil)
	treeSeedDep(t, pool, hidden, hiddenBlocker, dependencies.DepBlocks, owner2)
	var hiddenIsBlocked bool
	if err := pool.QueryRow(t.Context(), `
SELECT EXISTS (SELECT 1 FROM dependencies d JOIN issues b ON b.id = d.to_id
                WHERE d.from_id = $1 AND d.type IN ('blocks', 'waits-for')
                  AND b.status NOT IN ('closed', 'standing'))`, hidden).Scan(&hiddenIsBlocked); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if !hiddenIsBlocked {
		t.Fatalf("precondition: inaccessible issue %s is not blocked", hidden)
	}

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: owner1, ownerName: owner1Name, projectID: pid1, prefix: prefix1}

	zeroUUID := "00000000-0000-0000-0000-000000000000"
	for _, q := range []string{"", "maxDepth=2", "includeBlockers=true", "maxDepth=2&includeBlockers=true"} {
		name := "no parameter"
		if q != "" {
			name = q
		}
		t.Run("project/internal/"+name, func(t *testing.T) {
			res, body := callProjectTreeAs(t, env, owner1, "user", false, "rootId="+root+"&"+q)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("code %d, body %v", res.StatusCode, body)
			}
			// sk-674.18 T009: the internal reader gets the same Hard stub as
			// the external one — no real id, no projectId.
			assertStubRow(t, findStubRow(t, body), "hidden-1", zeroUUID, float64(1))
		})
		t.Run("project/external/"+name, func(t *testing.T) {
			_, body := callProjectTreeAs(t, env, owner1, "user", true, "rootId="+root+"&"+q)
			row := findStubRow(t, body)
			item, _ := row["item"].(map[string]any)
			id, _ := item["id"].(string)
			if !strings.HasPrefix(id, "hidden-") {
				t.Fatalf("the external member was sent the id %q, want a synthetic token", id)
			}
			assertStubRow(t, row, id, zeroUUID, float64(1))
		})
		t.Run("cross-project/internal/"+name, func(t *testing.T) {
			_, body := callAllTree(t, pool, owner1, "user", false, "rootId="+root+"&"+q)
			// sk-674.18 T009: the internal reader gets the same Hard stub as
			// the external one — no real id, no projectId.
			assertStubRow(t, findStubRow(t, body), "hidden-1", zeroUUID, float64(1))
		})
		t.Run("cross-project/external/"+name, func(t *testing.T) {
			_, body := callAllTree(t, pool, owner1, "user", true, "rootId="+root+"&"+q)
			row := findStubRow(t, body)
			item, _ := row["item"].(map[string]any)
			id, _ := item["id"].(string)
			assertStubRow(t, row, id, zeroUUID, float64(1))
		})
	}
}

// TestTree_SiblingOrder_NumericNotLexicographic — SC-004: siblings are ordered
// by the id RULE, not bytewise. Red before T005: the old sort put
// `.10` before `.2`.
func TestTree_SiblingOrder_NumericNotLexicographic(t *testing.T) {
	env := newListEnv(t)
	root := env.prefix + "-1"
	seedListIssue(t, env.pool, env.projectID, env.owner, root, "human", env.ownerName, nil)
	for _, n := range []string{"2", "10", "1"} {
		child := root + "." + n
		seedListIssue(t, env.pool, env.projectID, env.owner, child, "human", env.ownerName, nil)
		treeSeedDep(t, env.pool, child, root, dependencies.DepParentChild, env.owner)
	}

	_, body := callProjectTree(t, env, env.owner, "user", "rootId="+root)
	rows := parseTreeRows(t, body)

	var got []string
	for _, r := range rows {
		if r.Depth == 1 {
			got = append(got, r.ID)
		}
	}
	want := []string{root + ".1", root + ".2", root + ".10"}
	if len(got) != len(want) {
		t.Fatalf("first-level children %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sibling order %v, want %v — `.2` must come before `.10`", got, want)
		}
	}
}

// TestTreeAll_StubSortsAfterVisibleSiblings — F-24: the stub goes AFTER all
// visible siblings, whatever its real id is.
//
// The fixture is deterministic on purpose: project prefixes are random, so
// the roles "visible" and "hidden" are assigned AFTER comparing the issued prefixes —
// the hidden one is always the one whose id under the OLD rule would sort
// before the visible sibling. Otherwise the test would be green in about half
// of the runs regardless of the change.
func TestTreeAll_StubSortsAfterVisibleSiblings(t *testing.T) {
	pool := newListTestPool(t)
	ownerA, ownerAName := seedListUser(t, pool, "user")
	ownerB, ownerBName := seedListUser(t, pool, "user")
	pidA, prefixA := seedListProject(t, pool, ownerA, "ord")
	pidB, prefixB := seedListProject(t, pool, ownerB, "ord")

	// The hidden one must be the project with the SMALLER prefix: then the real id
	// of the stub under the OLD (bytewise) rule would sort BEFORE the
	// visible sibling, and the test goes red without the change, not every other time.
	visOwner, visOwnerName, visPID, visPrefix := ownerA, ownerAName, pidA, prefixA
	hidOwner, hidOwnerName, hidPID, hidPrefix := ownerB, ownerBName, pidB, prefixB
	if hidPrefix > visPrefix {
		visOwner, visOwnerName, visPID, visPrefix, hidOwner, hidOwnerName, hidPID, hidPrefix =
			hidOwner, hidOwnerName, hidPID, hidPrefix, visOwner, visOwnerName, visPID, visPrefix
	}

	root := visPrefix + "-1"
	visChild := visPrefix + "-1.1"
	hidChild := hidPrefix + "-1"
	seedListIssue(t, pool, visPID, visOwner, root, "human", visOwnerName, nil)
	seedListIssue(t, pool, visPID, visOwner, visChild, "human", visOwnerName, nil)
	seedListIssue(t, pool, hidPID, hidOwner, hidChild, "human", hidOwnerName, nil)
	treeSeedDep(t, pool, visChild, root, dependencies.DepParentChild, visOwner)
	treeSeedDep(t, pool, hidChild, root, dependencies.DepParentChild, visOwner)

	_, body := callAllTree(t, pool, visOwner, "user", false, "rootId="+root)
	rows := parseTreeRows(t, body)

	var lastVisible, stubAt = -1, -1
	for i, r := range rows {
		if r.Depth != 1 {
			continue
		}
		if r.Title == "<hidden>" {
			stubAt = i
		} else {
			lastVisible = i
		}
	}
	if stubAt < 0 {
		t.Fatalf("stub not found among the children; rows=%+v", rows)
	}
	if stubAt < lastVisible {
		t.Fatalf("stub at position %d before the visible sibling at %d — F-24 requires "+
			"the opposite (hidden prefix %q < visible %q)", stubAt, lastVisible, hidPrefix, visPrefix)
	}
}

// TestTree_MaxDepth_PrunesWalk — SC-005: the depth limit really prunes
// the traversal, and without the parameter the response is as before.
func TestTree_MaxDepth_PrunesWalk(t *testing.T) {
	env := newListEnv(t)
	root := env.prefix + "-1"
	child := root + ".1"
	grand := child + ".1"
	for _, id := range []string{root, child, grand} {
		seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", env.ownerName, nil)
	}
	treeSeedDep(t, env.pool, child, root, dependencies.DepParentChild, env.owner)
	treeSeedDep(t, env.pool, grand, child, dependencies.DepParentChild, env.owner)

	maxDepthOf := func(query string) int {
		_, body := callProjectTree(t, env, env.owner, "user", query)
		deepest := 0
		for _, r := range parseTreeRows(t, body) {
			if r.Depth > deepest {
				deepest = r.Depth
			}
		}
		return deepest
	}

	if got := maxDepthOf("rootId=" + root); got != 2 {
		t.Fatalf("without the parameter the deepest row depth=%d, want 2", got)
	}
	if got := maxDepthOf("rootId=" + root + "&maxDepth=1"); got != 1 {
		t.Fatalf("with maxDepth=1 the deepest row depth=%d, want 1 — the parameter "+
			"must prune the traversal itself", got)
	}
}

// ─────────────── sk-428: tails of the review of sk-421.2 ───────────────

// TestTreeAll_ExternalStubsOrderedBySyntheticID — F-24 / US-6: for an external
// member the mutual order of stubs is set by their SYNTHETIC ids, not by the
// real ones by the order rule. Token numbering (sk-163.19) goes bytewise
// over real ids, so on the pair `X-2` / `X-10` it gives `hidden-1 = X-10`,
// `hidden-2 = X-2`; sorting by real ids by the US-6 rule put `X-2`
// before `X-10` — and on the wire `hidden-2` ended up above `hidden-1`, i.e. the
// order of tokens reported that the bytewise and numeric order of hidden ids
// diverge.
//
// The fixture is deterministic: the pair `-2` / `-10` diverges under the two rules with
// any prefix, the randomness of the prefix decides nothing here.
func TestTreeAll_ExternalStubsOrderedBySyntheticID(t *testing.T) {
	pool := newListTestPool(t)
	visOwner, visOwnerName := seedListUser(t, pool, "user")
	hidOwner, hidOwnerName := seedListUser(t, pool, "user")
	visPID, visPrefix := seedListProject(t, pool, visOwner, "syn")
	hidPID, hidPrefix := seedListProject(t, pool, hidOwner, "syn")

	root := visPrefix + "-1"
	visChild := root + ".1"
	seedListIssue(t, pool, visPID, visOwner, root, "human", visOwnerName, nil)
	seedListIssue(t, pool, visPID, visOwner, visChild, "human", visOwnerName, nil)
	treeSeedDep(t, pool, visChild, root, dependencies.DepParentChild, visOwner)
	for _, id := range []string{hidPrefix + "-2", hidPrefix + "-10"} {
		seedListIssue(t, pool, hidPID, hidOwner, id, "human", hidOwnerName, nil)
		treeSeedDep(t, pool, id, root, dependencies.DepParentChild, visOwner)
	}

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: visOwner, ownerName: visOwnerName, projectID: visPID, prefix: visPrefix}

	cases := []struct {
		name string
		call func() map[string]any
	}{
		{"project", func() map[string]any {
			_, body := callProjectTreeAs(t, env, visOwner, "user", true, "rootId="+root)
			return body
		}},
		{"cross-project", func() map[string]any {
			_, body := callAllTree(t, pool, visOwner, "user", true, "rootId="+root)
			return body
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range parseTreeRows(t, tc.call()) {
				if r.Depth == 1 {
					got = append(got, r.ID)
				}
			}
			want := []string{visChild, "hidden-1", "hidden-2"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("first-level children of the external member = %v, want %v — "+
					"stubs after visible siblings and among themselves by synthetic id (F-24)",
					got, want)
			}
		})
	}
}

// TestTree_VisibleTaskTitledHiddenIsOrdinaryRow — US-5 acceptance 4, contract 7, SC-003 (second half): a visible issue that the user
// titled `<hidden>` is an ordinary row: real id, status and type, without the
// reference mark. What makes a stub is membership in the set of inaccessible issues, not
// the title (sk-421.2 T004).
//
// The row is looked up by the REAL id, not by the title: findStubRow looks up by
// title, and a rollback to comparing the title would not make it fail. The same tree
// also has a real stub — the check is that the mark distinguishes the two.
func TestTree_VisibleTaskTitledHiddenIsOrdinaryRow(t *testing.T) {
	pool := newListTestPool(t)
	visOwner, visOwnerName := seedListUser(t, pool, "user")
	hidOwner, hidOwnerName := seedListUser(t, pool, "user")
	visPID, visPrefix := seedListProject(t, pool, visOwner, "ttl")
	hidPID, hidPrefix := seedListProject(t, pool, hidOwner, "ttl")

	root := visPrefix + "-1"
	decoy := root + ".1"
	hidden := hidPrefix + "-1"
	seedListIssue(t, pool, visPID, visOwner, root, "human", visOwnerName, nil)
	seedListIssue(t, pool, visPID, visOwner, decoy, "human", visOwnerName, nil)
	seedListIssue(t, pool, hidPID, hidOwner, hidden, "human", hidOwnerName, nil)
	treeSeedDep(t, pool, decoy, root, dependencies.DepParentChild, visOwner)
	treeSeedDep(t, pool, hidden, root, dependencies.DepParentChild, visOwner)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET title = '<hidden>' WHERE id = $1`, decoy); err != nil {
		t.Fatalf("rename of the visible issue: %v", err)
	}

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: visOwner, ownerName: visOwnerName, projectID: visPID, prefix: visPrefix}

	for _, external := range []bool{false, true} {
		who := "internal"
		if external {
			who = "external"
		}
		variants := []struct {
			name string
			call func() map[string]any
		}{
			{"project/" + who, func() map[string]any {
				_, body := callProjectTreeAs(t, env, visOwner, "user", external, "rootId="+root)
				return body
			}},
			{"cross-project/" + who, func() map[string]any {
				_, body := callAllTree(t, pool, visOwner, "user", external, "rootId="+root)
				return body
			}},
		}
		for _, v := range variants {
			t.Run(v.name, func(t *testing.T) {
				items, ok := v.call()["items"].([]any)
				if !ok {
					t.Fatal("items missing or not an array")
				}
				var decoyRow map[string]any
				stubs := 0
				for _, raw := range items {
					row, _ := raw.(map[string]any)
					item, _ := row["item"].(map[string]any)
					if item == nil || item["title"] != "<hidden>" {
						continue
					}
					if item["id"] == decoy {
						decoyRow = row
						continue
					}
					stubs++
					if row["isReference"] != true {
						t.Errorf("real stub %v without the reference mark", item["id"])
					}
				}
				if stubs != 1 {
					t.Errorf("real stubs %d, want 1", stubs)
				}
				if decoyRow == nil {
					t.Fatalf("visible issue %s titled <hidden> not found by its id; items=%v", decoy, items)
				}
				if got, present := decoyRow["isReference"]; present && got != false {
					t.Errorf("visible issue titled <hidden> is marked as a reference: isReference=%#v", got)
				}
				item := decoyRow["item"].(map[string]any)
				if item["status"] != "open" || item["type"] != "task" {
					t.Errorf("visible issue titled <hidden> lost status/type: status=%#v type=%#v",
						item["status"], item["type"])
				}
				if item["projectId"] != visPID.String() {
					t.Errorf("visible issue titled <hidden>: projectId=%#v, want %s",
						item["projectId"], visPID)
				}
			})
		}
	}
}

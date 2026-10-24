package tools

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// epicCreateFixture wires the handler + a seeded project for the parentId /
// labels / reviewRequired tests. Each test seeds its own project, so a
// narrowed package run stays self-sufficient.
type epicCreateFixture struct {
	pool      *pgxpool.Pool
	userID    uuid.UUID
	projectID uuid.UUID
	prefix    string
	handler   func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	ctx       context.Context
}

func newEpicCreateFixture(t *testing.T) *epicCreateFixture {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ecp")
	return &epicCreateFixture{
		pool:      pool,
		userID:    userID,
		projectID: pid,
		prefix:    prefix,
		handler:   handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool)),
		ctx:       withCtx(t, userID, "admin"),
	}
}

func (f *epicCreateFixture) call(t *testing.T, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := f.handler(f.ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return res
}

// parentIDOf reads the CANONICAL parent: the parent-child dep edge, not the
// issues.parent_id column. That column has been frozen since sk-35 (see the
// note at internal/issues/store.go:649) and is NULL on every issue created
// through this path — asserting on it would pass for a node with no parent at
// all. The question "is this node reachable from the branch" is answered by
// the edge, which is what every parentId-scoped listing derives from.
func parentIDOf(t *testing.T, pool *pgxpool.Pool, issueID string) (string, bool) {
	t.Helper()
	var parent string
	err := pool.QueryRow(context.Background(),
		`SELECT to_id FROM dependencies WHERE from_id = $1 AND type = 'parent-child'`,
		issueID).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read parent edge of %s: %v", issueID, err)
	}
	return parent, true
}

// listedUnder answers the acceptance question directly, through the tool the
// caller actually uses: does skein_list(parentId=…) return this issue — with
// no skein_reparent in between.
func listedUnder(t *testing.T, f *epicCreateFixture, parentID string) []string {
	t.Helper()
	listHandler := handleList(f.pool, issues.NewStore(f.pool), projects.NewStore(f.pool), nil)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  f.projectID.String(),
		"parentId": parentID,
	}
	res, err := listHandler(f.ctx, req)
	if err != nil {
		t.Fatalf("list handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("list returned an error: %+v", res)
	}
	items, _ := extractToolJSON(t, res)["items"].([]any)
	out := []string{}
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		if id, _ := row["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func labelsOf(t *testing.T, pool *pgxpool.Pool, issueID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT label FROM issue_labels WHERE issue_id = $1`, issueID)
	if err != nil {
		t.Fatalf("read labels of %s: %v", issueID, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("scan label: %v", err)
		}
		out = append(out, l)
	}
	// Without this a truncated result set reads as "this issue has no
	// labels" — which is exactly what the inheritance test asserts, so the
	// failure would pass as a success.
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate labels of %s: %v", issueID, err)
	}
	sort.Strings(out)
	return out
}

func reviewRequiredOf(t *testing.T, pool *pgxpool.Pool, issueID string) bool {
	t.Helper()
	var flag bool
	if err := pool.QueryRow(context.Background(),
		`SELECT review_required FROM issues WHERE id = $1`, issueID).Scan(&flag); err != nil {
		t.Fatalf("read review_required of %s: %v", issueID, err)
	}
	return flag
}

func epicAndChildIDs(t *testing.T, res *mcpgo.CallToolResult) (string, []string) {
	t.Helper()
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
	childIDs := []string{}
	rawChildren, _ := body["children"].([]any)
	for i, raw := range rawChildren {
		child, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("children[%d] wrong type: %T", i, raw)
		}
		id, _ := child["id"].(string)
		if id == "" {
			t.Fatalf("children[%d].id missing", i)
		}
		childIDs = append(childIDs, id)
	}
	return epicID, childIDs
}

// TestEpicCreate_ParentID_LandsUnderParent is the point of the parameter:
// the node must be reachable from a parentId-scoped listing with no
// follow-up reparent, and its id must come from the parent's subtree
// rather than the project counter.
func TestEpicCreate_ParentID_LandsUnderParent(t *testing.T) {
	f := newEpicCreateFixture(t)
	branch := seedIssue(t, f.pool, f.projectID, f.userID, f.prefix+"-1", "Implementation", "open", 2)

	res := f.call(t, map[string]any{
		"project":  f.projectID.String(),
		"title":    "G1 · foundation",
		"kind":     "directed",
		"parentId": branch,
		"childTemplates": []any{
			map[string]any{"title": "config predicates"},
			map[string]any{"title": "send verdict"},
		},
	})
	epicID, childIDs := epicAndChildIDs(t, res)

	gotParent, hasParent := parentIDOf(t, f.pool, epicID)
	if !hasParent || gotParent != branch {
		t.Fatalf("epic parent edge = %q (set=%v), want %q", gotParent, hasParent, branch)
	}
	// The acceptance question, asked the way the caller asks it.
	if got := listedUnder(t, f, branch); len(got) != 1 || got[0] != epicID {
		t.Fatalf("skein_list(parentId=%q) = %v, want exactly [%s] with no reparent", branch, got, epicID)
	}
	if !strings.HasPrefix(epicID, branch+".") {
		t.Fatalf("epic id = %q, want a child id of %q", epicID, branch)
	}
	if len(childIDs) != 2 {
		t.Fatalf("children = %d, want 2", len(childIDs))
	}
	for _, childID := range childIDs {
		if !strings.HasPrefix(childID, epicID+".") {
			t.Fatalf("child id = %q, want a child id of the epic %q", childID, epicID)
		}
		if p, ok := parentIDOf(t, f.pool, childID); !ok || p != epicID {
			t.Fatalf("child %s parent edge = %q (set=%v), want %q", childID, p, ok, epicID)
		}
	}
}

// TestEpicCreate_NoParentID_StaysAtRoot guards the legacy shape: omitting
// the parameter must behave exactly as before it existed, because every
// existing caller omits it.
func TestEpicCreate_NoParentID_StaysAtRoot(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project":        f.projectID.String(),
		"title":          "root epic",
		"kind":           "directed",
		"childTemplates": []any{map[string]any{"title": "only child"}},
	})
	epicID, childIDs := epicAndChildIDs(t, res)

	if _, hasParent := parentIDOf(t, f.pool, epicID); hasParent {
		t.Fatalf("epic %s has a parent, want root-level", epicID)
	}
	if strings.Contains(strings.TrimPrefix(epicID, f.prefix+"-"), ".") {
		t.Fatalf("epic id = %q, want a flat project-counter id", epicID)
	}
	if len(childIDs) != 1 || !strings.HasPrefix(childIDs[0], epicID+".") {
		t.Fatalf("children = %v, want one child of %q", childIDs, epicID)
	}
}

// TestEpicCreate_ParentNotFound: an ineligible parent is refused, and —
// atomicity — nothing at all is written, not even the epic.
func TestEpicCreate_ParentNotFound(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project":        f.projectID.String(),
		"title":          "orphan epic",
		"kind":           "directed",
		"parentId":       f.prefix + "-404",
		"childTemplates": []any{map[string]any{"title": "child"}},
	})
	if !res.IsError {
		t.Fatalf("expected isError=true for a missing parent; got %+v", res)
	}
	if got := extractToolJSON(t, res)["error"]; got != ErrCodeParentNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeParentNotFound)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, f.projectID).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if n != 0 {
		t.Fatalf("issues written = %d, want 0 — a refused parent must roll the batch back", n)
	}
}

// TestEpicCreate_ParentClosed pins the second ineligible-parent code. Without
// it a typo in mapEpicCreateError's parent_closed branch would ship green:
// only the missing-parent branch was driven by a test.
func TestEpicCreate_ParentClosed(t *testing.T) {
	f := newEpicCreateFixture(t)
	closedParent := seedIssue(t, f.pool, f.projectID, f.userID, f.prefix+"-1", "done branch", "closed", 2)

	res := f.call(t, map[string]any{
		"project":        f.projectID.String(),
		"title":          "child of a closed branch",
		"kind":           "directed",
		"parentId":       closedParent,
		"childTemplates": []any{map[string]any{"title": "child"}},
	})
	if !res.IsError {
		t.Fatalf("expected isError=true for a closed parent; got %+v", res)
	}
	if got := extractToolJSON(t, res)["error"]; got != ErrCodeParentClosed {
		t.Fatalf("error = %v, want %s", got, ErrCodeParentClosed)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, f.projectID).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	// The seeded parent lives in the same project; nothing else may exist.
	if n != 1 {
		t.Fatalf("issues = %d, want only the seeded parent — the batch must roll back", n)
	}
}

// TestEpicCreate_ParentNotVisible pins the anti-leak surface: a parent in a
// project the caller cannot see answers exactly as a nonexistent parent —
// same code, same message (INV-3, sk-674.18 T033) — never anything that
// would confirm the parent exists.
func TestEpicCreate_ParentNotVisible(t *testing.T) {
	f := newEpicCreateFixture(t)
	// A project belonging to someone else, with the caller not a member.
	otherOwner, _ := seedUser(t, f.pool, "user")
	otherPID, otherPrefix := seedProject(t, f.pool, otherOwner, "ecpx")
	hiddenParent := seedIssue(t, f.pool, otherPID, otherOwner, otherPrefix+"-1", "hidden branch", "open", 2)

	// A non-admin caller: an admin sees every project by construction.
	stranger, strangerName := seedUser(t, f.pool, "user")
	if _, err := f.pool.Exec(context.Background(),
		`WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
         VALUES ($1, $2, 'member', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, f.projectID, stranger); err != nil {
		t.Fatalf("add %s to the review project: %v", strangerName, err)
	}
	strangerCtx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":        f.projectID.String(),
		"title":          "child of an invisible branch",
		"kind":           "directed",
		"parentId":       hiddenParent,
		"childTemplates": []any{map[string]any{"title": "child"}},
	}
	res, err := f.handler(strangerCtx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for an invisible parent; got %+v", res)
	}
	hidden := extractToolJSON(t, res)

	// The reference: the same call with a parent id that does not exist.
	req.Params.Arguments.(map[string]any)["parentId"] = otherPrefix + "-999"
	missingRes, err := f.handler(strangerCtx, req)
	if err != nil {
		t.Fatalf("handler error (missing parent): %v", err)
	}
	missing := extractToolJSON(t, missingRes)
	if missing["error"] != ErrCodeParentNotFound {
		t.Fatalf("a nonexistent parent: error = %v, want %s", missing["error"], ErrCodeParentNotFound)
	}
	if hidden["error"] != missing["error"] || hidden["message"] != missing["message"] {
		t.Errorf("an invisible parent answers %v / %v, a nonexistent one %v / %v — a distinct answer would confirm the parent exists",
			hidden["error"], hidden["message"], missing["error"], missing["message"])
	}
}

// TestEpicCreate_ParentID_MalformedHandle keeps the shape check at the tool
// boundary, so a bad handle reads as validation_error rather than as a
// parent that happens not to exist.
func TestEpicCreate_ParentID_MalformedHandle(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project":  f.projectID.String(),
		"title":    "bad parent handle",
		"kind":     "directed",
		"parentId": "42",
	})
	if !res.IsError {
		t.Fatalf("expected isError=true for a bare-number parentId; got %+v", res)
	}
	if got := extractToolJSON(t, res)["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestEpicCreate_Labels_NotInheritedByChildren pins both halves: labels
// land where they were asked for, and a child that asked for none gets
// none — unlike componentIds, labels do not flow down from the epic.
func TestEpicCreate_Labels_NotInheritedByChildren(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project": f.projectID.String(),
		"title":   "labelled epic",
		"kind":    "directed",
		"labels":  []any{"triage", "group-node"},
		"childTemplates": []any{
			map[string]any{"title": "impl task"},
			map[string]any{"title": "test task", "labels": []any{"tests"}},
		},
	})
	epicID, childIDs := epicAndChildIDs(t, res)

	if got, want := labelsOf(t, f.pool, epicID), []string{"group-node", "triage"}; !equalStrings(got, want) {
		t.Fatalf("epic labels = %v, want %v", got, want)
	}
	if got := labelsOf(t, f.pool, childIDs[0]); len(got) != 0 {
		t.Fatalf("child without labels got %v, want none — the epic's labels must not be inherited", got)
	}
	if got, want := labelsOf(t, f.pool, childIDs[1]), []string{"tests"}; !equalStrings(got, want) {
		t.Fatalf("child labels = %v, want %v", got, want)
	}
}

// TestEpicCreate_Labels_RejectsNonString fails the whole batch on a bad
// child label, naming the child — the same contract every other per-child
// field has.
func TestEpicCreate_Labels_RejectsNonString(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project": f.projectID.String(),
		"title":   "bad child label",
		"kind":    "directed",
		"childTemplates": []any{
			map[string]any{"title": "child", "labels": []any{"ok", 7}},
		},
	})
	if !res.IsError {
		t.Fatalf("expected isError=true for a non-string label; got %+v", res)
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "childTemplates[0]") {
		t.Fatalf("message = %q, want it to name childTemplates[0]", msg)
	}
	// The offset inside labels must be the real one. A literal "labels[i]"
	// passes a check that only looks for the child index, and leaves the
	// agent guessing which slot it has to fix.
	if !strings.Contains(msg, "labels[1]") {
		t.Fatalf("message = %q, want it to name the failing label slot labels[1]", msg)
	}
	// ToolError merges extra fields at the TOP level of the body, not under
	// a "details" object (internal/mcp/respond.go:65).
	if got, want := body["labelIndex"], float64(1); got != want {
		t.Fatalf("labelIndex = %v, want %v", got, want)
	}
	if got, want := body["childIndex"], float64(0); got != want {
		t.Fatalf("childIndex = %v, want %v", got, want)
	}
}

// TestEpicCreate_ReviewRequired_DoesNotReachChildren: the flag is about the
// node, whose result nobody reads. The children carry the work, so they
// must keep the author's default.
func TestEpicCreate_ReviewRequired_DoesNotReachChildren(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project":        f.projectID.String(),
		"title":          "structural node",
		"kind":           "directed",
		"reviewRequired": false,
		"childTemplates": []any{map[string]any{"title": "real work"}},
	})
	epicID, childIDs := epicAndChildIDs(t, res)

	if reviewRequiredOf(t, f.pool, epicID) {
		t.Fatalf("epic review_required = true, want false")
	}
	if !reviewRequiredOf(t, f.pool, childIDs[0]) {
		t.Fatalf("child review_required = false, want the author's default (true) — the flag must not propagate")
	}
}

// TestEpicCreate_ReviewRequired_OmittedKeepsDefault separates "did not pass"
// from "passed false": a default at the tool boundary would silently cancel
// the author's personal default on every call that never mentions it.
func TestEpicCreate_ReviewRequired_OmittedKeepsDefault(t *testing.T) {
	f := newEpicCreateFixture(t)

	res := f.call(t, map[string]any{
		"project":        f.projectID.String(),
		"title":          "unflagged node",
		"kind":           "directed",
		"childTemplates": []any{map[string]any{"title": "work"}},
	})
	epicID, _ := epicAndChildIDs(t, res)

	if !reviewRequiredOf(t, f.pool, epicID) {
		t.Fatalf("epic review_required = false with the key omitted, want the author's default (true)")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

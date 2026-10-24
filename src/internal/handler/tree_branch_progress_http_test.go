package handler

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
)

// sk-461 — the response of the REAL handler, on the rows the epic section
// actually draws.
//
// # Why this file exists separately from the renderer's tests
//
// The indicator this fix serves was already built, and built correctly, with
// tests that pass it `epicProgress` in a fixture. Those tests are right to do
// that — they check RENDERING. What none of them could see is that the server
// never sends the field for any row the section renders, so the drawing code
// was unreachable on live data from the day it was written.
//
// A fixture is therefore not evidence for this defect: it asserts that the
// structure CAN carry the field, which was never in doubt. The only statement
// that closes the gap is the one below — a real GET through the real handler,
// whose decoded body carries the numbers on a row that has children. Nothing
// here is mocked: the env is the shared handler harness, the rows come out of
// Postgres, and the assertions read the JSON as a client would.

// setIssueTypeSQL / setIssueStatusSQL adjust a seeded row. seedListIssue
// writes every issue as an open task, and this fixture needs an epic root and
// a closed child.
func setIssueTypeSQL(t *testing.T, pool *pgxpool.Pool, id, issueType string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET type = $1 WHERE id = $2`, issueType, id); err != nil {
		t.Fatalf("set issue %s type=%s: %v", id, issueType, err)
	}
}

func setIssueStatusSQL(t *testing.T, pool *pgxpool.Pool, id, status string) {
	t.Helper()
	closedAt := "NULL"
	if status == "closed" {
		closedAt = "now()"
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET status = $1, closed_at = `+closedAt+` WHERE id = $2`, status, id); err != nil {
		t.Fatalf("set issue %s status=%s: %v", id, status, err)
	}
}

// httpBranchProgress — the row progress as it arrived on the wire.
//
// sk-489, matrix row 17: the branch progress has three new keys, and they must be read
// by the same function that read the previous three — otherwise "the per-outcome numbers on
// the wire" would remain a claim without an observer on the HTTP surface.
type httpBranchProgress struct {
	Closed      float64
	Total       float64
	Unavailable float64
	Done        float64
	NoWork      float64
	Counted     float64
	Depth       float64
}

// branchProgressOf reads item.epicProgress off a decoded tree row, insisting
// the key is THERE — its absence is the defect, so "missing" must fail loudly
// rather than read as a zero.
//
// The keys INSIDE epicProgress are read just as strictly and for the same reason: all
// six go without omitempty, because zero must be read as "counted, and there
// are none", while a missing key must be read as "the old api answers" (US-10). A soft
// read (`v, _ := prog["done"].(float64)`) would merge these two states and
// miss exactly the defect for which the keys are not omitempty.
func branchProgressOf(t *testing.T, items []map[string]any, id string) httpBranchProgress {
	t.Helper()
	row := treeRowByItemID(t, items, id)
	item, ok := row["item"].(map[string]any)
	if !ok {
		t.Fatalf("row %s: item is not an object (%#v)", id, row["item"])
	}
	var out httpBranchProgress
	out.Depth, _ = row["depth"].(float64)
	raw, ok := item["epicProgress"]
	if !ok || raw == nil {
		t.Fatalf("row %s came WITHOUT the epicProgress key — this is the sk-461 defect: "+
			"the section draws a branch progress indicator for this row, but the server did not send "+
			"the numbers it draws. Row keys: %v", id, sortedItemKeys(item))
	}
	prog, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("row %s: epicProgress is not an object (%#v)", id, raw)
	}
	num := func(key string) float64 {
		v, present := prog[key]
		if !present {
			t.Fatalf("row %s: epicProgress has no key %q (got %#v). All six keys "+
				"go without omitempty on purpose: zero means \"counted, and there are none\", while a missing "+
				"key the client must read as \"the old api answers\" (US-10)", id, key, prog)
		}
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("row %s: epicProgress[%q] is not a number (%#v)", id, key, v)
		}
		return f
	}
	out.Closed = num("closed")
	out.Total = num("total")
	out.Unavailable = num("unavailable")
	out.Done = num("done")
	out.NoWork = num("noWork")
	out.Counted = num("counted")
	return out
}

// sortedItemKeys names what DID arrive, so a failure above is diagnosable
// without a second run.
func sortedItemKeys(item map[string]any) []string {
	keys := make([]string, 0, len(item))
	for k := range item {
		keys = append(keys, k)
	}
	return keys
}

// TestTreeBranchProgressHTTP_RowWithChildrenCarriesProgress — the acceptance
// statement: over HTTP, a row that has children carries the triple even
// though it is a TASK, and it is a row at depth 1, i.e. one the epic section
// renders as a child rather than as its header.
func TestTreeBranchProgressHTTP_RowWithChildrenCarriesProgress(t *testing.T) {
	env := newListEnv(t)
	p := env.prefix

	epicID := p + "-1"
	parent := p + "-1.1"
	closedKid := p + "-1.1.1"
	openKid := p + "-1.1.2"
	leaf := p + "-1.1.3"

	for _, id := range []string{epicID, parent, closedKid, openKid, leaf} {
		seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", "", nil)
	}
	setIssueTypeSQL(t, env.pool, epicID, "epic")
	setIssueStatusSQL(t, env.pool, closedKid, "closed")

	treeSeedDep(t, env.pool, parent, epicID, dependencies.DepParentChild, env.owner)
	for _, kid := range []string{closedKid, openKid, leaf} {
		treeSeedDep(t, env.pool, kid, parent, dependencies.DepParentChild, env.owner)
	}

	status, body := callTreeRaw(t, env, treeRouteProject, env.owner, "user", false, "rootId="+epicID)
	if status != http.StatusOK {
		t.Fatalf("GET tree: code %d, body %s", status, body)
	}
	items := decodeTreeItems(t, body)

	got := branchProgressOf(t, items, parent)
	if got.Depth != 1 {
		t.Errorf("row %s arrived at depth %v, want 1 — the claim is about a row that the section "+
			"draws as a CHILD, not as a header", parent, got.Depth)
	}
	if got.Closed != 1 || got.Total != 3 || got.Unavailable != 0 {
		t.Errorf("row %s: epicProgress = {closed:%v total:%v unavailable:%v}, "+
			"want {1 3 0}", parent, got.Closed, got.Total, got.Unavailable)
	}
	// sk-489, row 17 over HTTP: the closed child of this fixture is closed by a raw
	// UPDATE, i.e. with the outcome "done" from trigger 000050 — so done is
	// 1 of 3 counted, none dropped.
	if got.Done != 1 || got.NoWork != 0 || got.Counted != 3 {
		t.Errorf("row %s: per-outcome numbers = {done:%v noWork:%v counted:%v}, want {1 0 3}",
			parent, got.Done, got.NoWork, got.Counted)
	}

	// And a leaf has no key at all: "no children" and "nothing closed" are different
	// claims, and a zero triple would say the second instead of the first.
	leafRow := treeRowByItemID(t, items, leaf)
	leafItem, ok := leafRow["item"].(map[string]any)
	if !ok {
		t.Fatalf("row %s: item is not an object", leaf)
	}
	if raw, present := leafItem["epicProgress"]; present && raw != nil {
		t.Errorf("leaf %s carries epicProgress %#v; there must be no key", leaf, raw)
	}
}

// TestTreeBranchProgressHTTP_CrossProjectRouteToo — the same statement on the
// cross-project route. The epic section asks for the CROSS-PROJECT variant
// (project: null), so a fix that reached only the project-scoped handler
// would leave the live surface exactly as broken.
func TestTreeBranchProgressHTTP_CrossProjectRouteToo(t *testing.T) {
	env := newListEnv(t)
	p := env.prefix

	epicID := p + "-1"
	parent := p + "-1.1"
	kid := p + "-1.1.1"
	for _, id := range []string{epicID, parent, kid} {
		seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", "", nil)
	}
	setIssueTypeSQL(t, env.pool, epicID, "epic")
	setIssueStatusSQL(t, env.pool, kid, "closed")
	treeSeedDep(t, env.pool, parent, epicID, dependencies.DepParentChild, env.owner)
	treeSeedDep(t, env.pool, kid, parent, dependencies.DepParentChild, env.owner)

	status, body := callTreeRaw(t, env, treeRouteAll, env.owner, "user", false, "rootId="+epicID)
	if status != http.StatusOK {
		t.Fatalf("GET cross-project tree: code %d, body %s", status, body)
	}
	got := branchProgressOf(t, decodeTreeItems(t, body), parent)
	if got.Closed != 1 || got.Total != 1 || got.Unavailable != 0 {
		t.Errorf("row %s on the cross-project route: {closed:%v total:%v unavailable:%v}, "+
			"want {1 1 0}", parent, got.Closed, got.Total, got.Unavailable)
	}
	if got.Done != 1 || got.NoWork != 0 || got.Counted != 1 {
		t.Errorf("row %s on the cross-project route: per-outcome numbers = "+
			"{done:%v noWork:%v counted:%v}, want {1 0 1}",
			parent, got.Done, got.NoWork, got.Counted)
	}
}

// TestTreeBranchProgressHTTP_ListRouteUnchanged — the guard on the other
// side of the same wire. The list endpoint keeps answering for epics only;
// widening it would silently change every MCP and REST list consumer, and
// /me/watching with them.
func TestTreeBranchProgressHTTP_ListRouteUnchanged(t *testing.T) {
	env := newListEnv(t)
	p := env.prefix

	epicID := p + "-1"
	parent := p + "-1.1"
	kid := p + "-1.1.1"
	for _, id := range []string{epicID, parent, kid} {
		seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", "", nil)
	}
	setIssueTypeSQL(t, env.pool, epicID, "epic")
	treeSeedDep(t, env.pool, parent, epicID, dependencies.DepParentChild, env.owner)
	treeSeedDep(t, env.pool, kid, parent, dependencies.DepParentChild, env.owner)

	res, body := callList(t, env, env.owner, "user", "limit=50")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET list: code %d, body %v", res.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items is not an array: %#v", body["items"])
	}
	found := false
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if row["id"] != parent {
			continue
		}
		found = true
		if prog, present := row["epicProgress"]; present && prog != nil {
			t.Errorf("LIST row %s (issue) carries epicProgress %#v — the list was not supposed "+
				"to be extended: its rows are read by MCP, REST consumers and /me/watching", parent, prog)
		}
	}
	if !found {
		t.Fatalf("list response has no row %s, so nothing was checked: %v", parent, items)
	}
}

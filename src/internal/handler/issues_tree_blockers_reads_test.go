package handler

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// sk-421.2.8 T006, test plan row 21 — read counter (SC-005, INV-4, NFR
// "Perf"). With the includeBlockers flag the extra server work is one
// batch read, whose count depends neither on the number of blocked
// nodes nor on the depth of the blocker chain; without the flag there is
// no extra work.
//
// The tracer is sqlCapture and newTracingPool from
// review_unreviewed_index_plan_test.go. Limit of the sample: it only collects
// statement texts, the counting is built on top. The batch query is recognised by
// the named label issues.TreeBlockersBatchLabel, not by a fragment of the
// blocking-edge predicate: after T003 that one is also in summariesForIDs.

// treeReadCount — how many statements one tree request executed.
type treeReadCount struct {
	total        int // all statements of the request
	batch        int // statements with the blockers batch query label
	rowsBlockers int // response rows with non-empty blockers (for the precondition)
}

func countTreeReads(t *testing.T, capture *sqlCapture, env listTestEnv, root string, includeBlockers bool) treeReadCount {
	t.Helper()
	query := "rootId=" + root
	if includeBlockers {
		query += "&includeBlockers=true"
	}
	capture.reset()
	status, body := callTreeRaw(t, env, treeRouteProject, env.owner, "user", false, query)
	if status != http.StatusOK {
		t.Fatalf("tree %s (includeBlockers=%v): code %d, body %s", root, includeBlockers, status, body)
	}

	capture.mu.Lock()
	stmts := append([]capturedStmt(nil), capture.stmts...)
	capture.mu.Unlock()

	n := treeReadCount{total: len(stmts)}
	for _, s := range stmts {
		if strings.Contains(s.sql, issues.TreeBlockersBatchLabel) {
			n.batch++
		}
	}
	for _, row := range decodeTreeItems(t, body) {
		if bl, ok := row["blockers"].([]any); ok && len(bl) > 0 {
			n.rowsBlockers++
		}
	}
	return n
}

// TestTreeBlockersReads_BatchCountDoesNotGrow — three trees of one project:
// with 1 blocked node, with 40 blocked nodes (each with its own
// blocker) and with blockers blocked by a chain of depth 3. With the flag —
// exactly 1 labelled statement and the same total number of statements on all three;
// without the flag — 0 labelled statements, the total is exactly 1 less than with
// the flag, on the same fixture.
func TestTreeBlockersReads_BatchCountDoesNotGrow(t *testing.T) {
	seedPool := newListTestPool(t)
	owner, _ := seedListUser(t, seedPool, "user")
	pid, prefix := seedListProject(t, seedPool, owner, "tbc")

	pool, capture := newTracingPool(t)
	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: owner, projectID: pid, prefix: prefix}

	counter := 0
	next := func() string {
		counter++
		id := prefix + "-" + strconv.Itoa(counter)
		seedListIssue(t, seedPool, pid, owner, id, "human", "", nil)
		return id
	}
	childOf := func(child, parent string) {
		treeSeedDep(t, seedPool, child, parent, dependencies.DepParentChild, owner)
	}
	blockedBy := func(from, to string) {
		treeSeedDep(t, seedPool, from, to, dependencies.DepBlocks, owner)
	}

	// A tree with one blocked node.
	oneRoot := next()
	oneChild, oneBlocker := next(), next()
	childOf(oneChild, oneRoot)
	blockedBy(oneChild, oneBlocker)

	// A tree with 40 blocked nodes, each with its own blocker.
	const many = 40
	manyRoot := next()
	for i := 0; i < many; i++ {
		child, blocker := next(), next()
		childOf(child, manyRoot)
		blockedBy(child, blocker)
	}

	// A tree where the blocker is itself blocked by a chain of depth 3.
	chainRoot := next()
	chainChild, k1, k2, k3 := next(), next(), next(), next()
	childOf(chainChild, chainRoot)
	blockedBy(chainChild, k1)
	blockedBy(k1, k2)
	blockedBy(k2, k3)

	trees := []struct {
		name    string
		root    string
		blocked int
	}{
		{"1 blocked node", oneRoot, 1},
		{strconv.Itoa(many) + " blocked nodes", manyRoot, many},
		{"blockers in a chain of depth 3", chainRoot, 1},
	}

	with := map[string]treeReadCount{}
	without := map[string]treeReadCount{}
	for _, tr := range trees {
		w := countTreeReads(t, capture, env, tr.root, true)
		wo := countTreeReads(t, capture, env, tr.root, false)
		with[tr.name], without[tr.name] = w, wo
		t.Logf("%s: with the flag %d statements (%d labelled), without the flag %d (%d labelled)",
			tr.name, w.total, w.batch, wo.total, wo.batch)

		if w.rowsBlockers != tr.blocked {
			t.Fatalf("%s: precondition — rows with blockers %d, want %d", tr.name, w.rowsBlockers, tr.blocked)
		}
		if w.batch != 1 {
			t.Errorf("%s: with the flag labelled statements %d, want exactly 1 (INV-4)", tr.name, w.batch)
		}
		if wo.batch != 0 {
			t.Errorf("%s: without the flag labelled statements %d, want 0", tr.name, wo.batch)
		}
		if w.total-wo.total != 1 {
			t.Errorf("%s: with the flag %d statements, without — %d; difference %d, want exactly 1 — the flag "+
				"adds one read and nothing more", tr.name, w.total, wo.total, w.total-wo.total)
		}
	}

	ref := trees[0].name
	for _, tr := range trees[1:] {
		if with[tr.name].total != with[ref].total {
			t.Errorf("with the flag: \"%s\" — %d statements, \"%s\" — %d; the number of reads depends on the tree (SC-005)",
				tr.name, with[tr.name].total, ref, with[ref].total)
		}
		if without[tr.name].total != without[ref].total {
			t.Errorf("without the flag: \"%s\" — %d statements, \"%s\" — %d", tr.name, without[tr.name].total, ref, without[ref].total)
		}
	}
}

package nudges

// index_plan_test.go — sk-303.14, task T020, matrix row 86.
//
// "The actual unaccepted selection uses the index, not a scan, at a
// representative volume".
//
// # Why the query is not assembled by hand
//
// It is taken from the worker itself: selectDueSQL is the same function
// SelectDue uses, with the same parameters. A query assembled by the test as
// "roughly the same" would prove the index applies to a fictional selection,
// and the real one is glued from the set fragment, the join to the addressee,
// the join to the rate-limit state and the unsubscribe check — and any of
// the pieces is capable of sending the planner elsewhere.
//
// # Why volume is required and why this volume
//
// On an empty table the planner picks a sequential scan regardless of
// indexes, and the test would prove the opposite of what it is written for.
// The fixture's form repeats the form of a real database on the selectivity
// axis that makes the partial index meaningful: unaccepted issues are a
// handful, issues in the database are thousands. A fixture where a noticeable
// share of the table lands in the set makes the planner indifferent — it
// picks any suitable index, and the test declares a coincidence as proof.
// This was already observed on the neighbouring class (T014, header of
// migration 000046).
//
// # What exactly is asserted
//
// The node that reads the issues table must be a read BY THE INDEX of
// migration 000047. The assertion is addressed: saying "there is no Seq Scan
// anywhere in the plan" is not allowed — the fragment's subqueries
// (addressee resolvability, settings, project membership) read their tables
// their own way and have no relation to this gate. Row order is not needed
// by the selection, so a bitmap access by this same index is accepted: the
// aggregation by addressee sorts anyway.
//
// # Where the plan is asked (sk-718)
//
// Against a twin schema holding only this test's rows
// (testsupport.NewPlanShadow), not against the shared tables. The partial
// index is meaningful only while its rows are a handful against the table —
// and in the shared database the packages that ran before leave their own
// open assigned issues in it and their users beside them: after a full run
// the planner's honest answer for public was a sequential scan, while the
// fixture alone was still read by the index. The gate judged foreign rows
// (§4a). The assertion is unchanged; the node must also read the twin
// (EXPLAIN VERBOSE carries the schema), so a fall-back to public cannot pass
// for the fixture. What public looks like is logged beside, as a diagnostic
// only.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// unacceptedIndexName — the index of migration 000047.
const unacceptedIndexName = "idx_issues_unaccepted_assignee"

// unreviewedIndexName — the index of migration 000046.
//
// ⚠ The second gate was introduced by repair A/8. The previous header
// claimed that "the plans of BOTH selections go Index Scan on their
// indexes", and there was ONE gate in the tree — on unaccepted. The
// existing handler/review_unreviewed_index_plan_test.go covers the HANDLER
// queries (selection and counter), and the worker's selection has a
// different form: a join to the addressee, a join to the rate-limit state,
// a deadline predicate and an aggregation. Both critics independently
// checked that the claim is true today — but a change to the selection
// would drop the plan in silence.
const unreviewedIndexName = "idx_issues_unreviewed_creator"

// planVolume — the fixture volume. Not "a bit more for solidity": on a
// dozen rows a sequential scan is cheaper than any index, and the planner
// is right, the test is not.
const planVolume = 5000

// unacceptedInPlanFixture — how many issues land in the set. A handful
// against thousands — the very proportion the index is partial for.
const unacceptedInPlanFixture = 3

type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Schema       string     `json:"Schema"`
	Alias        string     `json:"Alias"`
	IndexName    string     `json:"Index Name"`
	Plans        []planNode `json:"Plans"`
}

// explainOnShadow asks the plan of sql against a twin schema holding only
// this env's rows — its project, the project's members and issues, the
// users they refer to, the project's space with those users' memberships,
// and the instance settings — and returns the plan roots, the twin's name
// and the raw plan for failure messages. See the header «Where the plan is
// asked».
func (e *env) explainOnShadow(t *testing.T, sql string, args []any) ([]planNode, string, string) {
	t.Helper()
	sh := testsupport.NewPlanShadow(t, e.pool, []testsupport.ShadowRows{
		{Table: "projects", Where: `WHERE id = $1`, Args: []any{e.project}},
		{Table: "project_members", Where: `WHERE project_id = $1`, Args: []any{e.project}},
		{Table: "issues", Where: `WHERE project_id = $1`, Args: []any{e.project}},
		{Table: "users", Where: `WHERE id IN (SELECT created_by FROM {shadow}.issues)
   OR id IN (SELECT user_id FROM {shadow}.project_members)`},
		{Table: "spaces", Where: `WHERE id IN (SELECT space_id FROM {shadow}.projects)`},
		{Table: "space_members", Where: `WHERE space_id IN (SELECT id FROM {shadow}.spaces)
   AND user_id IN (SELECT id FROM {shadow}.users)`},
		{Table: "instance_settings", Where: `WHERE true`},
	})
	var raw []byte
	if err := sh.Conn.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON, VERBOSE) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN on the twin: %v\nSQL: %s", err, sql)
	}
	return parsePlan(t, raw), sh.Schema, string(raw)
}

// parsePlan decodes an EXPLAIN (FORMAT JSON) answer into its roots.
func parsePlan(t *testing.T, raw []byte) []planNode {
	t.Helper()
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the plan: %v\n%s", err, raw)
	}
	roots := make([]planNode, 0, len(doc))
	for _, d := range doc {
		roots = append(roots, d.Plan)
	}
	return roots
}

// logPublicPicture logs — a diagnostic, never an assertion — how many rows
// of the shared issues table outside this env's project satisfy the partial
// index's predicate, and how public's plan reads issues: the picture that
// made the gate judge foreign rows before sk-718.
func (e *env) logPublicPicture(t *testing.T, label, indexPredicate, sql string, args []any) {
	t.Helper()
	var foreign, issuesTotal, usersTotal int64
	if err := e.pool.QueryRow(t.Context(), `
SELECT (SELECT count(*) FROM issues WHERE project_id <> $1 AND `+indexPredicate+`),
       (SELECT count(*) FROM issues), (SELECT count(*) FROM users)`, e.project).Scan(&foreign, &issuesTotal, &usersTotal); err != nil {
		t.Logf("%s: public picture unreadable: %v", label, err)
		return
	}
	var raw []byte
	if err := e.pool.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Logf("%s: public plan unreadable: %v", label, err)
		return
	}
	var reads []string
	for _, n := range issueScans(parsePlan(t, raw)) {
		reads = append(reads, n.NodeType+" "+strings.Join(indexNamesIn(n), ","))
	}
	t.Logf("%s, public (diagnostic): foreign rows under the index predicate %d, issues %d, users %d; public reads issues by %v",
		label, foreign, issuesTotal, usersTotal, reads)
}

// issueScans collects the nodes that read the issues table.
func issueScans(nodes []planNode) []planNode {
	var out []planNode
	var walk func([]planNode)
	walk = func(ns []planNode) {
		for _, n := range ns {
			if n.RelationName == "issues" {
				out = append(out, n)
			}
			walk(n.Plans)
		}
	}
	walk(nodes)
	return out
}

// indexNamesIn collects the index names of a node and its subtree: on a
// Bitmap Heap Scan the name sits not on it but on the child Bitmap Index
// Scan.
func indexNamesIn(n planNode) []string {
	var out []string
	if n.IndexName != "" {
		out = append(out, n.IndexName)
	}
	for _, c := range n.Plans {
		out = append(out, indexNamesIn(c)...)
	}
	return out
}

// settleIssuesPhysicalState brings the issues table and its indexes to the
// physical state of the rows that are live right now, then gathers the
// statistics the planner reads.
//
// # Why the fixture rows alone are not the proportion the planner sees
//
// The planner prices an access path by PAGES, not by live rows: a sequential
// scan by the heap's relpages, a read of a partial index by the index's
// relpages. The database is shared by the whole -p 1 run, and the packages
// before this one insert and delete issues by the thousand. Deleted rows
// leave two kinds of physical trace, and they age differently:
//
//   - the heap: autovacuum cuts empty trailing pages off, so its size falls
//     back towards the live rows whenever autovacuum happens to run;
//   - a btree: emptied pages are only recycled for reuse, the file never
//     shrinks (PostgreSQL 18 docs, 24.2 "Routine Reindexing").
//
// The unaccepted selection reads its partial index WITHOUT an index
// condition — the join to users is an OR of three keys — so its cost grows
// with the index's page count, not with the three rows in it. Observed in
// the full run (sk-726): heap 195 pages, idx_issues_unaccepted_assignee 89
// pages over 3 live entries — Seq Scan cost 259.89 against Index Scan
// 360.21, the planner rightly takes the scan, and the gate went red on a
// proportion the fixture never set. With the heap still bloated (18 726
// pages, autovacuum had not yet run) the same fixture was green: the
// verdict hung on autovacuum timing in OTHER packages. CI release 0.79.0
// (pipeline 48088) failed that way three times in a row.
//
// VACUUM FULL rewrites the heap and rebuilds every index of the table from
// the live rows, so the page counts are those of the fixture's proportion
// (plus whatever live rows others own) and nothing of the run's history.
// Nothing is relaxed: the query, the index and the assertion are the same;
// the planner sees the proportion the test states. It runs only here: the
// package runs alone under -p 1, and this test is not parallel, so the
// ACCESS EXCLUSIVE lock it takes waits on nobody else's work.
func settleIssuesPhysicalState(ctx context.Context, t *testing.T, e *env) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, `VACUUM FULL issues`); err != nil {
		t.Fatalf("VACUUM FULL issues: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `ANALYZE issues`); err != nil {
		t.Fatalf("ANALYZE issues: %v", err)
	}
}

// TestUnacceptedSelection_UsesTheIndexOnRepresentativeVolume — matrix row
// 86.
func TestUnacceptedSelection_UsesTheIndexOnRepresentativeVolume(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	_, workerName := e.user(t)

	// The bulk: accepted and closed issues. They do not enter the set and
	// do not land in the partial index at all.
	if _, err := e.pool.Exec(ctx, `
INSERT INTO issues (id, project_id, version, title, description, type, priority, status,
                    source, assignee, created_by, created_at, updated_at,
                    closed_at, accepted_at, assignee_set_at)
SELECT $1 || '-vol' || g, $2, 1, 'v' || g, '', 'task', 2, 'closed', 'human', $3, $4,
       now(), now(), now(), now(), now()
  FROM generate_series(1, $5) g`,
		e.prefix, e.project, workerName, author, planVolume); err != nil {
		t.Fatalf("volume fixture: %v", err)
	}
	// A handful — the set itself.
	base := ago(2 * time.Hour)
	for i := range unacceptedInPlanFixture {
		e.seed(t, issue{suffix: "plan" + strconv.Itoa(i), status: "open",
			assignee: workerName, assigneeSetAt: base, createdBy: author})
	}

	// Statistics are required: without them the planner considers the
	// table empty and picks a scan regardless of indexes. The physical
	// state of issues is required too — see settleIssuesPhysicalState. The
	// statistics are updated for EVERY table the selection reads, not only
	// issues: the join order with the addressee is chosen from the
	// statistics of both sides, and those of users the fixture just filled
	// are whatever the shared database last left there — the plan would
	// depend on whether autovacuum happened to pass (§4a). Returning them is
	// also required — the database is shared, and stale statistics after
	// dropping thousands of rows are other people's unpredictable plans.
	settleIssuesPhysicalState(ctx, t, e)
	const analyzeRead = `ANALYZE users, reminder_rate_limit, user_notification_reason_prefs`
	if _, err := e.pool.Exec(ctx, analyzeRead); err != nil {
		t.Fatalf("%s: %v", analyzeRead, err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM issues WHERE project_id = $1`, e.project)
		_, _ = e.pool.Exec(context.Background(), `ANALYZE issues`)
		_, _ = e.pool.Exec(context.Background(), analyzeRead)
	})

	// The query is the REAL one, from the worker itself.
	var args []any
	bind := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	sql, err := selectDueSQL(bind, review.CategoryUnaccepted, time.Now().UTC(),
		DefaultBatchSize, uuid.Nil)
	if err != nil {
		t.Fatalf("assembling the selection: %v", err)
	}

	e.logPublicPicture(t, "row 86",
		`status NOT IN ('closed', 'standing') AND accepted_at IS NULL AND assignee_set_at IS NOT NULL`, sql, args)
	roots, twin, plan := e.explainOnShadow(t, sql, args)

	scans := issueScans(roots)
	if len(scans) == 0 {
		t.Fatalf("row 86: the plan has no issues-reading node — there is nothing "+
			"to check the assertion against\nSQL: %s\nPlan: %s", sql, plan)
	}
	for _, n := range scans {
		if n.Schema != twin {
			t.Errorf("row 86: issues is read from schema %q, not the twin %q — the plan "+
				"judged the shared table, not the fixture\nPlan: %s", n.Schema, twin, plan)
			continue
		}
		if n.NodeType == "Seq Scan" {
			t.Errorf("row 86: issues is read by a sequential scan. At this volume "+
				"that means the index form did not match the selection form, and "+
				"the index is what must be fixed — it has not shipped yet\nSQL: %s\nPlan: %s", sql, plan)
			continue
		}
		names := indexNamesIn(n)
		hit := false
		for _, name := range names {
			if name == unacceptedIndexName {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("row 86: issues is read by node %q on indexes %v — among them "+
				"there is no %s from migration 000047. The index introduced for "+
				"THIS selection was not chosen by the planner\nSQL: %s\nPlan: %s",
				n.NodeType, names, unacceptedIndexName, sql, plan)
			continue
		}
		t.Logf("row 86: %s on %v", n.NodeType, names)
	}
}

// TestUnreviewedSelection_UsesTheIndexOnRepresentativeVolume — the second
// gate, introduced by repair A/8.
//
// The assertion is the same and for the same reason as for unaccepted, but
// about a DIFFERENT query: the unreviewed selection of the background
// worker joins the addressee on created_by, resolves the admin flag IN THE
// QUERY and aggregates by addressee — a form that neither the handler
// selection nor the counter has, and that therefore is not covered by
// handler/review_unreviewed_index_plan_test.go.
//
// The query is taken from the worker itself (selectDueSQL), not assembled
// by hand.
func TestUnreviewedSelection_UsesTheIndexOnRepresentativeVolume(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	_, workerName := e.user(t)

	// The bulk: closed issues WITHOUT the review flag. They do not enter
	// the set and do not land in the partial index at all — the same
	// "handful against thousands" proportion the index is partial for.
	if _, err := e.pool.Exec(ctx, `
INSERT INTO issues (id, project_id, version, title, description, type, priority, status,
                    source, assignee, created_by, created_at, updated_at,
                    closed_at, review_required)
SELECT $1 || '-uvol' || g, $2, 1, 'v' || g, '', 'task', 2, 'closed', 'human', $3, $4,
       now(), now(), now() - interval '2 days', false
  FROM generate_series(1, $5) g`,
		e.prefix, e.project, workerName, author, planVolume); err != nil {
		t.Fatalf("volume fixture: %v", err)
	}
	for i := range unacceptedInPlanFixture {
		id := e.seed(t, issue{suffix: "uplan" + strconv.Itoa(i), status: "closed",
			assignee: workerName, createdBy: author, reviewRequired: true})
		// The unreviewed countdown base is the close moment; it must sit
		// past the first-mark lower bound, otherwise the selection would
		// legitimately cut the row and the plan would be built on an
		// empty result.
		if _, err := e.pool.Exec(ctx,
			`UPDATE issues SET closed_at = now() - interval '2 days' WHERE id = $1`, id); err != nil {
			t.Fatalf("shifting the close moment: %v", err)
		}
	}

	// The same physical settling as for unaccepted: the run's history must
	// not decide the plan of this gate either.
	settleIssuesPhysicalState(ctx, t, e)
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM issues WHERE project_id = $1`, e.project)
		_, _ = e.pool.Exec(context.Background(), `ANALYZE issues`)
	})

	var args []any
	bind := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	sql, err := selectDueSQL(bind, review.CategoryUnreviewed, time.Now().UTC(),
		DefaultBatchSize, uuid.Nil)
	if err != nil {
		t.Fatalf("assembling the selection: %v", err)
	}

	e.logPublicPicture(t, "unreviewed",
		`status = 'closed' AND review_required AND reviewed_at IS NULL`, sql, args)
	roots, twin, plan := e.explainOnShadow(t, sql, args)

	scans := issueScans(roots)
	if len(scans) == 0 {
		t.Fatalf("the plan has no issues-reading node — there is nothing to check "+
			"the assertion against\nSQL: %s\nPlan: %s", sql, plan)
	}
	for _, n := range scans {
		if n.Schema != twin {
			t.Errorf("unreviewed: issues is read from schema %q, not the twin %q — the "+
				"plan judged the shared table, not the fixture\nPlan: %s", n.Schema, twin, plan)
			continue
		}
		if n.NodeType == "Seq Scan" {
			t.Errorf("issues is read by a sequential scan. At this volume that "+
				"means the form of index 000046 did not match the worker's "+
				"selection form — and the previous header claimed that the plans "+
				"of BOTH selections go on their indexes\nSQL: %s\nPlan: %s", sql, plan)
			continue
		}
		names := indexNamesIn(n)
		hit := false
		for _, name := range names {
			if name == unreviewedIndexName {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("issues is read by node %q on indexes %v — among them there is "+
				"no %s from migration 000046\nSQL: %s\nPlan: %s",
				n.NodeType, names, unreviewedIndexName, sql, plan)
			continue
		}
		t.Logf("unreviewed: %s on %v", n.NodeType, names)
	}
}

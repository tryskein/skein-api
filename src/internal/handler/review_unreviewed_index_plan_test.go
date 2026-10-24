package handler

// review_unreviewed_index_plan_test.go — sk-303.14, task T014,
// matrix row 61.
//
// "Unreviewed selections use indexes, not a scan, at a representative
// volume".
//
// # Why not EXPLAIN of a rewritten query
//
// The temptation is this: assemble "roughly the same" SQL by hand and
// look at its plan. That would prove the index applies to a MADE-UP
// query. The real queries are assembled inside the store from a dozen
// pieces — joins with users, the epic-progress projection, derived
// blockedness, keyset paging — and any of them can lead the planner
// away.
//
// So the queries taken are REAL: a pgx observer (QueryTracer) is hung
// on the connection, the real set readers are called, and the plan is
// built from the intercepted text with the same parameters. There is
// not a single hand-assembled SQL in this file.
//
// # Why not index-access counters
//
// pg_stat_user_indexes looks simpler, but it is updated asynchronously
// and with a delay, so it would require a wait with a timeout — that is,
// a flaky test. The plan is deterministic.
//
// # Why volume is mandatory
//
// On an empty table the planner picks a sequential scan regardless of
// indexes, and the test would prove the opposite of what it is written
// for. So the fixture seeds a real volume and updates statistics.
//
// # What exactly is asserted
//
// The node that reads the OUTER issues table (alias `i`) must be a read
// by the index of migration 000046. The assertion is addressed: saying
// "there is no Seq Scan anywhere in the plan" is not allowed — nested
// subqueries (epic progress, blockedness) read issues their own way and
// have no relation to this gate.
//
// For the TWO LIST queries the assertion is stronger, and it is exactly
// this strong assertion the migration header uses to justify the index
// column order `(created_by, updated_at DESC, id DESC)`: the read must
// be ORDERED (Index Scan / Index Only Scan on this index specifically,
// not a bitmap access), and there must be no sort sitting above it. A
// weak check "the index name occurred somewhere in the subtree" does
// not give this: on a Bitmap Heap Scan the name sits on the child
// Bitmap Index Scan, and a bitmap read with a separate sort would pass
// it through — that is, the gate would stay green exactly on the loss
// of the property the index is ordered for.
//
// The second-number counter and the lists' companion-COUNTs do not need
// order: it is enough for them that the index was chosen. So the strong
// part is applied not to every intercepted query, but to those that
// order the result with a keyset-paging pair.
//
// # WHERE each of the two parts is checked — sk-348
//
// The parts are laid out by what the test ACTUALLY controls, and are
// checked in different places.
//
// The WEAK PART ("the index fitted the query") — on the REAL table.
// This is a property of the pair "query and index": the filter is by a
// freshly created author, and how many foreign rows sit beside it does
// not affect the index's applicability.
//
// The STRONG PART ("the read is ordered, there is no sort above it") —
// on an ISOLATED SNAPSHOT of its own fixture (see newFixtureShadow). On
// the shared database it is unprovable, and that is MEASURED, not
// assumed.
//
// What exactly was measured (2026-09-03, this task, a separate Postgres,
// the fixture below unchanged). What loses order is not the issues read,
// it is the JOIN WITH users on `LOWER(ua.username) = LOWER(i.assignee)`
// — the very join index 000046 has no relation to. The planner picks
// the form of this join by the volume of the users table, and neighbouring
// packages fill that table:
//
//	users in snapshot   form of the join with users         sort above the read
//	2                   Nested Loop + Seq Scan              no
//	20                  Nested Loop + Seq Scan              no
//	100                 Hash Right Join (issues in hash)    YES
//	500                 Nested Loop + Index Scan            no
//	5000                Nested Loop + Index Scan            no
//
// The dependence is NOT monotonic: the pit is in the middle — where a
// full walk of users is already expensive for a nested loop, but still
// cheap for a hash. The shared run database falls into this pit (149
// users were observed), and that is exactly why the gate was green in
// a standalone run and red in a full one.
//
// Growing the unreviewed set does NOT close the pit: at 1012 unreviewed
// issues of the caller the sort is still there (the planner's estimate
// stays rows=3 — there are too many predicates it cannot estimate, so
// sorting "three rows" is free for it).
//
// Hence the conclusion written here: on the shared database "there is
// no sort" is a claim not about the index, but about the size of a
// FOREIGN table. Forcing the desired plan with switches (enable_sort
// and the like) would mean proving the plan is coerced, not chosen —
// that is, the opposite of the gate's meaning. Deleting foreign rows
// would bind the test to nobody else writing foreign ones. So the
// strong part is asked where the WHOLE circle of tables belongs to the
// test.
//
// ⚠ The header of migration 000046 asserts the absence of a sort without
// this caveat. The migration is already applied and is not edited; the
// divergence is recorded here and in task sk-348. The property the
// header uses to justify the KEY COLUMN ORDER is preserved in full by
// the check below: break the index column order — the snapshot reddens.
//
// # Does this mean the "one index, not two" decision is revised — sk-303.19
//
// No. A second index on `issues` with leading `project_id` would not
// have closed this pit — it is not even the plan node where order is
// lost. The pit arises on the JOIN with `users`, to which both
// hypothetical indexes on `issues` equally have no relation: the index
// determines the form of reading the OUTER issues table, not the form
// of the join with users further down the plan. Adding a second index
// means doubling the write cost (see the 000046 header) and not moving
// a single row of the "form of the join with users" table above. The
// "one index" rationale in 000046 remains in force; what was wrong was
// only the wording about the absence of a sort WITHOUT THE CAVEAT about
// the volume of users, not the choice of the number of indexes itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
)

// unreviewedIndexNames are the indexes of migration 000046. A read of
// the outer issues in unreviewed selections must go by one of them.
//
// A set, not one name: the migration started with two indexes, and it
// is this test that reduced them to one, showing that the planner does
// not pick the second on any of the paths. The form is left so that
// bringing a second index back is a data edit.
var unreviewedIndexNames = map[string]bool{
	"idx_issues_unreviewed_creator": true,
}

// representativeIssueCount is the fixture volume. Not "more for gravitas":
// on a dozen rows a sequential scan is cheaper than any index, and the
// planner is right, the test is not.
const representativeIssueCount = 5000

// ---------------------------------------------------------------------------
// Observer of the real queries
// ---------------------------------------------------------------------------

type capturedStmt struct {
	sql  string
	args []any
}

// sqlCapture is a pgx.QueryTracer: it intercepts EVERY query of the
// connection together with its parameters.
type sqlCapture struct {
	mu    sync.Mutex
	stmts []capturedStmt
}

func (c *sqlCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, capturedStmt{sql: data.SQL, args: data.Args})
	return ctx
}

func (c *sqlCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *sqlCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = nil
}

// unreviewedOnes selects intercepted queries that read the unreviewed
// set. The marker is the index's partial-index predicate in the text:
// it is on exactly the fragment's queries and on none of the neighbours.
func (c *sqlCapture) unreviewedOnes() []capturedStmt {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedStmt
	for _, s := range c.stmts {
		if strings.Contains(s.sql, "review_required") && strings.Contains(s.sql, "reviewed_at IS NULL") {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Plan parse
// ---------------------------------------------------------------------------

// planNode is the minimal form of a plan node: name, relation, alias, index.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Schema       string     `json:"Schema"`
	Alias        string     `json:"Alias"`
	IndexName    string     `json:"Index Name"`
	Plans        []planNode `json:"Plans"`
}

// scanSite is the node that reads the outer issues together with the
// chain of its ancestors from the plan root.
//
// Ancestors are needed for the assertion "there is no sort above the
// read". Searching for Sort across the whole plan is not allowed: both
// list queries carry a correlated subquery `ORDER BY d.created_at LIMIT 1`
// (the current parent) in the projection, and it yields its OWN Sort
// node — a neighbour, not one stacked above. Forbidding any Sort in the
// plan would redden a correct plan and force the query to be distorted.
type scanSite struct {
	node      planNode
	ancestors []planNode
}

// outerIssueScans collects nodes that read the outer issues table (alias
// `i`) — the ones whose form migration 000046 sets — together with their
// ancestors.
func outerIssueScans(nodes []planNode) []scanSite {
	var out []scanSite
	var walk func([]planNode, []planNode)
	walk = func(ns []planNode, chain []planNode) {
		for _, n := range ns {
			if n.RelationName == "issues" && n.Alias == "i" {
				out = append(out, scanSite{node: n, ancestors: chain})
			}
			walk(n.Plans, append(append([]planNode(nil), chain...), n))
		}
	}
	walk(nodes, nil)
	return out
}

// orderedScanTypes are nodes that yield rows in index order. Bitmap
// access is absent here on purpose: it reads the index but loses order.
var orderedScanTypes = map[string]bool{
	"Index Scan":      true,
	"Index Only Scan": true,
}

// sortNodeTypes are nodes that sort the result.
var sortNodeTypes = map[string]bool{
	"Sort":             true,
	"Incremental Sort": true,
}

// keysetOrderBy is the tail of both list queries of the set.
//
// The marker is "this query orders the result", not "ORDER BY occurs
// somewhere in the text": the current-parent subquery has its own ORDER
// BY and is in every list's projection. The match is textual and therefore
// fragile ON PURPOSE: if order in the store changes, the gate will not
// silently weaken, it will fall on the check below ("not a single ordered
// query"), and it will have to be brought in line together with the
// migration header.
const keysetOrderBy = "ORDER BY i.updated_at DESC, i.id DESC"

// ordersResult — the query yields the result in keyset-paging order.
func ordersResult(st capturedStmt) bool {
	return strings.Contains(st.sql, keysetOrderBy)
}

// planTarget is what the plan is built AGAINST: the real database (the
// pool) or an isolated snapshot of the fixture (its own connection with
// a rewritten search_path). The interface is reduced to one method on
// purpose — from it you can see that the difference between the two
// checks is exactly one: the circle of tables the planner sees. Neither
// *pgxpool.Pool nor *pgx.Conn is rewritten for this.
type planTarget interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// explain builds the plan of the intercepted query with ITS OWN parameters.
//
// VERBOSE is on for one key — "Schema" on the read node. Without it the
// snapshot would be unprovable: a search_path that failed for any reason
// would silently return the plan of the real table, and the strong part
// would check exactly what the split is meant to leave. With it a miss
// is visible (see assertOrderedRead).
func explain(t *testing.T, target planTarget, st capturedStmt) []planNode {
	t.Helper()
	var raw []byte
	if err := target.QueryRow(t.Context(),
		"EXPLAIN (FORMAT JSON, VERBOSE) "+st.sql, st.args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v\nSQL: %s", err, st.sql)
	}
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("plan parse: %v\n%s", err, raw)
	}
	out := make([]planNode, 0, len(doc))
	for _, d := range doc {
		out = append(out, d.Plan)
	}
	return out
}

// indexNamesIn collects index names of the node and of its whole subtree.
//
// The subtree is walked not for completeness, but because of how the
// plan is built: on a Bitmap Heap Scan the index name sits NOT on it,
// but on the child Bitmap Index Scan — itself it carries only the
// relation and the alias. Looking only at the node would mean declaring
// any bitmap access a scan.
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

// assertUsesUnreviewedIndex is the WEAK part of the row-61 gate for one
// query: the outer issues read goes by the index of migration 000046,
// not by a scan.
//
// Asked of ALL intercepted queries and ON THE REAL TABLE. Applicability
// of the index to the query is a property of the pair "query and index":
// the filter is by a freshly created author, and foreign rows of
// neighbouring packages do not affect it.
func assertUsesUnreviewedIndex(t *testing.T, target planTarget, surface string,
	st capturedStmt,
) {
	t.Helper()
	for _, site := range outerIssueSites(t, target, surface, st) {
		n := site.node
		if n.NodeType == "Seq Scan" {
			t.Errorf("%s: outer issues is read by a sequential scan. At this "+
				"volume that means the index shape did not fit the query shape, and the "+
				"index is what must be fixed — it is not shipped yet\nSQL: %s", surface, st.sql)
			continue
		}
		names := indexNamesIn(n)
		hit := false
		for _, name := range names {
			if unreviewedIndexNames[name] {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("%s: outer issues is read by node %q on indexes %v — none of "+
				"migration 000046. The planner did not pick the index introduced for "+
				"this query\nSQL: %s", surface, n.NodeType, names, st.sql)
			continue
		}
		t.Logf("%s: %s on %v", surface, n.NodeType, names)
	}
}

// assertOrderedRead is the STRONG part of the gate: the outer issues
// read must be ORDERED (Index Scan / Index Only Scan on the 000046 index
// specifically, not bitmap access) and with no sort above it. It is
// exactly this that the migration header uses to justify the key column
// order `(created_by, updated_at DESC, id DESC)`.
//
// Asked only of queries ordered by the keyset-paging pair, and ONLY on
// an isolated snapshot of the fixture (schema is its schema): on the
// shared database the form of the join with users is decided by the
// volume of users, not by this index. Measurements are in the file
// header.
func assertOrderedRead(t *testing.T, target planTarget, schema, surface string,
	st capturedStmt,
) {
	t.Helper()
	for _, site := range outerIssueSites(t, target, surface, st) {
		n := site.node
		// The snapshot must be what it is named. A search_path miss (schema
		// dropped, wrong name, wrong connection) without this check would
		// look like a successful check of the real table — that is, exactly
		// the defect the split is meant to leave.
		if n.Schema != schema {
			t.Fatalf("%s: plan built against schema %q, want isolated snapshot %q — "+
				"the strong part would be checked not where volume is under control\nSQL: %s",
				surface, n.Schema, schema, st.sql)
		}
		if !orderedScanTypes[n.NodeType] {
			t.Errorf("%s: outer issues is read by node %q. The index is used, but "+
				"row order is lost — bitmap access yields rows in page "+
				"order. It is an ordered read that the 000046 header uses to justify "+
				"the key (created_by, updated_at DESC, id DESC) and the refusal of a "+
				"second index\nSQL: %s", surface, n.NodeType, st.sql)
			continue
		}
		if !unreviewedIndexNames[n.IndexName] {
			t.Errorf("%s: the ordered read goes by index %q, not by the index of "+
				"migration 000046 (the subtree had %v). Order is taken not from "+
				"the index it is justified by\nSQL: %s",
				surface, n.IndexName, indexNamesIn(n), st.sql)
			continue
		}
		if bad := sortsAbove(site); len(bad) > 0 {
			t.Errorf("%s: sort nodes %v sit above the outer issues read. "+
				"So the index order is not used: rows are gathered and "+
				"sorted again, and LIMIT stops being a cut from the top — "+
				"exactly the property the 000046 header uses to justify the "+
				"column order\nSQL: %s", surface, bad, st.sql)
			continue
		}
		// The chosen index is printed on purpose: it goes into the plan-check
		// report, and from it you can see that ONE index covers both paths —
		// even though the leading column of the paths differs.
		t.Logf("%s: snapshot — %s on %v, no sort above the read",
			surface, n.NodeType, indexNamesIn(n))
	}
}

// outerIssueSites builds the plan and returns the outer-issues read
// nodes, failing if there is nothing to assert on. The shared entry of
// both parts of the gate.
func outerIssueSites(t *testing.T, target planTarget, surface string, st capturedStmt) []scanSite {
	t.Helper()
	sites := outerIssueScans(explain(t, target, st))
	if len(sites) == 0 {
		t.Fatalf("%s: the plan has no outer-issues read node — there is nothing "+
			"to assert on\nSQL: %s", surface, st.sql)
	}
	return sites
}

// sortsAbove returns the types of sort nodes stacked above the outer
// issues read (ancestors only, not the whole plan — see scanSite).
func sortsAbove(site scanSite) []string {
	var out []string
	for _, a := range site.ancestors {
		if sortNodeTypes[a.NodeType] {
			out = append(out, a.NodeType)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Volume fixture
// ---------------------------------------------------------------------------

// unreviewedPerProject is how many of the caller's issues enter the set
// in each project of the fixture.
//
// The number is small ON PURPOSE, and that is the main parameter of the
// whole check. A person has a handful of unreviewed results, and the
// database has thousands of issues — it is exactly this difference that
// makes a partial index meaningful. A fixture where a noticeable share
// of the table enters the set makes the planner indifferent: it picks
// any fitting index, and the test declares as proof what was actually
// a coincidence. Observed on this very task — at about a quarter share
// the planner went to idx_issues_project_status.
const unreviewedPerProject = 3

// seedRepresentativeVolume fills ONE project and does not touch
// statistics — the caller must update them, once for the whole fixture.
//
// The fixture's shape repeats the real database along TWO axes of
// selectivity, each of which takes part in the set predicate:
//
//   - authorship: the bulk of issues is created by SOMEONE ELSE, the
//     caller — a handful. A fixture where the caller created everything
//     makes `created_by = me` a useless filter;
//   - membership in the set: of the caller's issues only closed ones
//     with the flag on enter the set.
//
// Volume is spread across SEVERAL projects (see the caller). A
// single-project fixture is meaningless: the project path has an
// equality on project_id, and in the only project it coincides in
// selectivity with the filter by author — then the choice between the
// remaining index and the foreign idx_issues_project_status stops
// meaning anything.
func seedRepresentativeVolume(t *testing.T, pool *pgxpool.Pool, pid, filler, caller uuid.UUID,
	prefix, assignee string, count int,
) {
	t.Helper()
	ctx := t.Context()

	// The bulk: open issues of a FOREIGN author without the flag. They
	// do not enter the set and do not enter the partial index.
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata,
                    previous_assignees, dismissed_by,
                    review_required, reviewed_at, reviewed_by)
SELECT $1 || '-' || g, $2, NULL, 1,
       'volume ' || g, '', 'task', 2, 'open', false,
       'human', NULL, $3::varchar,
       '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
       NULL, $4, now(), now(), NULL, NULL, '{}'::jsonb,
       '{}'::uuid[], '{}'::uuid[],
       false, NULL, NULL
  FROM generate_series(100, $5) g`,
		prefix, pid, assignee, filler, count); err != nil {
		t.Fatalf("volume fixture: %v", err)
	}

	// A handful of the caller's issues — the set itself.
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata,
                    previous_assignees, dismissed_by,
                    review_required, reviewed_at, reviewed_by)
SELECT $1 || '-mine' || g, $2, NULL, 1,
       'mine ' || g, '', 'task', 2, 'closed', false,
       'human', NULL, $3::varchar,
       '', '', '', NULL, NULL, NULL, NULL, now(), NULL,
       NULL, $4, now(), now(), NULL, NULL, '{}'::jsonb,
       '{}'::uuid[], '{}'::uuid[],
       true, NULL, NULL
  FROM generate_series(1, $5) g`,
		prefix, pid, assignee, caller, unreviewedPerProject); err != nil {
		t.Fatalf("filling the set: %v", err)
	}
}

// plannedTables — every table the set's queries read: the three readers
// with their companion-COUNTs and the access relation behind the
// cross-project horizon (skein_project_access_set). The fixture seeds
// users, projects and their members along with issues.
var plannedTables = []string{
	"issues", "users", "projects", "project_members", "space_members", "spaces",
	"instance_settings", "dependencies", "issue_labels", "issue_components",
	"user_notification_prefs",
}

// analyzePlannedTables updates statistics of every table the plan reads
// and returns them to the previous shape after the test.
//
// Statistics are mandatory: without them the planner treats the table
// as empty and picks a scan regardless of indexes — the test would
// prove the opposite of what it is written for. ALL read tables, not
// only issues: the join order is chosen from the statistics of every
// side, and those of a table the fixture just filled are whatever the
// shared database last left there — the plan would depend on whether
// autovacuum happened to pass (§4a). The return is also mandatory: the
// shared database is shared across packages, and stale statistics after
// dropping thousands of rows are other people's unpredictable plans.
func analyzePlannedTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	analyze := func(ctx context.Context) error {
		_, err := pool.Exec(ctx, `ANALYZE `+strings.Join(plannedTables, ", "))
		return err
	}
	if err := analyze(t.Context()); err != nil {
		t.Fatalf("ANALYZE %v: %v", plannedTables, err)
	}
	t.Cleanup(func() { _ = analyze(context.Background()) })
}

// ---------------------------------------------------------------------------
// Isolated fixture snapshot (sk-348)
// ---------------------------------------------------------------------------

// fixtureShadow is the snapshot the STRONG part of the gate is asked
// against: the same schema, the same indexes, but ONLY the rows this
// test seeded.
//
// conn is its own connection with a rewritten search_path, not a
// connection from the shared pool. The reason is not neatness: pgx
// caches prepared statements ON THE CONNECTION, and the table name is
// bound to a concrete relation at prepare time. The same EXPLAIN text,
// prepared first against the real table, would come back from the cache
// on the snapshot too — and the snapshot would silently be the real
// table. A separate connection makes that swap impossible, and the
// Schema check in assertOrderedRead catches it if it still happens.
type fixtureShadow struct {
	schema string
	conn   *pgx.Conn
}

// newFixtureShadow builds the snapshot and returns it.
//
// # Why a twin schema, not "clean up before the run"
//
// Foreign rows cannot be deleted: the test would start depending on
// nobody else writing foreign ones — the same disease, only from the
// other side. The snapshot touches nothing foreign: foreign rows stay
// where they are, they are simply not in the other namespace.
//
// # Why ALL public tables are copied, not just issues
//
// Completeness of the circle here is a property of the construction,
// not of a hand-written list. The set's queries read more than issues:
// users (twice), projects, project_members, user_notification_prefs,
// dependencies. A list in the code would go stale on the first store
// edit, and the snapshot would quietly stop being a snapshot — a
// missing table would be read from public down the search_path tail.
// Every table of the schema is copied, so a new table cannot be added
// to the query unnoticed.
//
// Indexes are transferred not by hand-written DDL, but by definitions
// from pg_indexes: names are preserved (an index name is unique within
// a schema), and with them — the planner's ability to pick a FOREIGN
// index. A snapshot where the planner has no alternatives would prove
// not a choice, but the absence of alternatives.
//
// # What lands in the rows
//
// Exactly this test's fixture: its projects, their issues, their
// members and the users those rows refer to. The other tables stay
// empty — the test created nothing in them, and emptiness here is
// honest.
//
// LIKE does not carry foreign keys, and that is deliberate: the
// snapshot is a controlled environment, not a byte-for-byte copy.
// Checked on an empty database (the only state where the real table
// is also under control): the plan shape of the snapshot and of the
// real table matches — an ordered read by the 000046 index with no
// sort above it.
func newFixtureShadow(t *testing.T, pool *pgxpool.Pool, projects []uuid.UUID) *fixtureShadow {
	t.Helper()
	ctx := t.Context()

	schema := "unrev_plan_" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String(), "-", "")[:16]
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("snapshot: creating schema: %v", err)
	}
	t.Cleanup(func() {
		// The test context is already cancelled by this point, so this one
		// is our own — but with a ceiling: cleanup has no right to hang if
		// the connection is stuck.
		//
		// A drop error is REPORTED, not swallowed, and the fact of the
		// schema's disappearance is checked separately. A swallowed error
		// would leave the test green over a leaked schema — that is, green
		// secured by something other than it looks: exactly the disease
		// this whole gate is written against. The schema lives in the
		// shared database, and a leak goes to neighbouring packages.
		//
		// Snapshot cleanup is registered LATER than the pool close, and
		// order here is the reverse of registration — so at this moment
		// the pool is still alive.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("snapshot: schema %s was not dropped: %v. It stayed in the shared database "+
				"and will go to neighbouring packages", schema, err)
			return
		}
		var left bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
			schema).Scan(&left); err != nil {
			t.Errorf("snapshot: drop of schema %s not confirmed: %v", schema, err)
			return
		}
		if left {
			t.Errorf("snapshot: schema %s survived its own DROP — cleanup "+
				"silently did not work", schema)
		}
	})

	tables := queryStrings(t, pool, `
SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if len(tables) == 0 {
		t.Fatalf("snapshot: public had not a single table")
	}
	for _, tb := range tables {
		// INCLUDING CONSTRAINTS is for CHECK constraints: the planner
		// reads them (constraint exclusion), and a snapshot without them
		// would differ from the original. GENERATED is for computed
		// columns: without them copying rows hits "cannot write a
		// non-DEFAULT into a generated column".
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE %s.%s (LIKE public.%s`+
				` INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS)`,
			schema, tb, tb)); err != nil {
			t.Fatalf("snapshot: table %s: %v", tb, err)
		}
	}

	rows, err := pool.Query(ctx, `
SELECT tablename, indexdef FROM pg_indexes WHERE schemaname = 'public'`)
	if err != nil {
		t.Fatalf("snapshot: index list: %v", err)
	}
	type indexDef struct{ table, def string }
	var defs []indexDef
	for rows.Next() {
		var d indexDef
		if err := rows.Scan(&d.table, &d.def); err != nil {
			t.Fatalf("snapshot: reading index: %v", err)
		}
		defs = append(defs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot: index list: %v", err)
	}
	for _, d := range defs {
		on := " ON public." + d.table + " "
		if !strings.Contains(d.def, on) {
			t.Fatalf("snapshot: index definition could not be retargeted: %s", d.def)
		}
		if _, err := pool.Exec(ctx,
			strings.Replace(d.def, on, " ON "+schema+"."+d.table+" ", 1)); err != nil {
			t.Fatalf("snapshot: index %s: %v", d.def, err)
		}
	}

	// Columns are listed explicitly: generated ones cannot be written,
	// and `SELECT *` would silently pick them up.
	writableCols := func(tb string) string {
		cols := queryStrings(t, pool, `
SELECT column_name FROM information_schema.columns
 WHERE table_schema = 'public' AND table_name = $1 AND is_generated = 'NEVER'
 ORDER BY ordinal_position`, tb)
		if len(cols) == 0 {
			t.Fatalf("snapshot: table %s had no writable columns", tb)
		}
		for i, c := range cols {
			cols[i] = `"` + c + `"`
		}
		return strings.Join(cols, ", ")
	}
	copyRows := func(tb, where string, args ...any) {
		cols := writableCols(tb)
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s.%s (%s) SELECT %s FROM public.%s %s`,
			schema, tb, cols, cols, tb, where), args...); err != nil {
			t.Fatalf("snapshot: copying %s: %v", tb, err)
		}
	}
	copyRows("projects", `WHERE id = ANY($1)`, projects)
	copyRows("project_members", `WHERE project_id = ANY($1)`, projects)
	copyRows("issues", `WHERE project_id = ANY($1)`, projects)
	// Users are those the copied rows refer to: the issues' author and
	// the project members. The set's join goes through users twice, so
	// their volume is part of the snapshot, not a trifle.
	copyRows("users", fmt.Sprintf(`u WHERE EXISTS (
    SELECT 1 FROM %s.issues i WHERE i.created_by = u.id
) OR EXISTS (
    SELECT 1 FROM %s.project_members pm WHERE pm.user_id = u.id
)`, schema, schema))

	for _, tb := range tables {
		if _, err := pool.Exec(ctx, `ANALYZE `+schema+`.`+tb); err != nil {
			t.Fatalf("snapshot: ANALYZE %s: %v", tb, err)
		}
	}

	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("snapshot: connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	// public stays as the tail for extension types and operators (vector,
	// pg_trgm live in public). It cannot substitute a table: every public
	// table is already in the snapshot, and the first of them wins.
	if _, err := conn.Exec(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatalf("snapshot: search_path: %v", err)
	}
	return &fixtureShadow{schema: schema, conn: conn}
}

// queryStrings is a one-column list of strings.
func queryStrings(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("list query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("reading list: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list query: %v", err)
	}
	return out
}

// newTracingPool opens a pool with a query observer.
func newTracingPool(t *testing.T) (*pgxpool.Pool, *sqlCapture) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	capture := &sqlCapture{}
	cfg.ConnConfig.Tracer = capture
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, capture
}

// ---------------------------------------------------------------------------
// The assertion
// ---------------------------------------------------------------------------

// TestUnreviewed_ReadsUseTheIndexes — row 61.
//
// Three readers of the set, their real queries (the lists also go with
// their companion-COUNT), their plans.
func TestUnreviewed_ReadsUseTheIndexes(t *testing.T) {
	pool, capture := newTracingPool(t)

	caller, callerName := seedIMUser(t, pool, "user")
	worker, workerName := seedIMUser(t, pool, "user")

	// Volume is spread across several projects of ONE author. A
	// single-project fixture would prove nothing: on the project path the
	// equality on project_id would coincide in selectivity with the
	// filter by author, and the choice between the 000046 index and the
	// foreign idx_issues_project_status would become indifferent. The
	// cross-project path must at the same time see ALL projects —
	// otherwise its horizon degenerates into one uuid, that is into the
	// same equality on project_id that this path does not have in the
	// product.
	const projectCount = 4
	var (
		pid     uuid.UUID
		visible []uuid.UUID
	)
	for p := range projectCount {
		projectID, projectPrefix := seedIMProject(t, pool, caller, worker)
		seedRepresentativeVolume(t, pool, projectID, worker, caller, projectPrefix, workerName,
			representativeIssueCount/projectCount)
		visible = append(visible, projectID)
		if p == 0 {
			pid = projectID
		}
	}
	analyzePlannedTables(t, pool)

	// A snapshot of its own fixture is the arena of the STRONG part of
	// the gate. Built after ANALYZE of the real table and before the
	// first read: the snapshot's contents must match what the real
	// readers saw.
	shadow := newFixtureShadow(t, pool, visible)

	store := issues.NewStore(pool)
	ctx := t.Context()

	// wantOrdered — whether the reader must yield at least one query
	// ordered by the keyset-paging pair. The field exists so that the
	// strong part of the gate cannot silently never fire: if the ORDER
	// BY tail in the store changes, the marker will stop matching, and
	// the test will fall here, not go green on the weak check.
	surfaces := []struct {
		name        string
		wantOrdered bool
		run         func(t *testing.T)
	}{
		{
			name:        "cross-project filter",
			wantOrdered: true,
			run: func(t *testing.T) {
				if _, err := store.ListByUser(ctx, issues.ListByUserArgs{
					Scope:  authz.ForUser(caller),
					UserID: caller,
					Mode:   issues.ModeCreatedBy,
					// The horizon is passed explicitly (Scope above).
					CallerUsername: callerName,
					Unreviewed:     true,
					Limit:          25,
				}); err != nil {
					t.Fatalf("ListByUser: %v", err)
				}
			},
		},
		{
			name:        "project filter",
			wantOrdered: true,
			run: func(t *testing.T) {
				id := caller
				if _, err := store.ListIssues(ctx, issues.ListIssuesArgs{
					ProjectID:      pid,
					Caller:         &id,
					CallerUsername: callerName,
					CreatedBy:      &id,
					Unreviewed:     true,
					Limit:          25,
				}); err != nil {
					t.Fatalf("ListIssues: %v", err)
				}
			},
		},
		{
			name:        "the second number",
			wantOrdered: false,
			run: func(t *testing.T) {
				// sk-622.21 T024: the second number is a scalar subquery of
				// the one header-counts statement (computeCounts).
				h := NewMeHandler(pool, nil, nil)
				if _, err := h.computeCounts(ctx, caller); err != nil {
					t.Fatalf("computeCounts: %v", err)
				}
			},
		},
	}

	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			capture.reset()
			s.run(t)

			stmts := capture.unreviewedOnes()
			if len(stmts) == 0 {
				t.Fatalf("%s: the observer intercepted not a single set query — "+
					"the assertion would be empty", s.name)
			}
			orderedSeen := 0
			for i, st := range stmts {
				ordered := ordersResult(st)
				if ordered {
					orderedSeen++
				}
				surface := fmt.Sprintf("%s, query %d", s.name, i+1)
				// Weak part — of every query and on the REAL table.
				assertUsesUnreviewedIndex(t, pool, surface, st)
				if ordered {
					// Strong part — only of ordered queries and only
					// on the snapshot (why — in the file header).
					assertOrderedRead(t, shadow.conn, shadow.schema, surface, st)
				}
			}
			if s.wantOrdered && orderedSeen == 0 {
				t.Errorf("%s: not a single intercepted query is ordered by %q. "+
					"Either order in the store changed, or the orderedness marker "+
					"went stale — in both cases the strong part of the gate never "+
					"fired, and its green means nothing", s.name, keysetOrderBy)
			}
			if !s.wantOrdered && orderedSeen != 0 {
				t.Errorf("%s: a reader that does not need order yielded %d "+
					"ordered queries — the surface's query set changed",
					s.name, orderedSeen)
			}
		})
	}
}

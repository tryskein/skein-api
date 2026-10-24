package issues

// inv8_corpus_test.go — sk-282 T001, test-plan row 13.
//
// Covers: "INV-8: reproducibility of the reference set (100 issues × 20
// children, 10 of them with a child in an invisible project) and
// stability of the median of five runs".
//
// This is the harness the INV-8 budget (growth ≤ 20%) is measured with,
// not the budget check itself: the ≤20% comparison needs a "before" and an
// "after" number and therefore belongs to T013/T030, which run the same
// harness on either side of the change. What this file proves is the
// precondition those tasks depend on — that the corpus is the SAME data
// set on both sides, and that one command produces the (median, plan) pair
// for a named query.
//
// Run: ./scripts/integration-test.sh ./internal/issues/... (needs
// TEST_DATABASE_URL; the harness sets it).

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

// TestINV8Corpus_ShapeIsReproducible seeds the reference corpus twice and
// asserts both instances match the declared shape exactly and each other.
//
// "The same data set before and after the change" is only checkable if the
// seeder is deterministic in shape. Comparing two independent seedings in
// one run is the strongest form of that check available without keeping a
// golden dump: any nondeterminism in the seeder (random statuses, a
// fan-out that depends on a counter, a hidden child that lands in the
// visible project) shows up as a diff here.
func TestINV8Corpus_ShapeIsReproducible(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	first := testsupport.SeedINV8Corpus(t, pool)
	second := testsupport.SeedINV8Corpus(t, pool)

	want := testsupport.ExpectedINV8Shape()
	gotFirst := testsupport.ReadINV8Shape(t, pool, first)
	gotSecond := testsupport.ReadINV8Shape(t, pool, second)

	if gotFirst != want {
		t.Errorf("first corpus shape\n got %+v\nwant %+v", gotFirst, want)
	}
	if gotSecond != want {
		t.Errorf("second corpus shape\n got %+v\nwant %+v", gotSecond, want)
	}
	if gotFirst != gotSecond {
		t.Errorf("two seedings of the same corpus differ:\n first %+v\nsecond %+v", gotFirst, gotSecond)
	}

	// The in-memory handle must agree with what is actually in the
	// database — T013/T030 drive their queries off the handle's ids.
	if n := len(first.ParentIDs); n != testsupport.INV8Parents {
		t.Errorf("handle ParentIDs = %d, want %d", n, testsupport.INV8Parents)
	}
	if n := len(first.HiddenChildIDs); n != testsupport.INV8HiddenParents {
		t.Errorf("handle HiddenChildIDs = %d, want %d", n, testsupport.INV8HiddenParents)
	}
	if got, want := first.TotalIssues(), testsupport.INV8Parents*(testsupport.INV8ChildrenPerParent+1); got != want {
		t.Errorf("handle TotalIssues = %d, want %d", got, want)
	}

	// The 10 hidden children must be invisible to the caller: they live
	// in a project the caller has no project_members row for. If this
	// ever passes trivially (e.g. the caller became a member), the
	// "unavailable" side of every later measurement is silently gone.
	assertHiddenFromCaller(t, pool, first)
}

// TestINV8Corpus_MedianAndPlanForNamedQuery is the T001 gate: one command,
// one named query, the (median, execution plan) pair printed.
//
// Timing is asserted only where an assertion cannot be flaky. A wall-clock
// bound on a shared CI database would be a coin flip, and the INV-8 budget
// is explicitly a BEFORE/AFTER comparison (≤20% growth), not an absolute
// ceiling — so the absolute number is reported, and what is asserted is
// that the harness produced a usable pair: five timed runs, a non-zero
// median, a real plan, and the same plan shape on a repeat pass over the
// identical corpus.
func TestINV8Corpus_MedianAndPlanForNamedQuery(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	corpus := testsupport.SeedINV8Corpus(t, pool)

	// The named query models the shape INV-8 cares about: the caller's
	// visible top-level issues on the corpus, together with the per-row
	// child rollup US-3 introduces. It is expressed as SQL rather than as
	// a store call so that the median and the plan describe the same
	// statement.
	//
	// The caller's visibility horizon is passed as an ARRAY of project
	// ids, matching how the production list path inlines
	// `i.project_id = ANY(...)`: measuring a single-project equality
	// instead would exercise a different plan than the tool actually
	// runs. Every placeholder is cast explicitly — an unused or
	// un-inferable parameter is rejected by the server before the query
	// ever runs (SQLSTATE 42P18).
	const namedQuery = `
SELECT i.id, i.title, i.status, i.priority,
       (SELECT count(*) FROM dependencies d
         JOIN issues c ON c.id = d.from_id
        WHERE d.to_id = i.id AND d.type = 'parent-child'
          AND c.project_id = ANY($1::uuid[])) AS children_count
  FROM issues i
 WHERE i.project_id = ANY($1::uuid[])
   AND NOT EXISTS (SELECT 1 FROM dependencies pd
                    WHERE pd.from_id = i.id AND pd.type = 'parent-child')
 ORDER BY i.priority, i.id
 LIMIT 100`

	args := []any{[]uuid.UUID{corpus.VisibleProjectID}}

	first := testsupport.MeasureINV8Query(t, pool,
		"issues.list_top_level_with_child_rollup", namedQuery, args...)
	testsupport.ReportINV8(t, first)

	if len(first.Runs) != testsupport.INV8Runs {
		t.Fatalf("timed runs = %d, want %d", len(first.Runs), testsupport.INV8Runs)
	}
	if first.Median <= 0 {
		t.Fatalf("median = %v, want a positive duration", first.Median)
	}
	if strings.TrimSpace(first.Plan) == "" || !strings.Contains(first.Plan, "actual time") {
		t.Fatalf("execution plan missing or not an ANALYZE plan:\n%s", first.Plan)
	}

	// Repeat pass over the SAME corpus. The planner sees identical
	// statistics, so the plan STRUCTURE must be identical; only measured
	// times may move. A structural diff here means the corpus is not
	// stable enough to compare "before" against "after".
	second := testsupport.MeasureINV8Query(t, pool,
		"issues.list_top_level_with_child_rollup(repeat)", namedQuery, args...)
	testsupport.ReportINV8(t, second)

	if got, want := planShape(second.Plan), planShape(first.Plan); got != want {
		t.Errorf("execution plan shape differs between two passes over the identical corpus:\n--- first ---\n%s\n--- second ---\n%s", want, got)
	}
	if second.Median <= 0 {
		t.Fatalf("repeat median = %v, want a positive duration", second.Median)
	}

	// Sanity guard, deliberately loose: the two medians are measured on
	// the same data by the same process, so an order-of-magnitude gap
	// means the measurement itself is unusable (machine paging, another
	// package hammering the shared DB), not that the query got slower.
	// The real ≤20% budget is T030's comparison of before/after runs.
	if r := medianRatio(first.Median, second.Median); r > 10 {
		t.Errorf("median unstable across two passes: %v vs %v (ratio %.1f)", first.Median, second.Median, r)
	}

	// The store path is measurable through the same harness — the pair's
	// plan half is honestly absent there rather than fabricated.
	store := NewStore(pool)
	storeMeasurement := testsupport.MeasureINV8Named(t, pool,
		"issues.ListAllIssues", "", nil, func() error {
			_, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
				Scope:          authz.ForUser(corpus.CallerID),
				Caller:         corpus.CallerID,
				CallerUsername: corpus.CallerUsername,
				Limit:          25,
			})
			return err
		})
	testsupport.ReportINV8(t, storeMeasurement)
	if storeMeasurement.Plan != "(not explained)" {
		t.Errorf("store measurement plan = %q, want the explicit not-explained marker", storeMeasurement.Plan)
	}
}

// assertHiddenFromCaller fails when the corpus caller can see the hidden
// project — i.e. when the "invisible child" half of the corpus has
// silently become visible.
func assertHiddenFromCaller(t *testing.T, pool *pgxpool.Pool, corpus *testsupport.INV8Corpus) {
	t.Helper()
	var member int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`,
		corpus.HiddenProjectID, corpus.CallerID).Scan(&member); err != nil {
		t.Fatalf("check hidden-project membership: %v", err)
	}
	if member != 0 {
		t.Fatalf("caller is a member of the hidden project (%d rows) — the invisible children are visible", member)
	}
}

// planNoise are the substitutions that turn an EXPLAIN ANALYZE plan into
// its run-invariant skeleton.
//
// Two classes are stripped, and the second one is why this is a list of
// substitutions rather than one regexp:
//
//	MEASUREMENT — times, costs, buffer counts, sort memory. Nobody expects
//	  two runs to agree on these.
//
//	DATA COUNTERS — `rows=`, `loops=`, `width=`, «Rows Removed by …»,
//	  «Index Searches», «Heap Fetches», «Heap Blocks», «Workers Launched».
//	  These describe HOW MUCH data the node touched, not WHAT the node
//	  did, and the amount is not a property of the reference corpus: the
//	  measured statement seq-scans `issues`, so «Rows Removed by Filter»
//	  counts every row every other test in the run has left in the table.
//	  Two passes inside one process see the same table and agree by
//	  accident; T013/T030 compare "before" against "after" on two separate
//	  throwaway databases with different amounts of foreign rows, and
//	  there the accident does not hold. A skeleton that keeps them would
//	  report "the plan changed" for a corpus that did not.
//
// The KEY is kept and only the VALUE dropped, so a node that reports a
// counter still looks different from a node that does not — losing that
// would let «Rows Removed by Filter» appear or vanish unnoticed, and its
// presence IS structure (it means a filter ran there).
//
// What survives is exactly what a plan change moves: node types, join
// order and strategy, the chosen indexes, the filter and join conditions,
// sort keys, the output column list.
var planNoise = []struct {
	re   *regexp.Regexp
	with string
}{
	// Measurement.
	{regexp.MustCompile(`actual time=[0-9.]+\.\.[0-9.]+`), `actual time=`},
	{regexp.MustCompile(`cost=[0-9.]+\.\.[0-9.]+`), `cost=`},
	{regexp.MustCompile(`(?m)^\s*Buffers:.*$`), ``},
	{regexp.MustCompile(`(?m)^\s*(Planning|Execution) Time:.*$`), ``},
	{regexp.MustCompile(`(?m)^\s*(Sort Method|Memory Usage|Peak Memory).*$`), ``},
	// Data counters. `rows=` is fractional on an ANALYZE plan (`rows=100.00`,
	// and `rows=19.90` on a node with loops>1, where it is an AVERAGE over
	// the loops) — matching only the integer part would leave the fraction
	// in the skeleton, which is the same leak one decimal point further in.
	{regexp.MustCompile(`\brows=[0-9]+(\.[0-9]+)?`), `rows=`},
	{regexp.MustCompile(`\bloops=[0-9]+(\.[0-9]+)?`), `loops=`},
	{regexp.MustCompile(`\bwidth=[0-9]+`), `width=`},
	{regexp.MustCompile(`(?m)^(\s*Rows Removed by [A-Za-z ]+:).*$`), `$1`},
	{regexp.MustCompile(`(?m)^(\s*(Index Searches|Heap Fetches|Heap Blocks|Workers Launched):).*$`), `$1`},
	// A hash node sizes itself from the data it hashed; the whole line is
	// counters («Buckets: 2048  Batches: 1  Memory Usage: 117kB») and the
	// Sort Method / Memory rule above does not reach it, because here
	// «Memory Usage» sits mid-line rather than at the start.
	{regexp.MustCompile(`(?m)^(\s*Buckets:).*$`), `$1`},
	// Run-specific IDENTITY inside a condition. The measured statement is
	// scoped to the corpus's project, so its plan carries that project's
	// uuid verbatim:
	//
	//	Filter: (i.project_id = ANY ('{f7cda7b8-…}'::uuid[]))
	//
	// The uuid is minted by the seeder, so it is different on every
	// database — guaranteed different between T013's "before" and T030's
	// "after", which are two separate throwaway databases. Keeping it
	// reproduces the very defect the counters did: the skeleton diverges
	// where the plan did not.
	//
	// The line is masked, not dropped, and the boundary runs through the
	// LITERAL rather than the condition: the column, the operator and the
	// shape of the test all survive, so swapping the filter to another
	// column, or `= ANY (…)` for `=`, still fails the comparison. What is
	// lost is only the answer to "which database was this taken on", which
	// is not what the skeleton is asked.
	{regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), `<uuid>`},
	// Same class, second form: testsupport.SeedProject mints an id_prefix
	// of `<name><6 hex from a random uuid>`, so the corpus's hierarchical
	// ids (`inv8vis1a2b3c-7`) are per-run names of throwaway objects too. A
	// query filtering on one puts it in the plan. Matched by the seeder's
	// own two names rather than by a general "word plus hex" shape — a
	// broad heuristic here would start eating column and index names,
	// which ARE structure.
	{regexp.MustCompile(`\b(inv8vis|inv8hid)[0-9a-f]{6}\b`), `$1<seed>`},
}

// planShape reduces an EXPLAIN ANALYZE plan to its run-invariant skeleton.
func planShape(plan string) string {
	stripped := plan
	for _, n := range planNoise {
		stripped = n.re.ReplaceAllString(stripped, n.with)
	}
	var kept []string
	for _, l := range strings.Split(stripped, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		kept = append(kept, strings.TrimRight(l, " "))
	}
	return strings.Join(kept, "\n")
}

// medianRatio returns max/min of two durations, guarding a zero divisor.
func medianRatio(a, b time.Duration) float64 {
	x, y := float64(a.Nanoseconds()), float64(b.Nanoseconds())
	if x < y {
		x, y = y, x
	}
	if y == 0 {
		return 0
	}
	return x / y
}

// TestPlanShape_IgnoresDataVolumeButNotStructure pins what the skeleton
// is allowed to forget.
//
// It is a pure function test on captured plan text — no pool, no
// TEST_DATABASE_URL — because the property it checks cannot be observed
// against a live database in one process: two passes over one table see
// the same row counts by construction, which is exactly why the defect it
// guards (a skeleton that keeps data counters) stayed invisible while
// looking green. T013/T030 read "before" and "after" off two separate
// throwaway databases whose `issues` table holds a different amount of
// unrelated rows, and that is the situation reproduced here by hand.
func TestPlanShape_IgnoresDataVolumeButNotStructure(t *testing.T) {
	// The same plan, executed on a database where the table holds far
	// more foreign rows. Every difference below is either a COUNT or a
	// run-specific IDENTITY (the seeded project's uuid, the seeded id
	// prefix) — the two things that cannot be equal on two separate
	// throwaway databases, and neither of which is the plan.
	small := `Limit  (cost=1.05..1.30 rows=10 width=64) (actual time=0.021..0.033 rows=10 loops=1)
  Output: i.id, i.title
  ->  Sort  (cost=1.05..1.10 rows=100 width=64) (actual time=0.020..0.024 rows=10 loops=1)
        Sort Key: i.priority, i.id
        Sort Method: quicksort  Memory: 25kB
        ->  Hash  (cost=0.55..0.55 rows=2000 width=19) (actual time=0.247..0.247 rows=2000.00 loops=1)
              Buckets: 2048  Batches: 1  Memory Usage: 117kB
              ->  Nested Loop  (cost=0.28..1.70 rows=20 width=0) (actual time=0.005..0.044 rows=19.90 loops=100)
                    Inner Unique: true
        ->  Seq Scan on public.issues i  (cost=0.00..0.90 rows=100 width=64) (actual time=0.005..0.011 rows=100.00 loops=1)
              Filter: (i.project_id = ANY ('{2e65211b-94f4-4d80-ba72-83b802de8a7d}'::uuid[]))
              Index Cond: ((i.id)::text ~~ 'inv8vis1a2b3c-%'::text)
              Rows Removed by Filter: 2100
              Heap Fetches: 0
              Index Searches: 1
              Buffers: shared hit=17
Planning Time: 0.412 ms
Execution Time: 0.061 ms`

	large := `Limit  (cost=9.05..9.30 rows=10 width=71) (actual time=1.021..1.933 rows=10 loops=1)
  Output: i.id, i.title
  ->  Sort  (cost=9.05..9.10 rows=100 width=71) (actual time=1.020..1.424 rows=10 loops=1)
        Sort Key: i.priority, i.id
        Sort Method: quicksort  Memory: 61kB
        ->  Hash  (cost=4.55..4.55 rows=80000 width=19) (actual time=9.247..9.247 rows=80000.00 loops=1)
              Buckets: 131072  Batches: 4  Memory Usage: 4211kB
              ->  Nested Loop  (cost=0.28..9.70 rows=20 width=0) (actual time=0.005..0.244 rows=17.42 loops=100)
                    Inner Unique: true
        ->  Seq Scan on public.issues i  (cost=0.00..8.90 rows=100 width=71) (actual time=0.105..0.911 rows=100.00 loops=1)
              Filter: (i.project_id = ANY ('{599e6edc-0b1e-43ee-93ea-366f3d74a1a1}'::uuid[]))
              Index Cond: ((i.id)::text ~~ 'inv8vis9f8e7d-%'::text)
              Rows Removed by Filter: 84213
              Heap Fetches: 4
              Index Searches: 3
              Buffers: shared hit=902 read=41
Planning Time: 1.910 ms
Execution Time: 2.702 ms`

	if planShape(small) != planShape(large) {
		t.Errorf("the skeleton changed when only the amount of unrelated data changed:\n"+
			"--- small ---\n%s\n--- large ---\n%s\n\n"+
			"INV-8 compares the plan of the SAME query on two databases seeded with the same "+
			"corpus but a different history of foreign rows. A skeleton that moves with the "+
			"row counts reports a plan change for a plan that did not change.",
			planShape(small), planShape(large))
	}

	// And the things a plan change actually moves must still move the
	// skeleton. Each mutation below is a different KIND of structural
	// change; a skeleton that survives any of them is not a plan check.
	structural := []struct {
		name string
		plan string
	}{
		{
			name: "node type (Seq Scan → Index Scan)",
			plan: strings.Replace(small,
				"Seq Scan on public.issues i",
				"Index Scan using issues_project_id_idx on public.issues i", 1),
		},
		{
			name: "sort key",
			plan: strings.Replace(small, "Sort Key: i.priority, i.id", "Sort Key: i.id", 1),
		},
		{
			// The masking runs through the literal, so the OPERATOR is
			// still part of the skeleton: `= ANY (…)` is a different test
			// than `=`, whatever uuid either of them names.
			name: "filter operator (= ANY (…) → =)",
			plan: strings.Replace(small,
				"Filter: (i.project_id = ANY ('{2e65211b-94f4-4d80-ba72-83b802de8a7d}'::uuid[]))",
				"Filter: (i.project_id = '00000000-0000-0000-0000-000000000000'::uuid)", 1),
		},
		{
			// …and so is the COLUMN. This is the case that decides whether
			// masking went too far: a filter moved to another column is a
			// different plan and must fail, even though every literal in
			// the line is masked identically.
			name: "filter column (project_id → created_by)",
			plan: strings.Replace(small,
				"Filter: (i.project_id = ANY ('{2e65211b-94f4-4d80-ba72-83b802de8a7d}'::uuid[]))",
				"Filter: (i.created_by = ANY ('{2e65211b-94f4-4d80-ba72-83b802de8a7d}'::uuid[]))", 1),
		},
		{
			name: "index condition dropped",
			plan: strings.Replace(small,
				"              Index Cond: ((i.id)::text ~~ 'inv8vis1a2b3c-%'::text)\n", "", 1),
		},
		{
			name: "output column list",
			plan: strings.Replace(small, "Output: i.id, i.title", "Output: i.id", 1),
		},
		{
			name: "a filter node stops filtering (the counter LINE disappears)",
			plan: strings.Replace(small, "              Rows Removed by Filter: 2100\n", "", 1),
		},
	}
	for _, m := range structural {
		if planShape(m.plan) == planShape(small) {
			t.Errorf("the skeleton did not notice a change of %s — it strips too much:\n%s",
				m.name, planShape(m.plan))
		}
	}
}

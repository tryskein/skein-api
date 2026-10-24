package testsupport

// inv8.go — the INV-8 reference corpus and the "median of five + execution
// plan" measurement harness (sk-282 T001).
//
// Why this lives here and not in a throwaway script: INV-8 says the
// performance budget (growth ≤ 20%) is only meaningful when BOTH
// measurements are taken on the *same* data set. A script that re-invents
// the corpus on each wave produces two numbers that cannot be compared,
// and both formally pass. So the corpus is a function, its shape is a set
// of named constants, and a test asserts the shape did not drift.
//
// Corpus shape — verbatim from the wave's specification, section "Constraints",
// "Performance — a single norm (INV-8)":
//
//	"a list of 100 issues, each with 20 children; of those, 10 issues have a
//	 child in a project invisible to the caller; the same data set before and
//	 after the change"
//
// One reading decision the spec leaves open and this file therefore fixes
// (it changes the row count, so it cannot be left to the reader): for the
// 10 marked parents the hidden child is ONE OF THE TWENTY, not a 21st.
// Every parent has exactly 20 children; for the marked ones the last of
// those 20 lives in a project the caller is not a member of. That keeps
// the fan-out uniform at 20 — which is what the measured query's cost
// depends on — and keeps the visible/hidden split observable as a pure
// visibility effect rather than as a difference in child count.
//
// Parent-child is stored as a `dependencies` row (from = child, to =
// parent, type = 'parent-child'). Since sk-35 that edge is the source of
// truth and `issues.parent_id` is a frozen column that new children do NOT
// set (see migration 000022 and internal/issues/store.go). The corpus
// follows the production representation exactly — seeding parent_id
// instead would measure a code path that no longer exists.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Corpus shape constants. They are the machine-readable form of the spec
// paragraph quoted above; INV8CorpusShape asserts a seeded corpus against
// them, so a drift in either direction is a red test rather than a silent
// re-baseline.
const (
	// INV8Parents is the number of top-level issues in the corpus.
	INV8Parents = 100
	// INV8ChildrenPerParent is the fan-out of every parent.
	INV8ChildrenPerParent = 20
	// INV8HiddenParents is how many of the parents have one child in a
	// project the caller cannot see.
	INV8HiddenParents = 10
	// INV8ClosedChildrenPerParent is how many of each parent's children
	// are closed. Fixed (not random) so two runs of the seeder produce
	// byte-identical status distributions — the corpus must be the same
	// before and after the change, not merely the same size.
	INV8ClosedChildrenPerParent = 15
	// INV8Runs is the number of timed runs whose median is reported.
	INV8Runs = 5
)

// INV8Corpus is the handle a measurement returns to. Every id is a real
// row in the database; the caller is a non-admin user who is a member of
// VisibleProjectID and NOT a member of HiddenProjectID, which is what
// makes the 10 hidden children invisible to it.
type INV8Corpus struct {
	// CallerID / CallerUsername identify the non-admin user the measured
	// query runs as.
	CallerID       uuid.UUID
	CallerUsername string

	// VisibleProjectID is the project the caller is a member of.
	VisibleProjectID uuid.UUID
	// VisibleProjectPrefix is that project's id_prefix (issue ids are
	// "<prefix>-N" and "<prefix>-N.M").
	VisibleProjectPrefix string

	// HiddenProjectID holds the 10 invisible children. The caller is not
	// a member and is not an admin, so it cannot see this project.
	HiddenProjectID uuid.UUID
	// HiddenProjectPrefix is that project's id_prefix.
	HiddenProjectPrefix string

	// ParentIDs holds the INV8Parents top-level issue ids, in seeding
	// order (index 0 is "<prefix>-1").
	ParentIDs []string
	// VisibleChildIDs holds every child that lives in the visible
	// project — INV8Parents*INV8ChildrenPerParent minus INV8HiddenParents.
	VisibleChildIDs []string
	// HiddenChildIDs holds the INV8HiddenParents children that live in
	// the hidden project.
	HiddenChildIDs []string
}

// TotalIssues is the number of issues rows the corpus owns across both
// projects.
func (c *INV8Corpus) TotalIssues() int {
	return len(c.ParentIDs) + len(c.VisibleChildIDs) + len(c.HiddenChildIDs)
}

// SeedINV8Corpus materialises the INV-8 reference corpus and returns a
// handle to it. It skips the test when TEST_DATABASE_URL is unset (via the
// pool the caller passes in — use NewTestPool).
//
// Everything it writes is torn down by the two SeedProject cleanups, which
// delete the projects' issues leaf-first; dependency rows cascade on issue
// delete, including the cross-project parent-child edges.
//
// The corpus is deterministic: same ids relative to the (random) project
// prefixes, same titles, same statuses, same priorities, same edge set on
// every run. Only the project prefixes and user names carry the random
// suffix that keeps concurrent packages from colliding.
func SeedINV8Corpus(t *testing.T, pool *pgxpool.Pool) *INV8Corpus {
	t.Helper()
	ctx := context.Background()

	callerID, callerName := SeedUser(t, pool, "user")
	visibleID, visiblePrefix := SeedProject(t, pool, callerID, "inv8vis")
	hiddenOwner, _ := SeedUser(t, pool, "user")
	hiddenID, hiddenPrefix := SeedProject(t, pool, hiddenOwner, "inv8hid")

	// SeedProject already made the owner a member. The caller owns the
	// visible project and has no row at all in the hidden one — that is
	// the whole point of the split.
	c := &INV8Corpus{
		CallerID:             callerID,
		CallerUsername:       callerName,
		VisibleProjectID:     visibleID,
		VisibleProjectPrefix: visiblePrefix,
		HiddenProjectID:      hiddenID,
		HiddenProjectPrefix:  hiddenPrefix,
		ParentIDs:            make([]string, 0, INV8Parents),
		VisibleChildIDs:      make([]string, 0, INV8Parents*INV8ChildrenPerParent),
		HiddenChildIDs:       make([]string, 0, INV8HiddenParents),
	}

	type issueRow struct {
		id        string
		projectID uuid.UUID
		title     string
		status    string
		priority  int
	}
	type depRow struct {
		from string
		to   string
	}

	issueRows := make([]issueRow, 0, INV8Parents*(INV8ChildrenPerParent+1))
	depRows := make([]depRow, 0, INV8Parents*INV8ChildrenPerParent)
	hiddenSeq := 0

	for p := 1; p <= INV8Parents; p++ {
		parentID := fmt.Sprintf("%s-%d", visiblePrefix, p)
		c.ParentIDs = append(c.ParentIDs, parentID)
		issueRows = append(issueRows, issueRow{
			id:        parentID,
			projectID: visibleID,
			title:     fmt.Sprintf("INV8 parent %03d", p),
			status:    "open",
			// Priorities cycle 0..4 so status/priority filters and index
			// selectivity are not degenerate on the corpus.
			priority: (p - 1) % 5,
		})

		// The last child of the first INV8HiddenParents parents lives in
		// the hidden project.
		hiddenChildIndex := 0
		if p <= INV8HiddenParents {
			hiddenChildIndex = INV8ChildrenPerParent
		}

		for k := 1; k <= INV8ChildrenPerParent; k++ {
			status := "open"
			if k <= INV8ClosedChildrenPerParent {
				status = "closed"
			}

			var childID string
			projectID := visibleID
			if k == hiddenChildIndex {
				hiddenSeq++
				childID = fmt.Sprintf("%s-%d", hiddenPrefix, hiddenSeq)
				projectID = hiddenID
				c.HiddenChildIDs = append(c.HiddenChildIDs, childID)
			} else {
				childID = fmt.Sprintf("%s.%d", parentID, k)
				c.VisibleChildIDs = append(c.VisibleChildIDs, childID)
			}

			issueRows = append(issueRows, issueRow{
				id:        childID,
				projectID: projectID,
				title:     fmt.Sprintf("INV8 child %03d.%02d", p, k),
				status:    status,
				priority:  (k - 1) % 5,
			})
			depRows = append(depRows, depRow{from: childID, to: parentID})
		}
	}

	// CopyFrom rather than 2 100 individual INSERTs: the corpus is seeded
	// on every measurement run, and a per-row round trip would dominate
	// the numbers we are trying to measure with setup cost.
	issueCols := []string{
		"id", "project_id", "version", "title", "description", "type",
		"priority", "status", "pinned", "source", "created_by",
		"created_at", "updated_at", "closed_at", "closed_reason",
	}
	now := time.Now().UTC()
	copied, err := pool.CopyFrom(ctx,
		pgx.Identifier{"issues"}, issueCols,
		pgx.CopyFromSlice(len(issueRows), func(i int) ([]any, error) {
			r := issueRows[i]
			var closedAt any
			var closedReason any
			// issues_closed_consistency: closed rows must carry closed_at.
			if r.status == "closed" {
				closedAt = now
				closedReason = "inv8 corpus"
			}
			return []any{
				r.id, r.projectID, 1, r.title, "", "task",
				r.priority, r.status, false, "human", callerID,
				now, now, closedAt, closedReason,
			}, nil
		}))
	if err != nil {
		t.Fatalf("seed INV8 issues: %v", err)
	}
	if int(copied) != len(issueRows) {
		t.Fatalf("seed INV8 issues: copied %d of %d rows", copied, len(issueRows))
	}

	depCopied, err := pool.CopyFrom(ctx,
		pgx.Identifier{"dependencies"},
		[]string{"from_id", "to_id", "type", "metadata", "version", "created_by", "created_at"},
		pgx.CopyFromSlice(len(depRows), func(i int) ([]any, error) {
			return []any{depRows[i].from, depRows[i].to, "parent-child", []byte("{}"), 1, callerID, now}, nil
		}))
	if err != nil {
		t.Fatalf("seed INV8 parent-child edges: %v", err)
	}
	if int(depCopied) != len(depRows) {
		t.Fatalf("seed INV8 edges: copied %d of %d rows", depCopied, len(depRows))
	}

	// child_counters must not fall behind the ids we just wrote, or a
	// later CreateIssue against a corpus parent would collide.
	if _, err := pool.Exec(ctx, `
INSERT INTO child_counters (parent_id, last_child)
SELECT id, $2 FROM issues WHERE project_id = $1 AND id NOT LIKE '%.%'
ON CONFLICT (parent_id) DO UPDATE SET last_child = GREATEST(child_counters.last_child, EXCLUDED.last_child)`,
		visibleID, INV8ChildrenPerParent); err != nil {
		t.Fatalf("seed INV8 child counters: %v", err)
	}
	// Same for the project counters — parents consumed 1..INV8Parents in
	// the visible project and 1..INV8HiddenParents in the hidden one.
	bumpProjectCounter(t, pool, visibleID, INV8Parents)
	bumpProjectCounter(t, pool, hiddenID, INV8HiddenParents)

	// ANALYZE so the first EXPLAIN of the run reads real statistics
	// instead of the defaults a freshly-COPYed table carries. Without it
	// the "before" plan and the "after" plan can differ for reasons that
	// have nothing to do with the change under test.
	if _, err := pool.Exec(ctx, `ANALYZE issues, dependencies`); err != nil {
		t.Fatalf("analyze after INV8 seed: %v", err)
	}

	return c
}

func bumpProjectCounter(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, to int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE project_counters SET counter = GREATEST(counter, $2) WHERE project_id = $1`,
		projectID, to); err != nil {
		t.Fatalf("bump project counter: %v", err)
	}
}

// INV8Shape is the observable shape of a seeded corpus, read back from the
// database rather than from the in-memory handle. Reading it back is the
// point: it is what proves the corpus is identical before and after a
// change, and it is what fails when a schema or a seeding assumption
// drifts.
type INV8Shape struct {
	Parents                int
	VisibleChildren        int
	HiddenChildren         int
	ParentChildEdges       int
	ClosedChildren         int
	OpenChildren           int
	ParentsWithHiddenChild int
}

// ReadINV8Shape counts the corpus back out of the database.
func ReadINV8Shape(t *testing.T, pool *pgxpool.Pool, c *INV8Corpus) INV8Shape {
	t.Helper()
	ctx := context.Background()
	var s INV8Shape

	scan := func(sql string, args []any, dst ...any) {
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst...); err != nil {
			t.Fatalf("read INV8 shape (%s): %v", strings.TrimSpace(sql), err)
		}
	}

	scan(`SELECT count(*) FROM issues i
           WHERE i.project_id = $1
             AND NOT EXISTS (SELECT 1 FROM dependencies d
                              WHERE d.from_id = i.id AND d.type = 'parent-child')`,
		[]any{c.VisibleProjectID}, &s.Parents)

	scan(`SELECT count(*) FROM issues i
           JOIN dependencies d ON d.from_id = i.id AND d.type = 'parent-child'
           WHERE i.project_id = $1`,
		[]any{c.VisibleProjectID}, &s.VisibleChildren)

	scan(`SELECT count(*) FROM issues i
           JOIN dependencies d ON d.from_id = i.id AND d.type = 'parent-child'
           WHERE i.project_id = $1`,
		[]any{c.HiddenProjectID}, &s.HiddenChildren)

	scan(`SELECT count(*) FROM dependencies d
           JOIN issues p ON p.id = d.to_id
           WHERE d.type = 'parent-child' AND p.project_id = $1`,
		[]any{c.VisibleProjectID}, &s.ParentChildEdges)

	scan(`SELECT
            count(*) FILTER (WHERE i.status = 'closed'),
            count(*) FILTER (WHERE i.status = 'open')
          FROM issues i
          JOIN dependencies d ON d.from_id = i.id AND d.type = 'parent-child'
          JOIN issues p ON p.id = d.to_id
          WHERE p.project_id = $1`,
		[]any{c.VisibleProjectID}, &s.ClosedChildren, &s.OpenChildren)

	scan(`SELECT count(DISTINCT d.to_id) FROM dependencies d
           JOIN issues c2 ON c2.id = d.from_id
           WHERE d.type = 'parent-child' AND c2.project_id = $1`,
		[]any{c.HiddenProjectID}, &s.ParentsWithHiddenChild)

	return s
}

// ExpectedINV8Shape is the shape the constants above describe. A seeded
// corpus must match it exactly.
func ExpectedINV8Shape() INV8Shape {
	total := INV8Parents * INV8ChildrenPerParent
	return INV8Shape{
		Parents:                INV8Parents,
		VisibleChildren:        total - INV8HiddenParents,
		HiddenChildren:         INV8HiddenParents,
		ParentChildEdges:       total,
		ClosedChildren:         INV8Parents * INV8ClosedChildrenPerParent,
		OpenChildren:           total - INV8Parents*INV8ClosedChildrenPerParent,
		ParentsWithHiddenChild: INV8HiddenParents,
	}
}

// ---------------------------------------------------------------------
// Measurement harness — "median of five + execution plan" for a NAMED query
// ---------------------------------------------------------------------

// INV8Measurement is the pair the INV-8 gate asks for: the median wall
// time of INV8Runs executions and the execution plan of the same query.
type INV8Measurement struct {
	Name   string
	Median time.Duration
	Runs   []time.Duration
	Plan   string
}

// MeasureINV8Query runs `sql` INV8Runs times against the pool, returns the
// median wall time and the EXPLAIN (ANALYZE, BUFFERS) plan of the same
// statement. "Any named query" in the T001 gate means exactly this: the
// caller supplies a name and a statement, the harness supplies the pair.
//
// The rows are drained but discarded — we measure the server round trip
// plus scan, which is what the tool's latency is made of, not what the
// caller then does with the rows.
func MeasureINV8Query(t *testing.T, pool *pgxpool.Pool, name, sql string, args ...any) INV8Measurement {
	t.Helper()
	return MeasureINV8Named(t, pool, name, sql, args, func() error {
		rows, err := pool.Query(context.Background(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	})
}

// MeasureINV8Named is the general form: the timed unit is an arbitrary
// func (a store method, an MCP handler call), while the plan still comes
// from an explainable statement. Pass an empty `explainSQL` when there is
// no single statement to explain — the plan is then reported as
// "(not explained)" rather than fabricated.
func MeasureINV8Named(t *testing.T, pool *pgxpool.Pool, name, explainSQL string, explainArgs []any, run func() error) INV8Measurement {
	t.Helper()
	m := INV8Measurement{Name: name, Runs: make([]time.Duration, 0, INV8Runs)}

	// One untimed warm-up: the first execution pays for plan caching and
	// cold buffers, and including it would make the median a measure of
	// the cache rather than of the query.
	if err := run(); err != nil {
		t.Fatalf("measure %q warm-up: %v", name, err)
	}
	for range INV8Runs {
		start := time.Now()
		if err := run(); err != nil {
			t.Fatalf("measure %q: %v", name, err)
		}
		m.Runs = append(m.Runs, time.Since(start))
	}
	sorted := slices.Clone(m.Runs)
	slices.Sort(sorted)
	m.Median = sorted[len(sorted)/2]

	m.Plan = "(not explained)"
	if explainSQL != "" {
		m.Plan = ExplainAnalyze(t, pool, explainSQL, explainArgs...)
	}
	return m
}

// ExplainAnalyze returns the text execution plan of sql. ANALYZE actually
// executes the statement — that is deliberate: a plan without real row
// counts cannot show the difference between an estimate and what the
// query did, which is the whole reason INV-8 asks for the plan and not
// just the timing.
func ExplainAnalyze(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT TEXT) "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return b.String()
}

// ReportINV8 prints the measurement in a line-oriented, greppable form so
// the "before" and "after" runs can be diffed without depending on the
// `go test` log format — the same discipline cmd/api/measure_tools_test.go
// uses for its MEASURE_ lines.
//
//	INV8_MEDIAN_NS <ns> <name>
//	INV8_RUNS_NS <ns> <ns> ... <name>
//	INV8_PLAN_BEGIN <name>
//	... plan lines ...
//	INV8_PLAN_END <name>
func ReportINV8(t *testing.T, m INV8Measurement) {
	t.Helper()
	runs := make([]string, 0, len(m.Runs))
	for _, d := range m.Runs {
		runs = append(runs, fmt.Sprint(d.Nanoseconds()))
	}
	plan := m.Plan
	if !strings.HasSuffix(plan, "\n") {
		plan += "\n"
	}
	// One t.Logf, not fmt.Printf: the report belongs to the test that
	// produced it. Printing to stdout puts it outside `go test`'s per-test
	// buffer, so under `-p`/`t.Parallel()` two measurements interleave
	// line by line and neither is attributable — which for a report whose
	// entire job is to be diffed between a "before" and an "after" run is
	// the difference between a baseline and a guess.
	//
	// One call rather than four keeps the block contiguous for the same
	// reason. The line prefixes stay greppable; the testing package only
	// indents them.
	t.Logf("INV8_MEDIAN_NS %d %s\nINV8_RUNS_NS %s %s\nINV8_PLAN_BEGIN %s\n%sINV8_PLAN_END %s",
		m.Median.Nanoseconds(), m.Name,
		strings.Join(runs, " "), m.Name,
		m.Name, plan, m.Name)
}

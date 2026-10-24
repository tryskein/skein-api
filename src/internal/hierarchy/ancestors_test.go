package hierarchy

// ancestors_test.go — sk-622.21, T003, test matrix row 8 (INV-3, US-3, edge
// 7). Reference — spec US-3 / INV-3, written before the code: the parent is
// the canonical one (the FIRST parent-child edge, as `parentIdCurrent`), a
// historical second edge is not a parent; ancestors are the whole chain of
// canonical parents, no depth limit (a chain deeper than 64 is processed in
// full); a closed loop stops the walk at the repetition and is logged as an
// error with the issue ids; the chain may cross projects.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// seedIssues inserts open issues <prefix>-<from> … <prefix>-<to> into project
// in one statement.
func seedIssues(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, prefix string, from, to int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, source_context, assignee, design, acceptance_criteria,
                    notes, estimated_minutes, due_at, defer_until, started_at, closed_at,
                    closed_reason, external_ref, created_by, created_at, updated_at, metadata)
SELECT $1 || '-' || n, $2, NULL, 1, 'issue ' || n, '', 'task', 2, 'open', false, 'human', NULL, NULL,
       '', '', '', NULL, NULL, NULL, NULL, NULL, NULL, NULL, $3, now(), now(), '{}'::jsonb
  FROM generate_series($4::int, $5::int) AS n`,
		prefix, projectID, creator, from, to); err != nil {
		t.Fatalf("seed issues %s-%d..%d: %v", prefix, from, to, err)
	}
}

// seedParentEdge writes a parent-child edge child → parent with an explicit
// creation time, so "first edge" is decided by the fixture, not by the clock.
func seedParentEdge(t *testing.T, pool *pgxpool.Pool, child, parent string, creator uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, $4)`, child, parent, creator, at); err != nil {
		t.Fatalf("seed edge %s -> %s: %v", child, parent, err)
	}
}

func ancestorIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) []string {
	t.Helper()
	got, err := Ancestors(ctx, pool, issueID)
	if err != nil {
		t.Fatalf("Ancestors(%s): %v", issueID, err)
	}
	ids := make([]string, len(got))
	for i, a := range got {
		if a.Depth != i+1 {
			t.Errorf("Ancestors(%s)[%d].Depth = %d, want %d", issueID, i, a.Depth, i+1)
		}
		ids[i] = a.IssueID
	}
	return ids
}

func isAncestor(t *testing.T, pool *pgxpool.Pool, ancestor, issue string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT `+IsAncestorExpr("$1::text", "$2::text"), ancestor, issue).Scan(&ok); err != nil {
		t.Fatalf("IsAncestorExpr(%s, %s): %v", ancestor, issue, err)
	}
	return ok
}

// TestAncestors_HistoricalSecondEdgeIsNotAParent — of two parent-child edges
// the older one is the parent; the newer one is history.
func TestAncestors_HistoricalSecondEdgeIsNotAParent(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "hier")
	seedIssues(t, pool, pid, owner, prefix, 1, 4)
	child, canonical, historical, root := prefix+"-1", prefix+"-2", prefix+"-3", prefix+"-4"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedParentEdge(t, pool, child, canonical, owner, base)
	seedParentEdge(t, pool, child, historical, owner, base.Add(time.Hour))
	seedParentEdge(t, pool, canonical, root, owner, base)

	if got, want := ancestorIDs(t, ctx, pool, child), []string{canonical, root}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Ancestors(%s) = %v, want %v", child, got, want)
	}
	if isAncestor(t, pool, historical, child) {
		t.Errorf("%s (historical second edge) is reported as an ancestor of %s", historical, child)
	}
	if !isAncestor(t, pool, root, child) {
		t.Errorf("%s is not reported as an ancestor of %s", root, child)
	}
}

// TestAncestors_DeepChainReturnedWhole — a chain of depth 70 (> 64) comes back
// in full, nearest first.
func TestAncestors_DeepChainReturnedWhole(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "hier")
	const depth = 70
	seedIssues(t, pool, pid, owner, prefix, 0, depth)
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT $1 || '-' || n, $1 || '-' || (n + 1), 'parent-child', '{}'::jsonb, 1, $2, now()
  FROM generate_series(0, $3::int - 1) AS n`, prefix, owner, depth); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	got := ancestorIDs(t, ctx, pool, prefix+"-0")
	if len(got) != depth {
		t.Fatalf("Ancestors of the chain bottom: %d entries, want %d", len(got), depth)
	}
	for i, id := range got {
		if want := fmt.Sprintf("%s-%d", prefix, i+1); id != want {
			t.Errorf("ancestor at depth %d = %s, want %s", i+1, id, want)
		}
	}
	if !isAncestor(t, pool, fmt.Sprintf("%s-%d", prefix, depth), prefix+"-0") {
		t.Errorf("the root at depth %d is not reported as an ancestor", depth)
	}
}

// TestAncestors_ClosedLoopStopsAndIsLogged — a loop in the data stops the walk
// at the repetition and leaves an error record naming the issues.
func TestAncestors_ClosedLoopStopsAndIsLogged(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "hier")
	seedIssues(t, pool, pid, owner, prefix, 1, 3)
	a, b, c := prefix+"-1", prefix+"-2", prefix+"-3"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedParentEdge(t, pool, a, b, owner, base)
	seedParentEdge(t, pool, b, c, owner, base)
	seedParentEdge(t, pool, c, b, owner, base) // b → c → b closes the loop

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := ancestorIDs(t, ctx, pool, a)
	if want := []string{b, c}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Ancestors(%s) over a loop = %v, want %v (stop at the repetition)", a, got, want)
	}

	var record map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["level"] == "ERROR" {
			record = m
			break
		}
	}
	if record == nil {
		t.Fatalf("no error record for the loop; log: %s", buf.String())
	}
	if record["issueId"] != a || record["repeatedIssueId"] != b {
		t.Errorf("loop record issueId=%v repeatedIssueId=%v, want %s / %s", record["issueId"], record["repeatedIssueId"], a, b)
	}
	chain, _ := record["chain"].(string)
	for _, id := range []string{a, b, c} {
		if !strings.Contains(chain, id) {
			t.Errorf("loop record chain %q does not name %s", chain, id)
		}
	}
}

// TestAncestors_ChainCrossesProjects — the chain follows edges across projects.
func TestAncestors_ChainCrossesProjects(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	p1, prefix1 := testsupport.SeedProject(t, pool, owner, "hier")
	p2, prefix2 := testsupport.SeedProject(t, pool, owner, "hier")
	seedIssues(t, pool, p1, owner, prefix1, 1, 2)
	seedIssues(t, pool, p2, owner, prefix2, 1, 1)
	child, middle, top := prefix1+"-1", prefix2+"-1", prefix1+"-2"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedParentEdge(t, pool, child, middle, owner, base)
	seedParentEdge(t, pool, middle, top, owner, base)

	if got, want := ancestorIDs(t, ctx, pool, child), []string{middle, top}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Ancestors(%s) across projects = %v, want %v", child, got, want)
	}
	if !isAncestor(t, pool, top, child) {
		t.Errorf("%s (two projects away) is not reported as an ancestor of %s", top, child)
	}
}

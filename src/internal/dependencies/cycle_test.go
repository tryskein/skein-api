package dependencies

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedParentChildDep inserts one parent-child dep row directly so the
// cycle-detection tests can assemble fixtures without going through
// Create (which would itself call the cycle check we're testing).
func seedParentChildDep(t *testing.T, pool *pgxpool.Pool, fromID, toID string, creator uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`,
		fromID, toID, creator)
	if err != nil {
		t.Fatalf("seed parent-child dep: %v", err)
	}
}

// TestCheckParentChildCycle_DirectCycle: A is child of B, then try to
// add B is child of A. Walking forward from `to=A` along parent-child
// edges reaches B (A→B exists already) which is the new edge's
// `from`, so the cycle is detected.
//
// Note on direction semantics: parent-child has from=child, to=parent,
// so "A→B" reads "A is a child of B". checkCycleForTypes walks forward
// from the new edge's to-side along outgoing edges of the SAME type;
// if it finds the new edge's from-side, the new edge would close a
// cycle. For A→B (A child of B), adding B→A would mean "B is a child
// of A". Forward walk from A finds B via A→B, so adding B→A would
// close the cycle.
func TestCheckParentChildCycle_DirectCycle(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	a := prefix + "-1"
	b := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, a, "a")
	seedOpenIssue(t, pool, pid, uid, b, "b")
	seedParentChildDep(t, pool, a, b, uid)

	err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return CheckParentChildCycle(context.Background(), tx, b, a)
	})
	var ce *ErrDepCycle
	if !errors.As(err, &ce) {
		t.Fatalf("expected ErrDepCycle, got %v", err)
	}
}

// TestCheckParentChildCycle_TransitiveCycle: A→B, B→C (A is grandchild
// of C). Inserting C→A would re-enter the graph through A. Walking
// forward from `to=A` we reach B via A→B and C via B→C — finding `C`
// at the cycle-closing point.
func TestCheckParentChildCycle_TransitiveCycle(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	a := prefix + "-1"
	b := prefix + "-2"
	c := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, a, "a")
	seedOpenIssue(t, pool, pid, uid, b, "b")
	seedOpenIssue(t, pool, pid, uid, c, "c")
	seedParentChildDep(t, pool, a, b, uid)
	seedParentChildDep(t, pool, b, c, uid)

	err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return CheckParentChildCycle(context.Background(), tx, c, a)
	})
	var ce *ErrDepCycle
	if !errors.As(err, &ce) {
		t.Fatalf("expected ErrDepCycle for transitive cycle, got %v", err)
	}
}

// TestCheckParentChildCycle_NoCycle: multi-parent fan-in (two siblings
// share the same parent) is NOT a cycle.
func TestCheckParentChildCycle_NoCycle(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	a := prefix + "-1"
	b := prefix + "-2"
	c := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, a, "a")
	seedOpenIssue(t, pool, pid, uid, b, "b")
	seedOpenIssue(t, pool, pid, uid, c, "c")
	seedParentChildDep(t, pool, a, b, uid) // A is child of B
	// Now propose C→B (C is also child of B) — both A and C are
	// children of B. No cycle.
	err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return CheckParentChildCycle(context.Background(), tx, c, b)
	})
	if err != nil {
		t.Fatalf("expected nil for non-cyclic insert, got %v", err)
	}
}

// seedEdges inserts edges[i] = {from, to} of one type in a single statement.
func seedEdges(t *testing.T, pool *pgxpool.Pool, depType string, edges [][2]string, creator uuid.UUID) {
	t.Helper()
	froms := make([]string, len(edges))
	tos := make([]string, len(edges))
	for i, e := range edges {
		froms[i], tos[i] = e[0], e[1]
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT f, t, $3, '{}'::jsonb, 1, $4, now()
  FROM unnest($1::text[], $2::text[]) AS e(f, t)`, froms, tos, depType, creator); err != nil {
		t.Fatalf("seed %d %s edges: %v", len(edges), depType, err)
	}
}

// seedIssues creates n open issues <prefix>-1 … <prefix>-n and returns their ids.
func seedIssues(t *testing.T, pool *pgxpool.Pool, pid, uid uuid.UUID, prefix string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		seedOpenIssue(t, pool, pid, uid, ids[i], ids[i])
	}
	return ids
}

// cycleCheckers are the two public entry points over the shared walk; every
// property below holds for both type-classes.
var cycleCheckers = []struct {
	name    string
	depType string
	check   func(context.Context, pgx.Tx, string, string) error
}{
	{"blocks", DepBlocks, CheckBlocksCycle},
	{"parent-child", DepParentChild, CheckParentChildCycle},
}

// TestCheckCycle_TransitiveDAG_BoundedResources is the sk-647 regression.
//
// Incident: an agent added ~330 blocks edges forming a dense DAG; every later
// cycle check enumerated each simple path from `to` (one CTE row per path)
// and spilled ~7 GB of pgsql_tmp until the disk filled (SQLSTATE 53100).
//
// Fixture: the full transitive DAG on 22 issues — issue i has an edge to
// every issue j > i, 231 edges. From issue 1 there are 2^20 simple paths, but
// only 22 reachable issues. The check runs for an edge whose FROM side is
// unreachable, so nothing can stop the walk early: it must cover the whole
// reachable set.
//
// Resource criterion, deterministic and not a clock: the transaction caps
// itself at work_mem = 1MB and temp_file_limit = 1MB (the role is a superuser
// in the test stack). A walk over issues stays in memory far below that; a
// walk over paths cannot, and Postgres cancels it with 53400. Both settings
// are pinned here, so the verdict does not depend on the server's defaults.
//
// The second half checks that the same graph still yields a cycle, with the
// shortest closing path, when the new edge points back to its start.
func TestCheckCycle_TransitiveDAG_BoundedResources(t *testing.T) {
	const n = 22
	for _, c := range cycleCheckers {
		t.Run(c.name, func(t *testing.T) {
			pool := newTestPool(t)
			uid := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, uid)
			ids := seedIssues(t, pool, pid, uid, prefix, n+1)
			dag, outsider := ids[:n], ids[n]
			var edges [][2]string
			for i := range n {
				for j := i + 1; j < n; j++ {
					edges = append(edges, [2]string{dag[i], dag[j]})
				}
			}
			seedEdges(t, pool, c.depType, edges, uid)

			run := func(from, to string) error {
				return pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
					if _, err := tx.Exec(context.Background(),
						`SET LOCAL work_mem = '1MB'; SET LOCAL temp_file_limit = '1MB'`); err != nil {
						t.Fatalf("pin resource limits: %v", err)
					}
					return c.check(context.Background(), tx, from, to)
				})
			}

			// outsider -> dag[0]: dag[0] reaches all 22 issues, never outsider.
			if err := run(outsider, dag[0]); err != nil {
				t.Fatalf("check(%s -> %s) over the %d-issue transitive DAG = %v, want nil (no cycle) within work_mem/temp_file_limit 1MB",
					outsider, dag[0], n, err)
			}

			// dag[n-1] -> dag[0] closes a cycle; the shortest one is the
			// direct edge dag[0] -> dag[n-1].
			err := run(dag[n-1], dag[0])
			ce, ok := IsCycle(err)
			if !ok {
				t.Fatalf("check(%s -> %s) = %v, want ErrDepCycle", dag[n-1], dag[0], err)
			}
			want := []string{dag[n-1], dag[0], dag[n-1]}
			if !slices.Equal(ce.Path, want) {
				t.Fatalf("ErrDepCycle.Path = %v, want %v", ce.Path, want)
			}
		})
	}
}

// TestCheckCycle_LongerThan64_Detected: the old walk stopped at depth 64, so
// a chain of 70 edges plus the edge closing it was accepted — a real cycle in
// the blocking graph (Ready never releases any member) or in the hierarchy.
// The whole chain must come back as the path, in walk order.
func TestCheckCycle_LongerThan64_Detected(t *testing.T) {
	const chain = 71 // issues; 70 edges between them
	for _, c := range cycleCheckers {
		t.Run(c.name, func(t *testing.T) {
			pool := newTestPool(t)
			uid := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, uid)
			ids := seedIssues(t, pool, pid, uid, prefix, chain)
			edges := make([][2]string, 0, chain-1)
			for i := 0; i+1 < chain; i++ {
				edges = append(edges, [2]string{ids[i], ids[i+1]})
			}
			seedEdges(t, pool, c.depType, edges, uid)

			last, first := ids[chain-1], ids[0]
			err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
				return c.check(context.Background(), tx, last, first)
			})
			ce, ok := IsCycle(err)
			if !ok {
				t.Fatalf("check(%s -> %s) closing a %d-edge chain = %v, want ErrDepCycle", last, first, chain-1, err)
			}
			want := append([]string{last}, ids...)
			if !slices.Equal(ce.Path, want) {
				t.Fatalf("ErrDepCycle.Path = %v, want %v", ce.Path, want)
			}
		})
	}
}

// TestCheckCycle_CycleAlreadyInData: the graph already holds a cycle
// A → B → A (seeded directly — data from before a check existed, or a bug
// elsewhere), plus A → C. The walk must stay finite on it: UNION drops the
// revisit of A, where the old walk relied on its per-path guard.
//
//   - D → A: the walk from A covers {A, B, C} and never meets D — no cycle;
//   - C → B: the walk from B reaches C through the existing cycle, B → A → C,
//     so the edge closes C → B → A → C, and that is the path reported.
func TestCheckCycle_CycleAlreadyInData(t *testing.T) {
	for _, c := range cycleCheckers {
		t.Run(c.name, func(t *testing.T) {
			pool := newTestPool(t)
			uid := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, uid)
			ids := seedIssues(t, pool, pid, uid, prefix, 4)
			a, b, cc, d := ids[0], ids[1], ids[2], ids[3]
			seedEdges(t, pool, c.depType, [][2]string{{a, b}, {b, a}, {a, cc}}, uid)

			run := func(from, to string) error {
				return pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
					return c.check(context.Background(), tx, from, to)
				})
			}

			if err := run(d, a); err != nil {
				t.Fatalf("check(%s -> %s) beside the existing cycle = %v, want nil", d, a, err)
			}
			err := run(cc, b)
			ce, ok := IsCycle(err)
			if !ok {
				t.Fatalf("check(%s -> %s) = %v, want ErrDepCycle", cc, b, err)
			}
			want := []string{cc, b, a, cc}
			if !slices.Equal(ce.Path, want) {
				t.Fatalf("ErrDepCycle.Path = %v, want %v", ce.Path, want)
			}
		})
	}
}

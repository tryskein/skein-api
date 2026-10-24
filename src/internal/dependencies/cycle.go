package dependencies

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ErrDepCycle is returned by the cycle-detection helpers when inserting
// (from -> to) of the requested type would close a directed cycle inside
// the same type-class. Path lists the issue IDs along the closing cycle,
// in walk order, useful for UI error feedback ("X → Y → Z → X").
type ErrDepCycle struct {
	Path []string
}

func (e *ErrDepCycle) Error() string {
	return fmt.Sprintf("dependency cycle detected: %v", e.Path)
}

// IsCycle is the conventional unwrap helper.
func IsCycle(err error) (*ErrDepCycle, bool) {
	var ce *ErrDepCycle
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// blockingCycleTypes is the set of dep types that participate in the
// "blocking" cycle graph. Adding (from -> to) of one of these types must
// not introduce a directed cycle in the union of all such edges, because
// these are the types Ready algorithm walks. Mixing blocks/waits-for in
// the same graph mirrors the real semantics — both gate progress.
var blockingCycleTypes = []string{DepBlocks, DepWaitsFor}

// parentChildCycleTypes is the (singleton) type-class for hierarchical
// cycle detection. We walk only `parent-child` edges so a chain that
// blends with `blocks` doesn't accidentally count as a hierarchical
// loop. With sk-35 a child may carry multiple parent-child edges
// (no UNIQUE constraint on the type) — the walk follows them all,
// rejecting any insert that would re-enter the from-side.
var parentChildCycleTypes = []string{DepParentChild}

// CheckBlocksCycle returns ErrDepCycle if inserting (from -> to) of type
// `blocks` or `waits-for` would close a directed cycle in the
// blocks∪waits-for graph. Implementation walks forward from `to` along
// only those types; if it finds `from`, the new edge would close the
// cycle.
func CheckBlocksCycle(ctx context.Context, tx pgx.Tx, fromID, toID string) error {
	return checkCycleForTypes(ctx, tx, fromID, toID, blockingCycleTypes)
}

// CheckParentChildCycle returns ErrDepCycle if inserting a (from -> to)
// parent-child edge (i.e. "from is a child of to") would close a
// directed cycle in the parent-child graph. Cycle = the would-be parent
// is already a descendant of the would-be child. Restored from the
// pre-sk-33 implementation for sk-35.
func CheckParentChildCycle(ctx context.Context, tx pgx.Tx, fromID, toID string) error {
	return checkCycleForTypes(ctx, tx, fromID, toID, parentChildCycleTypes)
}

// reachSQL is the forward reachability set from $1 along edges whose type is
// in $2: one row per issue reachable from $1, $1 included.
//
// It is a walk over NODES, not over paths (sk-647.1). UNION — not UNION ALL —
// in the recursive term makes Postgres discard every row that duplicates one
// already in the result, so each issue enters the working table at most once
// and the recursion ends when a level adds no new issue: the work is bounded
// by the edges of the reachable subgraph, whatever its shape. That is also
// why a cycle already on disk cannot make it run away, and why no depth cap
// is needed.
//
// The walk it replaced carried the path in every row (UNION ALL plus
// `NOT (d.to_id = ANY(w.path))`), i.e. one row per simple path from $1. On a
// transitive DAG (every issue blocks every later one) that count is
// exponential: a batch of ~330 blocks edges made each check spill gigabytes
// of pgsql_tmp until the volume filled (SQLSTATE 53100, sk-647). Its
// `depth < 64` cap bounded the length of a path, not the number of them, and
// it also hid every cycle longer than 64 edges.
//
// ::text on both sides keeps the recursive CTE's column type stable — issues
// ids are varchar(1000), and Postgres rejects a non-recursive/recursive term
// pair whose column types differ (SQLSTATE 42804). The join stays on
// dependencies(from_id, type) (idx_deps_from_type, or the PK's leading
// from_id), so each level is an index lookup per frontier issue.
const reachSQL = `
WITH RECURSIVE reach(id) AS (
    SELECT $1::text
    UNION
    SELECT d.to_id::text
      FROM dependencies d
      JOIN reach r ON d.from_id = r.id
     WHERE d.type = ANY($2)
)`

// checkCycleForTypes reports whether inserting (from -> to) would close a
// directed cycle among edges of the given types: it would iff `from` is
// reachable from `to`.
//
// Two queries, and the second only on a hit:
//
//  1. Existence — `from` IN reach(to), LIMIT 1. The outer query filters the
//     CTE's rows as they are produced, so Postgres stops the recursion at the
//     first match (it evaluates only as many CTE rows as the parent fetches);
//     without a match it walks the reachable set once. This is the common
//     path: almost every insert is not a cycle.
//  2. Path, for ErrDepCycle — only when a cycle was found. It loads the edges
//     whose FROM side is in reach(to) and runs a breadth-first search in Go
//     from `to` to `from`. Bounded by the same reachable subgraph as (1), so
//     a rejected insert costs at most one more pass over it. BFS yields a
//     shortest path, which is what the old breadth-first path walk returned
//     too, so ErrDepCycle.Path keeps its form: from → to → … → from.
//
// Both queries run in the caller's transaction, which callers keep
// SERIALIZABLE (see CreateInTx), so (2) sees the graph (1) saw.
func checkCycleForTypes(ctx context.Context, tx pgx.Tx, fromID, toID string, types []string) error {
	if fromID == toID {
		// dep_no_self CHECK in DDL would catch this on INSERT, but we
		// short-circuit with a clearer error and avoid the round-trip.
		return &ErrDepCycle{Path: []string{fromID}}
	}

	var hit int
	err := tx.QueryRow(ctx, reachSQL+`
SELECT 1 FROM reach WHERE id = $3::text LIMIT 1`, toID, types, fromID).Scan(&hit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no cycle: walking forward from `to` never reached `from`
	}
	if err != nil {
		return fmt.Errorf("running cycle check: %w", err)
	}

	path, err := cyclePath(ctx, tx, fromID, toID, types)
	if err != nil {
		return err
	}
	return &ErrDepCycle{Path: path}
}

// cyclePath returns the cycle the edge (from -> to) would close, in the
// order ErrDepCycle.Path documents: from, to, …, from. It is called only
// after checkCycleForTypes established that `from` is reachable from `to`.
func cyclePath(ctx context.Context, tx pgx.Tx, fromID, toID string, types []string) ([]string, error) {
	rows, err := tx.Query(ctx, reachSQL+`
SELECT d.from_id::text, d.to_id::text
  FROM dependencies d
  JOIN reach r ON d.from_id = r.id
 WHERE d.type = ANY($2)`, toID, types)
	if err != nil {
		return nil, fmt.Errorf("loading cycle subgraph: %w", err)
	}
	out := map[string][]string{}
	for rows.Next() {
		var f, t string
		if err := rows.Scan(&f, &t); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning cycle subgraph edge: %w", err)
		}
		out[f] = append(out[f], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating cycle subgraph: %w", err)
	}

	walk := shortestPath(out, toID, fromID)
	if walk == nil {
		// Unreachable under the SERIALIZABLE snapshot both queries share;
		// reaching it means a caller ran the check outside one.
		return nil, fmt.Errorf("cycle check found %s reachable from %s but no path between them", fromID, toID)
	}
	full := make([]string, 0, len(walk)+1)
	full = append(full, fromID)
	full = append(full, walk...)
	return full, nil
}

// shortestPath is a breadth-first search over adjacency lists. It returns
// the node sequence src … dst of a shortest path, or nil when dst is not
// reachable from src.
func shortestPath(adj map[string][]string, src, dst string) []string {
	prev := map[string]string{src: ""}
	queue := []string{src}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n == dst {
			var rev []string
			for at := dst; at != src; at = prev[at] {
				rev = append(rev, at)
			}
			rev = append(rev, src)
			slices.Reverse(rev)
			return rev
		}
		for _, next := range adj[n] {
			if _, seen := prev[next]; seen {
				continue
			}
			prev[next] = n
			queue = append(queue, next)
		}
	}
	return nil
}

package dependencies

// counts.go — dependency counters in the «direction × type × status» cut
// (sk-282 T008, spec US-1 / INV-9).
//
// # Why counters replace edges
//
// An agent running a wave opens the group node to learn one thing: how
// many children there are and how many are closed. Today it gets the whole
// edge graph and pays 20 742 characters for it on the measured node — 88%
// of the response. The counters answer the actual question, and the edges
// become an explicit, typed request (T009).
//
// # Why the cut is direction × type and not type alone
//
// INV-9, in one sentence: a node with 57 children and one parent would
// report «58 parent-child links» under a direction-blind counter, and the
// agent could not tell «the parent exists» from «the children are done».
// The same rule binds the unavailable bucket — «unavailable: 2» without a
// direction cannot distinguish one invisible parent from two invisible
// children, and the orchestrator is back to not knowing whether the wave
// is complete.
//
// Directions are named after the TREE, not after the storage columns
// (plan, D-4). parent-child is stored from = child, to = parent, so:
//
//	up   — the issue's OUTGOING edges: where the issue points, its parent
//	down — the issue's INCOMING edges: what points at it, its children
//
// The mapping is uniform across types; naming the directions after the
// columns (`outgoing` / `incoming`) means the opposite of intuition for
// exactly the type an orchestrator reads most, and swapping the values
// under the old names would be the defect INV-9 names.
//
// # Why the invisible side gets its own bucket rather than being dropped
//
// «Counting only what is visible» is the variant the spec rejects by name:
// it makes a cross-project tail vanish silently, and an agent then reads a
// zero as permission to close a container that still has an invisible open
// child. So an edge whose other side the caller cannot see is counted — in
// a named `unavailable` bucket, inside its own direction × type cell, and
// NOT broken down by status. The status of an invisible issue must not be
// derivable from any bucket.
//
// That is enforced in SQL rather than in Go: the invisible rows have their
// status collapsed to NULL before aggregation, so the invisible side's
// status never leaves the database at all.

import (
	"context"
	"fmt"

	"skein-api/internal/authz"
)

// Direction keys of the aggregate. Named after the tree (D-4).
const (
	// DirectionUp holds the issue's outgoing edges — for parent-child,
	// its parent.
	DirectionUp = "up"
	// DirectionDown holds the issue's incoming edges — for parent-child,
	// its children.
	DirectionDown = "down"
)

// DepStatusCounts is one «direction × type» cell: a full set of status
// buckets for the visible edges plus one bucket for the edges whose other
// side the caller cannot see.
//
// Every bucket is emitted even at zero — SC-001 fixes that explicitly, so
// that a zero inside a present type reads as «counted, and it is none»
// rather than as «not counted». A type the issue has no edges of is absent
// from the map entirely, which is what keeps the aggregate at roughly 240
// characters instead of one cell per type in the closed set.
type DepStatusCounts struct {
	Open       int `json:"open"`
	InProgress int `json:"in_progress"`
	Deferred   int `json:"deferred"`
	Closed     int `json:"closed"`
	Standing   int `json:"standing"`
	// Unavailable counts edges whose other side is in a project the
	// caller is not a member of. Deliberately NOT split by status.
	Unavailable int `json:"unavailable"`
}

// Total is the number of edges in this cell — the sum every acceptance
// check compares against what an explicit edge request would return.
func (c DepStatusCounts) Total() int {
	return c.Open + c.InProgress + c.Deferred + c.Closed + c.Standing + c.Unavailable
}

// DependencyCounts is the whole aggregate: direction → type → cell.
//
// Both direction keys are ALWAYS present, empty map included. An issue
// with no parent must read as «nothing upward», and a missing key would be
// indistinguishable from «this direction was not computed» — the exact
// confusion INV-1 forbids. Ten characters is a cheap price for that.
type DependencyCounts struct {
	Up   map[string]DepStatusCounts `json:"up"`
	Down map[string]DepStatusCounts `json:"down"`
}

// depCountStatusBuckets maps a stored issue status to the field it lands
// in. It is the closed set the DB CHECK allows (migration 000029), and an
// unknown value is an ERROR rather than a silently dropped row — see
// CountDependencies.
var depCountStatusBuckets = map[string]func(*DepStatusCounts, int){
	"open":        func(c *DepStatusCounts, n int) { c.Open += n },
	"in_progress": func(c *DepStatusCounts, n int) { c.InProgress += n },
	"deferred":    func(c *DepStatusCounts, n int) { c.Deferred += n },
	"closed":      func(c *DepStatusCounts, n int) { c.Closed += n },
	"standing":    func(c *DepStatusCounts, n int) { c.Standing += n },
}

// DepCountStatuses returns the status names that have a bucket, sorted the
// way they are declared on DepStatusCounts. Exported for the drift-guard
// test, which compares it with the status CHECK constraint the database
// actually enforces.
func DepCountStatuses() []string {
	return []string{"open", "in_progress", "deferred", "closed", "standing"}
}

// ErrUnknownDependencyStatus is returned when the database yields an issue
// status this aggregate has no bucket for.
//
// Failing is deliberate. The status set is a closed CHECK constraint, so a
// new value can only arrive with a migration — and a migration that adds a
// status while the counters quietly drop it would make «the wave is
// finished» wrong in the one place an orchestrator trusts. A read that
// fails names the gap on the first call; a read that under-counts names it
// never.
var ErrUnknownDependencyStatus = fmt.Errorf("dependency counts: issue status has no bucket")

// CountDependencies returns the «direction × type × status» aggregate for
// one issue, in a single query.
//
// scope is the caller's horizon (authz), the same one List uses — the
// redaction policy is the access relation, not reinvented here, so the
// counters and an explicit edge request can never disagree about what the
// caller may see.
//
// The result never contains a type the issue has no edges of, and always
// contains both direction keys.
func (s *Store) CountDependencies(
	ctx context.Context,
	issueID string,
	scope authz.Scope,
) (*DependencyCounts, error) {
	unrestricted, caller, bindErr := scope.Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	// INV-28 (4): an invisible edge counts in `unavailable` only when its
	// other side lies in the caller's own non-personal space; an edge into
	// another's space or another's "Personal" is in no bucket at all.
	ownSpaces, spacesErr := scope.BindSpaces(ctx, s.pool)
	if spacesErr != nil {
		return nil, spacesErr
	}

	// One round-trip, mirroring List's UNION ALL so the two can never
	// disagree about which edges belong to the issue:
	//   direction 0 = outgoing (d.from_id = issue; other side = to_id)
	//   direction 1 = incoming (d.to_id   = issue; other side = from_id)
	//
	// The `classified` CTE decides visibility once, and the SELECT below
	// collapses an invisible row's status to NULL BEFORE grouping. That
	// is what makes «the invisible side's status is not disclosed» a
	// property of the query rather than of the Go code reading it.
	rows, err := s.pool.Query(ctx, `
WITH edges AS (
    SELECT 0 AS direction, d.type AS dep_type, i.project_id, i.status
      FROM dependencies d
      JOIN issues i ON i.id = d.to_id
     WHERE d.from_id = $1
    UNION ALL
    SELECT 1 AS direction, d.type AS dep_type, i.project_id, i.status
      FROM dependencies d
      JOIN issues i ON i.id = d.from_id
     WHERE d.to_id = $1
), classified AS (
    SELECT e.direction, e.dep_type,
           `+authz.ProjectVisibleExpr("e", "$2", "$3")+` AS visible,
           e.status
      FROM edges e
     WHERE `+authz.ProjectVisibleExpr("e", "$2", "$3")+`
        OR `+authz.HiddenCountableExpr("e.project_id", "$2", "$3", "$4")+`
)
SELECT direction, dep_type, visible,
       CASE WHEN visible THEN status END AS bucket_status,
       count(*) AS n
  FROM classified
 GROUP BY direction, dep_type, visible, CASE WHEN visible THEN status END`,
		issueID, unrestricted, caller, ownSpaces)
	if err != nil {
		return nil, fmt.Errorf("counting deps: %w", err)
	}
	defer rows.Close()

	out := &DependencyCounts{
		Up:   make(map[string]DepStatusCounts, 2),
		Down: make(map[string]DepStatusCounts, 2),
	}
	for rows.Next() {
		var (
			direction int
			depType   string
			isVisible bool
			status    *string
			n         int
		)
		if err := rows.Scan(&direction, &depType, &isVisible, &status, &n); err != nil {
			return nil, fmt.Errorf("scanning dep counts: %w", err)
		}

		side := out.Down
		if direction == 0 {
			side = out.Up
		}
		cell := side[depType]

		if !isVisible {
			cell.Unavailable += n
			side[depType] = cell
			continue
		}
		if status == nil {
			// A visible row with a NULL status cannot happen — the column
			// is NOT NULL — so this is corruption, not an edge case, and
			// guessing a bucket for it would fabricate a count.
			return nil, fmt.Errorf("%w: visible edge of type %q has no status",
				ErrUnknownDependencyStatus, depType)
		}
		bump, known := depCountStatusBuckets[*status]
		if !known {
			return nil, fmt.Errorf("%w: %q (type %q). A status was added to the schema "+
				"without a bucket here; the counters would under-report it and "+
				"«is the wave finished» would silently become wrong",
				ErrUnknownDependencyStatus, *status, depType)
		}
		bump(&cell, n)
		side[depType] = cell
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating dep counts: %w", err)
	}
	return out, nil
}

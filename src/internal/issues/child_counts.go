package issues

// child_counts.go — the US-3 child counters (sk-282 T012).
//
// # What question these answer
//
// «Is the group finished?» An agent reading its group node wants two
// numbers and gets neither today: the epic rollup exists only for issues
// of type «epic», and the head issue of a run is usually an ordinary task.
// One measured session called the list tool with a page size of one twelve
// times just to read a total.
//
// # Why these are NOT the epic rollup, and why both may exist
//
// They count different things on purpose, and INV-10 requires the
// difference to be checkable rather than merely explainable:
//
//   - the epic rollup counts children strictly in status «open»;
//   - unclosedChildrenCount counts children in ANY status other than
//     closed, because a group is held by «in progress» and «deferred» just
//     as much as by «open». Reusing the old name for the wider meaning was
//     rejected: two fields with near-identical names and different
//     semantics is worse than one longer name.
//
// Standing children are excluded from both counters — a standing task is a
// perpetual obligation, not a unit of work, and it has no place in the
// denominator of «done / total». That matches the existing rollup's rule.
//
// # Visibility, and the tail that must not vanish
//
// Children in projects the caller cannot see are NOT counted here. That is
// not «count only what is visible» — the rejected variant — because they
// do not disappear: they are counted by the dependency counters, in the
// `unavailable` bucket of the down/parent-child cell. Splitting them that
// way is what makes INV-10's identity hold:
//
//	childrenCount = down/parent-child total − standing bucket − unavailable
//
// and what makes «the wave is finished» answerable only when
// unclosedChildrenCount is zero AND that unavailable bucket is empty. A
// non-zero unavailable means «cannot say», not «yes».
//
// # Why this lives in the store and not on IssueDetail
//
// INV-2: no REST response changes its field set, including additively. So
// the counters are a SIDE value the MCP path asks for, never a field of
// the shared card (D-2). The cost is paid only where it was requested.

import (
	"context"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
)

// ChildCounts is the US-3 pair, re-exported from internal/childcount so
// callers of this store do not have to import the counting package too.
//
// The counting itself is NOT here: three list paths and this card path all
// need the same two numbers, and a second copy of the predicate is how
// «how many children» acquires more than one answer. See
// internal/childcount for the fragment and for why it is a side value
// rather than a column.
type ChildCounts = childcount.Counts

// CountChildren returns the US-3 counters for one issue.
//
// A thin delegation on purpose: the card path must not be able to drift
// away from the list paths, so it does not own a query.
func (s *Store) CountChildren(
	ctx context.Context,
	issueID string,
	scope authz.Scope,
) (ChildCounts, error) {
	return childcount.ForOne(ctx, s.pool, issueID, scope)
}

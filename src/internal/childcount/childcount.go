// Package childcount is the ONE place the US-3 child counters are counted
// (sk-282 T013).
//
// # Why a package of its own
//
// Three different SQL paths produce the issue row an agent reads —
// issues.ListIssues (project-mode list, and prime's «in progress» /
// «blocked» sections), issues.ListAllIssues (cross-project list) and
// dependencies.ListReady (prime's «ready to take») — and the same two
// numbers have to appear on all of them. Counting them three times is not
// a duplication smell, it is a correctness bug waiting: the moment one
// copy forgets to exclude standing children or applies visibility
// differently, «how many children» has three answers and INV-10's
// identity stops holding BETWEEN surfaces, which no per-surface test would
// catch.
//
// So the counting fragment lives here, in a leaf package both
// internal/issues and internal/dependencies can import without a cycle,
// and every caller — including the single-issue card path — goes through
// it.
//
// # Why a side value and not a column on the row
//
// INV-2 forbids changing a REST response's field set, including
// additively, and issues.IssueSummary / ListResult are what the REST list
// endpoints serialise. D-2 therefore fixes the shape: the counters come
// back as a SIDE VALUE — a map keyed by issue id — that only the MCP path
// asks for. A REST list never calls this, so it cannot leak there by
// accident: there is nothing to switch off.
//
// It also keeps the three list queries byte-identical to what they were,
// which is what makes INV-8's «with the flag off the execution plan is
// unchanged» true by construction rather than by measurement.

package childcount

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
	"skein-api/internal/status"
)

// Counts is the US-3 pair for one issue.
//
// Both numbers are always meaningful. Zero means «counted, and there are
// none» — never «not counted» — which is why For returns an entry for
// every id it was asked about, including the ones with no children at all.
type Counts struct {
	// Total is every child except standing ones. A standing task is a
	// perpetual obligation, not a unit of work, and has no place in the
	// denominator of «done / total» — the same rule the epic rollup uses.
	Total int
	// Unclosed is every child except standing and closed ones — the
	// number that answers «is the wave finished?». Wider than the epic
	// rollup's open-children on purpose: a wave is held by «in progress»
	// and «deferred» too.
	Unclosed int
}

// Querier is the narrow slice of pgx this package needs. Taking an
// interface rather than *pgxpool.Pool lets a caller pass a transaction and
// keeps this package free of a pool dependency.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// countSQL is THE fragment. It is a package-level string rather than an
// inline literal so that «all three carriers count the same way» is a
// property anyone can check by grep, and so a future change cannot reach
// one caller and miss another.
//
// The join to issues is not optional: the status filter and the visibility
// predicate both live on the CHILD row, so counting from the dependencies
// table alone would have to trust the edge and could neither exclude
// standing children nor respect the caller's horizon.
//
// The «unclosed» leg goes through status.ActiveWork, which IS «neither
// closed nor standing» — exactly US-3's definition — so a future change to
// what counts as active work reaches this counter without anyone
// remembering it, and the repository's static guard against raw status
// predicates stays satisfied by construction.
var countSQL = fmt.Sprintf(`
SELECT d.to_id,
       count(*) FILTER (WHERE c.status <> 'standing'),
       count(*) FILTER (WHERE %s)
  FROM dependencies d
  JOIN issues c ON c.id = d.from_id
 WHERE d.to_id = ANY($1::text[])
   AND d.type = 'parent-child'
   AND %s
 GROUP BY d.to_id`, status.ActiveWork("c"), authz.ProjectVisibleExpr("c", "$2", "$3"))

// For counts the children of every id in parentIDs.
//
// scope is the caller's horizon (authz) — the same one the dependency
// counters use, because INV-10's identity only holds if the two aggregates
// agree about what this caller may see. A child in a project the caller
// cannot see is NOT counted here, and it does not
// vanish either: the dependency counters carry it in the `unavailable`
// bucket of the down/parent-child cell. That split is the whole reason
// «the wave is finished» may only be concluded when the unclosed count is
// zero AND that bucket is empty.
//
// The returned map has an entry for EVERY requested id. Materialising the
// zeros here rather than at each call site is deliberate: three call sites
// each remembering to default a missing key is three chances to emit
// «absent» where US-3 requires «0», and absent reads as «not counted».
func For(
	ctx context.Context,
	q Querier,
	parentIDs []string,
	scope authz.Scope,
) (map[string]Counts, error) {
	out := make(map[string]Counts, len(parentIDs))
	for _, id := range parentIDs {
		out[id] = Counts{}
	}
	if len(parentIDs) == 0 {
		return out, nil
	}

	unrestricted, caller, bindErr := scope.Bind(ctx, q)
	if bindErr != nil {
		return nil, bindErr
	}
	rows, err := q.Query(ctx, countSQL, parentIDs, unrestricted, caller)
	if err != nil {
		return nil, fmt.Errorf("counting children: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			parentID string
			counts   Counts
		)
		if err := rows.Scan(&parentID, &counts.Total, &counts.Unclosed); err != nil {
			return nil, fmt.Errorf("scanning child counts: %w", err)
		}
		// Guard against a row for an id nobody asked about — it would mean
		// the predicate drifted, and silently adding it to the map would
		// hide that.
		if _, asked := out[parentID]; !asked {
			return nil, fmt.Errorf("child counts returned an unrequested parent %q", parentID)
		}
		out[parentID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating child counts: %w", err)
	}
	return out, nil
}

// ForOne is the single-issue convenience used by the issue card. It exists
// so the card path cannot grow its own query: same fragment, same rules.
func ForOne(
	ctx context.Context,
	q Querier,
	parentID string,
	scope authz.Scope,
) (Counts, error) {
	all, err := For(ctx, q, []string{parentID}, scope)
	if err != nil {
		return Counts{}, err
	}
	return all[parentID], nil
}

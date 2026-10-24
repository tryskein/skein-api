// Package hierarchy owns the ONE definition of "ancestor" in the issue tree
// (spec sk-622.21, US-3, INV-3, decisions D3 / K1):
//
//	X is an ancestor of T  ⇔  X is in the chain of canonical parents of T.
//
// The canonical parent of an issue is its FIRST parent-child edge by
// created_at — the same rule the API exposes as `parentIdCurrent`
// (issues/store.go#parentIDCurrentExpr). Historical second parent-child
// edges, if the data still has them, are never a parent. The chain is
// followed to the root with no depth limit; the hierarchy is acyclic by
// construction (the cycle check on edge creation), and if the data closes a
// loop anyway the walk stops at the first repeated issue and the loop is
// reported with [LogCycle].
//
// Subscription fan-out (T012), the Watching card (T012), the agent monitor
// with subtree=true (T013) and the reconciliation job (T020) all use the SQL
// below, so "subscribed to X with its subtree" means the same thing on every
// path. The chain may cross projects (edges allow it on creation); access is
// decided by the consumer, never here.
//
// Cost: one index seek on dependencies (from_id, type) per level — O(depth)
// per issue, evaluated at query time, so it is always right after a reparent.
package hierarchy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
)

// canonicalParentLateral is the per-level step: the canonical parent of the
// issue named by the SQL expression %s. Byte-for-byte the rule of
// issues/store.go#parentIDCurrentExpr.
const canonicalParentLateral = `SELECT d.to_id FROM dependencies d
              WHERE d.from_id = %s AND d.type = 'parent-child'
              ORDER BY d.created_at LIMIT 1`

// AncestorsCTE returns one element of a `WITH RECURSIVE` list, named name,
// that yields the ancestor chain of every issue produced by seedSQL.
//
// seedSQL must be a SELECT returning exactly one text column named
// `issue_id` (for example `SELECT $1::text AS issue_id`, or a SELECT over
// the issues of a batch). The element has the columns
//
//	issue_id    text     — the seed issue the row belongs to
//	ancestor_id text     — the issue at this level; the seed itself at depth 0
//	depth       integer  — 0 for the seed, 1 for its canonical parent, …
//	is_cycle    boolean  — true on the single row where the chain repeats an
//	                       issue already on it; the walk stops there
//	path        record[] — PostgreSQL's CYCLE bookkeeping; ignore it
//
// Proper ancestors are the rows with depth >= 1 AND NOT is_cycle. A row with
// is_cycle = true means the data holds a loop: its ancestor_id is the
// repeated issue, and the consumer must report it with [LogCycle] (the SQL
// cannot log). The seed is put on the path at depth 0, so a loop back to the
// seed is detected rather than listing the seed as its own ancestor.
//
// Usage:
//
//	sql := "WITH RECURSIVE " + hierarchy.AncestorsCTE("anc", "SELECT $1::text AS issue_id") +
//	       " SELECT ancestor_id FROM anc WHERE depth >= 1 AND NOT is_cycle"
//
// name and seedSQL are spliced into the statement verbatim; pass constants,
// never user input (values go through placeholders inside seedSQL).
func AncestorsCTE(name, seedSQL string) string {
	return fmt.Sprintf(`%[1]s(issue_id, ancestor_id, depth) AS (
    SELECT s.issue_id::text, s.issue_id::text, 0
      FROM (%[2]s) s
    UNION ALL
    SELECT a.issue_id, p.to_id::text, a.depth + 1
      FROM %[1]s a
      CROSS JOIN LATERAL (`+fmt.Sprintf(canonicalParentLateral, "a.ancestor_id")+`) p
) CYCLE ancestor_id SET is_cycle USING path`, name, seedSQL)
}

// IsAncestorExpr returns a boolean SQL expression that is true when the
// issue named by ancestorExpr is a proper ancestor of the issue named by
// issueExpr — INV-3's "T is a descendant of X". Both arguments are SQL
// expressions of type text (a column such as `iw.issue_id`, or a
// placeholder such as `$2`). The issue is never its own ancestor; on a loop
// in the data the walk stops at the repetition, so the expression always
// terminates (it does not log — use [AncestorsCTE] + [LogCycle] where a loop
// must be reported).
//
// Arguments are spliced verbatim; pass column references or placeholders,
// never user input.
func IsAncestorExpr(ancestorExpr, issueExpr string) string {
	return fmt.Sprintf(`EXISTS (
    WITH RECURSIVE %[3]s
    SELECT 1 FROM hier_anc
     WHERE hier_anc.depth >= 1
       AND NOT hier_anc.is_cycle
       AND hier_anc.ancestor_id = %[1]s
)`, ancestorExpr, issueExpr,
		AncestorsCTE("hier_anc", "SELECT ("+issueExpr+")::text AS issue_id"))
}

// Ancestor is one proper ancestor of an issue.
type Ancestor struct {
	// IssueID is the ancestor's id.
	IssueID string
	// Depth is 1 for the canonical parent, 2 for its parent, and so on.
	Depth int
}

// Querier is the subset of *pgxpool.Pool / pgx.Tx that Ancestors needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var ancestorsSQL = "WITH RECURSIVE " + AncestorsCTE("anc", "SELECT $1::text AS issue_id") + `
SELECT ancestor_id, depth, is_cycle FROM anc WHERE depth >= 1 ORDER BY depth`

// Ancestors returns the proper ancestors of issueID, nearest first, the
// whole chain whatever its depth. An issue without a parent (or an unknown
// id) has none. When the data closes a loop, the walk stops at the first
// repeated issue, the chain read so far is returned, and the loop is
// reported with [LogCycle] — a loop is a data defect to be investigated, not
// a reason to fail the caller.
func Ancestors(ctx context.Context, q Querier, issueID string) ([]Ancestor, error) {
	rows, err := q.Query(ctx, ancestorsSQL, issueID)
	if err != nil {
		return nil, fmt.Errorf("reading ancestors of %s: %w", issueID, err)
	}
	defer rows.Close()

	var (
		out      []Ancestor
		repeated string
	)
	for rows.Next() {
		var (
			id      string
			depth   int
			isCycle bool
		)
		if err := rows.Scan(&id, &depth, &isCycle); err != nil {
			return nil, fmt.Errorf("scanning ancestors of %s: %w", issueID, err)
		}
		if isCycle {
			repeated = id
			continue
		}
		out = append(out, Ancestor{IssueID: id, Depth: depth})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading ancestors of %s: %w", issueID, err)
	}
	if repeated != "" {
		chain := make([]string, 0, len(out)+1)
		chain = append(chain, issueID)
		for _, a := range out {
			chain = append(chain, a.IssueID)
		}
		LogCycle(ctx, issueID, repeated, chain)
	}
	return out, nil
}

// LogCycle reports a loop found in the canonical-parent chain of issueID: an
// error record (JSON through the service's DualHandler) carrying the issue
// whose walk found it, the repeated issue, and the chain walked from issueID
// up to the repetition. Every consumer that reads is_cycle from
// [AncestorsCTE] reports through this one function, so the record has one
// shape for whoever searches for it.
func LogCycle(ctx context.Context, issueID, repeatedIssueID string, chain []string) {
	slog.ErrorContext(ctx, "issue hierarchy cycle: canonical parent chain repeats an issue",
		"issueId", issueID,
		"repeatedIssueId", repeatedIssueID,
		"chain", strings.Join(chain, " -> "),
		"error", "hierarchy cycle at "+repeatedIssueID,
	)
}

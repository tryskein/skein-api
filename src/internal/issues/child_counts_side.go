package issues

// child_counts_side.go — the side channel that carries the US-3 counters
// alongside a list result (sk-282 T013).
//
// # The class, and why it is a class
//
// Three SQL paths produce the row an agent reads, and all three must be
// able to carry the same two numbers:
//
//	ListIssues     — skein_list in project mode, and skein_prime's
//	                 «in progress» / «blocked» sections
//	ListAllIssues  — skein_list in cross-project mode; its own SELECT over
//	                 the same row shape, so without this it would ship the
//	                 cross-project list without counters
//	ListReady      — skein_prime's «ready to take» (dependencies package)
//
// Doing one of them is worse than doing none: «how many children» would
// answer differently depending on which surface asked, and INV-10's
// identity would stop holding between surfaces while every per-surface
// test stayed green.
//
// # The shape, and why it is not a field
//
// D-2 / INV-2: the counters may not become a field of IssueSummary or
// ListResult — those are what the REST list endpoints serialise, and INV-2
// forbids even an additive key there. So they ride beside the result as a
// map keyed by issue id, produced by a method the MCP path calls and the
// REST path does not. There is no flag to leave switched on by mistake:
// the REST handlers simply never call this, and the list SQL is untouched.

import (
	"context"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
)

// ChildCountsForRows counts the children of every row in a list result.
//
// The MCP list paths call it immediately after the list query, with the
// same scope they listed under. One round trip for the whole
// page, not one per row.
func (s *Store) ChildCountsForRows(
	ctx context.Context,
	rows []IssueSummary,
	scope authz.Scope,
) (map[string]ChildCounts, error) {
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	return childcount.For(ctx, s.pool, ids, scope)
}

package dependencies

// child_counts_side.go — the «ready to take» carrier's half of the US-3
// child-counter class (sk-282 T013).
//
// ListReady produces a different row type from the issues package's list
// paths (dependencies.IssueSummary, kept here to avoid an
// issues↔dependencies import cycle), so it needs its own entry point. It
// must NOT get its own query: the counting fragment lives in
// internal/childcount and all three carriers go through it, or «how many
// children» acquires more than one answer.
//
// Only prime's «ready to take» section uses this. The standalone
// skein_ready output is deliberately NOT in the class — US-9 declares the
// rest of its row unchanged and asks for no counters there.

import (
	"context"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
)

// ChildCountsForReady counts the children of every row a ListReady call
// returned, under the same visibility horizon.
func (s *Store) ChildCountsForReady(
	ctx context.Context,
	rows []IssueSummary,
	scope authz.Scope,
) (map[string]childcount.Counts, error) {
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	return childcount.For(ctx, s.pool, ids, scope)
}

package authz

import (
	"context"
	"fmt"
)

// VisibleIssueIDs returns which of ids are issues the scope sees now: the
// issue row exists and its project is in the scope's visible set — the
// access relation narrowed by the key's scope (Scope.Bind). It is the one
// definition of "this issue is visible to the reader" for every surface
// that holds issue ids (the tree's outside parents, event data, cycle
// paths, close refusals, idempotent replays).
//
// An id without a row in issues — never existed or deleted — is never in
// the result, so a deleted issue is indistinguishable from a hidden one.
// An empty ids reads nothing. The empty scope (no principal, no
// WithUnrestricted) sees nothing; declared internal work sees every
// existing issue. Duplicates in ids are answered once.
func VisibleIssueIDs(ctx context.Context, q RowsQuerier, s Scope, ids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	unrestricted, visible, err := s.Bind(ctx, q)
	if err != nil {
		return nil, err
	}
	if !unrestricted && len(visible) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
SELECT i.id FROM issues i
 WHERE i.id = ANY($1::text[]) AND `+ProjectVisibleExpr("i", "$2", "$3"),
		ids, unrestricted, visible)
	if err != nil {
		return nil, fmt.Errorf("checking visibility of issues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning visible issue: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading visible issues: %w", err)
	}
	return out, nil
}

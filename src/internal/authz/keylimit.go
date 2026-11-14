package authz

import (
	"context"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/authz/authzsql"
)

// KeyLimit is the scope of the API key a request authenticated with
// (sk-878.3.1 D-3, INV-1, INV-2) — see authzsql.KeyLimit.
type KeyLimit = authzsql.KeyLimit

// Placeholders — see authzsql.Placeholders.
func Placeholders(args *[]any) func(value any) string { return authzsql.Placeholders(args) }

// keyLimitFromContext reads the limit of the request's key: none without a
// key id, the grant's scope with one, denied for a key id without a grant —
// absence of the grant never means "no limit".
func keyLimitFromContext(ctx context.Context) KeyLimit {
	if _, isKey := auth.APIKeyIDFrom(ctx); !isKey {
		return KeyLimit{}
	}
	g, ok := auth.KeyGrantFrom(ctx)
	if !ok {
		return authzsql.DeniedKeyLimit()
	}
	return authzsql.KeyLimitOf(g)
}

// KeyReaches returns which of projectIDs lie inside the key limit k. Without
// a limit every id is reached and nothing is read; a denied limit reaches
// none. It checks the scope only, not the owner's relation.
func KeyReaches(ctx context.Context, q RowsQuerier, k KeyLimit, projectIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(projectIDs))
	if !k.Limits() {
		for _, id := range projectIDs {
			out[id] = true
		}
		return out, nil
	}
	if len(projectIDs) == 0 {
		return out, nil
	}
	args := []any{projectIDs}
	ids, err := loadIDsArgs(ctx, q, `SELECT x.id FROM unnest($1::uuid[]) AS x(id) WHERE `+
		k.Expr("x.id", Placeholders(&args)), "projects inside the key scope", args...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

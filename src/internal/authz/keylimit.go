package authz

import (
	"context"

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

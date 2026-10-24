package projects

import (
	"context"

	"skein-api/internal/authz"
)

// storeFixtureCtx is the context a STORE-LEVEL fixture reads with: no
// transport, no principal, and the whole tree in view.
//
// Since sk-243's security follow-up an undeclared context sees nothing —
// "no principal" is no longer silently equivalent to "admin", because that
// equivalence turned any future dropped context value into cross-project
// disclosure. Server-internal readers say what they are instead, and a
// fixture exercising a projection rather than an authorisation rule is one
// of them. Tests about the horizon build a real caller context instead.
func storeFixtureCtx() context.Context {
	return authz.WithUnrestricted(context.Background())
}

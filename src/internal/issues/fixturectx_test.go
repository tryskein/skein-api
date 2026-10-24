package issues

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
// disclosure. Server-internal readers say what they are instead, and a test
// that is exercising a projection rather than an authorisation rule is one
// of them.
//
// Tests that are about the horizon itself do NOT use this: they build a
// real caller context with callerCtx (see epic_rollup_visibility_test.go),
// which is the shape a REST or MCP request actually arrives in.
func storeFixtureCtx() context.Context {
	return authz.WithUnrestricted(context.Background())
}

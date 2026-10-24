package auth

import (
	"context"

	"github.com/google/uuid"
)

// Principal is the centralized identity primitive (sk-163 S1). It carries
// everything an authorization decision needs about the caller — who they
// are (UserID), their stored role, and whether they are an EXTERNAL
// collaborator. Both transports (REST trust-headers and MCP api-key) resolve
// a Principal the same way via PrincipalFrom, so the admin-ceiling
// derivation lives in exactly one place.
//
// The single derivation point is IsAdmin(): a user is an admin only when
// their stored role is "admin" AND they are not external. is_external is the
// visibility ceiling — an external user is NEVER an admin regardless of what
// the `role` column says (a tampered row is also blocked at the DB layer by
// the users_external_not_admin CHECK from migration 000033, so the two
// guards are belt-and-braces).
//
// S1 ADDED this primitive and made is_external available on the
// principal/ctx. S2 replaced the ~30 raw role-comparison call sites with
// p.IsAdmin() (or the IsAdminRole helper for store-layer args structs that
// carry the two fields separately) so the ceiling derivation lives here and
// nowhere else.
type Principal struct {
	UserID     uuid.UUID
	Role       string
	IsExternal bool
}

// RoleAdmin is the stored users.role value that grants org-admin authority.
// Defined as a named constant so the admin-ceiling derivation references a
// symbol rather than a bare string literal — the S2 acceptance grep for raw
// role-equality comparisons then matches only legitimate sites (guards,
// role-value validation), never the centralized derivation.
const RoleAdmin = "admin"

// IsAdmin reports whether the principal should resolve to admin authority.
// External users are clamped to non-admin unconditionally — this is the
// ceiling invariant from spec §5. Implemented as a method (not a raw field
// comparison at the call site) so a grep for the admin check finds one
// authoritative definition.
func (p Principal) IsAdmin() bool {
	return p.Role == RoleAdmin && !p.IsExternal
}

// IsAdminRole is the (role, isExternal) form of the admin-ceiling derivation
// for call sites that carry the two fields separately — store-layer args
// structs and DB-reread paths that never assemble a full Principal. It
// delegates to Principal.IsAdmin so the clamp logic stays defined in exactly
// one place.
func IsAdminRole(role string, isExternal bool) bool {
	return Principal{Role: role, IsExternal: isExternal}.IsAdmin()
}

// PrincipalFrom assembles the caller's Principal from the request context.
// It reads the values stashed by ParseUserHeaders (REST) or RequireAPIKey
// (MCP) — both populate the same ctx keys, so this works identically across
// transports. The bool mirrors UserIDFrom: false when no user identity is
// present in ctx (e.g. an /auth/login request before authentication).
//
// Role defaults to "user" when unset so a missing role header can never be
// mistaken for an empty-string admin bypass; IsExternal defaults to false
// (absent header → internal), matching the column default so the slice is a
// safe no-op for existing callers.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	id, ok := UserIDFrom(ctx)
	if !ok {
		return Principal{}, false
	}
	role, _ := UserRoleFrom(ctx)
	if role == "" {
		role = "user"
	}
	return Principal{
		UserID:     id,
		Role:       role,
		IsExternal: IsExternalFrom(ctx),
	}, true
}

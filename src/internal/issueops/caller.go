package issueops

// caller.go — who asks for an operation, from the request context.

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// CallerRole is the role of the caller (admin or user) from ctx, user when
// the authentication left none (logged: a wiring regression).
func CallerRole(ctx context.Context) string {
	if role, ok := auth.UserRoleFrom(ctx); ok && role != "" {
		return role
	}
	slog.WarnContext(ctx, "auth role missing in mcp tool ctx", "defaulted_to", "user")
	return "user"
}

// CallerOriginMode classifies the caller as "agent" (an API key
// authenticated the request) or "human" (a session did); ok is false when
// neither signal is present — fail-closed, so a write is never silently
// recorded as a human's.
func CallerOriginMode(ctx context.Context) (string, bool) {
	if id, ok := auth.APIKeyIDFrom(ctx); ok && id != uuid.Nil {
		return "agent", true
	}
	if _, ok := auth.UserIDFrom(ctx); ok {
		return "human", true
	}
	return "", false
}

// LooksLikeHierarchicalID reports whether id has the shape of a full
// hierarchical issue id ("demo-42"): bare numbers are refused.
func LooksLikeHierarchicalID(id string) bool {
	return id != "" && strings.Contains(id, "-")
}

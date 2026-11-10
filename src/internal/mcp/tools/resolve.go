package tools

// resolve.go — the project handle resolution of the tools; it lives in
// issueops, shared with the public API (sk-878.3.1 T024a).

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issueops"
	"skein-api/internal/projects"
)

// AmbiguousProjectError — see issueops.AmbiguousProjectError.
type AmbiguousProjectError = issueops.AmbiguousProjectError

// ValidationProjectError — see issueops.ValidationProjectError.
type ValidationProjectError = issueops.ValidationProjectError

// ProjectNotFoundError — see issueops.ProjectNotFoundError.
type ProjectNotFoundError = issueops.ProjectNotFoundError

// ResolveProjectHandle resolves a free-form project handle — see
// issueops.ResolveProjectHandle.
func ResolveProjectHandle(ctx context.Context, pool *pgxpool.Pool, store *projects.Store, handle string, userID uuid.UUID) (uuid.UUID, error) {
	return issueops.ResolveProjectHandle(ctx, pool, store, handle, userID)
}

// resolveVisibleIssueProject — see issueops.ResolveVisibleIssueProject.
func resolveVisibleIssueProject(ctx context.Context, pool *pgxpool.Pool, toolName, issueID string, userID uuid.UUID) (uuid.UUID, *mcpgo.CallToolResult) {
	id, f := issueops.ResolveVisibleIssueProject(ctx, pool, toolName, issueID, userID)
	return id, toolFailure(f)
}

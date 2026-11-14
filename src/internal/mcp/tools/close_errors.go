package tools

// close_errors.go — the classification of close refusals; it lives in
// issueops, shared with the public API (sk-878.3.1 T024a).

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issueops"
)

type closeErrorClassification = issueops.CloseErrorClassification

func repoResolutionDetails(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, tool string) map[string]any {
	return issueops.RepoResolutionDetails(ctx, pool, projectID, tool)
}

func classifyCloseError(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, err error, tool, userID, issueID, closeTarget string) closeErrorClassification {
	return issueops.ClassifyCloseError(ctx, pool, projectID, err, tool, userID, issueID, closeTarget)
}

func classifyCloseRowError(ctx, loopCtx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, err error, userID, issueID, closeTarget string) closeErrorClassification {
	return issueops.ClassifyCloseRowError(ctx, loopCtx, pool, projectID, err, userID, issueID, closeTarget)
}

func classifyCloseTargetError(err error, closeTarget string) *closeErrorClassification {
	return issueops.ClassifyCloseTargetError(err, closeTarget)
}

func mapCloseError(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, err error, tool, userID, issueID, closeTarget string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapCloseError(ctx, pool, projectID, err, tool, userID, issueID, closeTarget))
}

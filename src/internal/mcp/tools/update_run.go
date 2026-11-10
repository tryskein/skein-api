package tools

// update_run.go — the update chain of one issue; it lives in issueops,
// shared with the public API (sk-878.3.1 T024a).

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issueops"
	"skein-api/internal/issues"
)

type errLabelNotPresent = issueops.ErrLabelNotPresent

type updateOneInput = issueops.UpdateOneInput

func runUpdateForOne(ctx context.Context, store *issues.Store, in updateOneInput) (*issues.IssueDetail, issues.CloseLinkState, error) {
	return issueops.RunUpdateForOne(ctx, store, in)
}

func runUpdateChainVersion(ctx context.Context, store *issues.Store, in updateOneInput) (int, error) {
	return issueops.RunUpdateChainVersion(ctx, store, in)
}

func runUpdateChain(ctx context.Context, store *issues.Store, in updateOneInput) (issues.CloseLinkState, issues.CommittedState, error) {
	return issueops.RunUpdateChain(ctx, store, in)
}

func refuseOutcomeOnNonClosingStatus(outcome closeOutcomeArgs, statusCanon, id string) *mcpgo.CallToolResult {
	return toolFailure(issueops.RefuseOutcomeOnNonClosingStatus(outcome, statusCanon, id))
}

func buildUpdateOneInput(args map[string]any, pool *pgxpool.Pool, ctx context.Context, id string, expectedVersion int, force bool, sessionID string, actor issues.Actor, depsCaller *issues.DependenciesCaller) (updateOneInput, *mcpgo.CallToolResult) {
	in, f := issueops.BuildUpdateOneInput(args, pool, ctx, id, expectedVersion, force, sessionID, actor, depsCaller)
	return in, toolFailure(f)
}

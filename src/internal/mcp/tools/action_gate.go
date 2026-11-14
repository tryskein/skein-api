package tools

// action_gate.go — the action gates of the tools; they live in issueops,
// shared with the public API (sk-878.3.1 T024a).

import (
	"context"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/authz"
	"skein-api/internal/issueops"
)

const msgActionForbidden = issueops.MsgActionForbidden

const msgForceUnlockForbidden = issueops.MsgForceUnlockForbidden

func issueActionGate(ctx context.Context, db authz.Querier, tool, idKey, issueID string, projectID, userID uuid.UUID, actions ...authz.Action) *mcpgo.CallToolResult {
	return toolFailure(issueops.IssueActionGate(ctx, db, tool, idKey, issueID, projectID, userID, actions...))
}

func projectActionGate(ctx context.Context, db authz.Querier, tool, projectArg string, projectID, userID uuid.UUID, actions ...authz.Action) *mcpgo.CallToolResult {
	return toolFailure(issueops.ProjectActionGate(ctx, db, tool, projectArg, projectID, userID, actions...))
}

func forceUnlockGate(ctx context.Context, db authz.Querier, tool, idKey, issueID string, projectID, userID uuid.UUID) *mcpgo.CallToolResult {
	return toolFailure(issueops.ForceUnlockGate(ctx, db, tool, idKey, issueID, projectID, userID))
}

func assigneeNotAllowedResult(fields map[string]any) *mcpgo.CallToolResult {
	return toolFailure(issueops.AssigneeNotAllowed(fields))
}

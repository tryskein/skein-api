package tools

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/authz"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// msgActionForbidden is the refusal of an action the caller's level does not
// allow in a project he sees (the action table of US-6). Since sk-741 the
// instance admin has the owner's rights, so no level of a visible project
// is refused the issue writes; the refusal stays for the action table.
const msgActionForbidden = "your access to this project does not allow this action"

// issueActionGate is the gate of an issue write by the action table of US-6
// (sk-674.18 T066): nil when the caller's level on the issue's project
// allows every named action; otherwise the ready MCP envelope —
// issue_not_found when the project is out of reach (one answer with a
// missing issue, INV-3), forbidden when it is visible but the level does not
// allow the action. The same rule as REST (projects.EnsureProjectAction).
// idKey names the issue in the envelope's details as the tool's own
// arguments do ("id", "issueId", "fromId").
func issueActionGate(
	ctx context.Context,
	db authz.Querier,
	tool, idKey, issueID string,
	projectID, userID uuid.UUID,
	actions ...authz.Action,
) *mcpgo.CallToolResult {
	_, err := projects.EnsureProjectAction(ctx, db, projectID, userID, actions...)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{idKey: issueID})
	case errors.Is(err, projects.ErrProjectActionForbidden):
		return skmcp.ToolError(ErrCodeForbidden, msgActionForbidden, map[string]any{idKey: issueID})
	default:
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", tool, "reason", "action check failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "permission check failed", nil)
	}
}

// projectActionGate is issueActionGate for a write addressed to a project
// (creating issues in it): project_not_found when it is out of reach,
// forbidden when the level does not allow the action.
func projectActionGate(
	ctx context.Context,
	db authz.Querier,
	tool, projectArg string,
	projectID, userID uuid.UUID,
	actions ...authz.Action,
) *mcpgo.CallToolResult {
	_, err := projects.EnsureProjectAction(ctx, db, projectID, userID, actions...)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		res, _ := projectErrorResult(&ProjectNotFoundError{Query: projectArg})
		return res
	case errors.Is(err, projects.ErrProjectActionForbidden):
		return skmcp.ToolError(ErrCodeForbidden, msgActionForbidden, map[string]any{"project": projectArg})
	default:
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", tool, "reason", "action check failed",
			"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "permission check failed", nil)
	}
}

// msgForceUnlockForbidden is the refusal of force=true where the caller may
// not release another holder's claim (US-16, K-28).
const msgForceUnlockForbidden = "force requires the right to release another holder's claim: the project owner, the space owner or the instance admin"

// forceUnlockGate is the gate of every path that bypasses a claim with
// force=true (sk-674.18 T034, K-28): nil when the caller holds
// authz.ActionForceUnlock on the issue's project; otherwise
// issue_not_found when the project is out of reach (INV-3) and
// force_unlock_forbidden when it is visible but the level does not allow
// it. The same right as REST (handler/issues.go#requireForceUnlock).
func forceUnlockGate(
	ctx context.Context,
	db authz.Querier,
	tool, idKey, issueID string,
	projectID, userID uuid.UUID,
) *mcpgo.CallToolResult {
	_, err := projects.EnsureProjectAction(ctx, db, projectID, userID, authz.ActionForceUnlock)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{idKey: issueID})
	case errors.Is(err, projects.ErrProjectActionForbidden):
		return skmcp.ToolError(ErrCodeForceUnlockForbidden, msgForceUnlockForbidden, map[string]any{idKey: issueID})
	default:
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", tool, "reason", "force_unlock check failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "permission check failed", nil)
	}
}

// assigneeNotAllowedResult is the envelope of issues.ErrAssigneeNotAllowed
// (INV-30, sk-674.18 T028), one for every tool that writes an assignee.
func assigneeNotAllowedResult(fields map[string]any) *mcpgo.CallToolResult {
	return skmcp.ToolError(ErrCodeAssigneeNotAllowed,
		"the assignee chosen by id may not work in this project; nothing was changed", fields)
}

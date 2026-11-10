package issueops

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"skein-api/internal/authz"
	"skein-api/internal/projects"
)

// MsgActionForbidden is the refusal of an action the caller's level does not
// allow in a project he sees (the action table of US-6). Since sk-741 the
// instance admin has the owner's rights, so no level of a visible project
// is refused the issue writes; the refusal stays for the action table.
const MsgActionForbidden = "your access to this project does not allow this action"

// IssueActionGate is the gate of an issue write by the action table of US-6
// (sk-674.18 T066): nil when the caller's level on the issue's project
// allows every named action; otherwise the ready MCP envelope —
// issue_not_found when the project is out of reach (one answer with a
// missing issue, INV-3), forbidden when it is visible but the level does not
// allow the action. The same rule as REST (projects.EnsureProjectAction).
// idKey names the issue in the envelope's details as the tool's own
// arguments do ("id", "issueId", "fromId").
func IssueActionGate(
	ctx context.Context,
	db authz.Querier,
	tool, idKey, issueID string,
	projectID, userID uuid.UUID,
	actions ...authz.Action,
) *Failure {
	_, err := projects.EnsureProjectAction(ctx, db, projectID, userID, actions...)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		return Fail(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{idKey: issueID})
	case errors.Is(err, projects.ErrProjectActionForbidden):
		return Fail(ErrCodeForbidden, MsgActionForbidden, map[string]any{idKey: issueID})
	default:
		slog.ErrorContext(ctx, "issue operation error",
			"tool", tool, "reason", "action check failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return Fail(ErrCodeInternalError, "permission check failed", nil)
	}
}

// ProjectActionGate is issueActionGate for a write addressed to a project
// (creating issues in it): project_not_found when it is out of reach,
// forbidden when the level does not allow the action.
func ProjectActionGate(
	ctx context.Context,
	db authz.Querier,
	tool, projectArg string,
	projectID, userID uuid.UUID,
	actions ...authz.Action,
) *Failure {
	_, err := projects.EnsureProjectAction(ctx, db, projectID, userID, actions...)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		return ProjectFailure(&ProjectNotFoundError{Query: projectArg})
	case errors.Is(err, projects.ErrProjectActionForbidden):
		return Fail(ErrCodeForbidden, MsgActionForbidden, map[string]any{"project": projectArg})
	default:
		slog.ErrorContext(ctx, "issue operation error",
			"tool", tool, "reason", "action check failed",
			"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
		return Fail(ErrCodeInternalError, "permission check failed", nil)
	}
}

// MsgForceUnlockForbidden is the refusal of force=true where the caller may
// not release another holder's claim (US-16, K-28).
const MsgForceUnlockForbidden = "force requires the right to release another holder's claim: the project owner, the space owner or the instance admin"

// ForceUnlockGate is the gate of every path that bypasses a claim with
// force=true (sk-674.18 T034, K-28): nil when the caller holds
// authz.ActionForceUnlock on the issue's project; otherwise
// issue_not_found when the project is out of reach (INV-3) and
// force_unlock_forbidden when it is visible but the level does not allow
// it. The same right as REST (handler/issues.go#requireForceUnlock).
func ForceUnlockGate(
	ctx context.Context,
	db authz.Querier,
	tool, idKey, issueID string,
	projectID, userID uuid.UUID,
) *Failure {
	_, err := projects.EnsureProjectAction(ctx, db, projectID, userID, authz.ActionForceUnlock)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		return Fail(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{idKey: issueID})
	case errors.Is(err, projects.ErrProjectActionForbidden):
		return Fail(ErrCodeForceUnlockForbidden, MsgForceUnlockForbidden, map[string]any{idKey: issueID})
	default:
		slog.ErrorContext(ctx, "issue operation error",
			"tool", tool, "reason", "force_unlock check failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return Fail(ErrCodeInternalError, "permission check failed", nil)
	}
}

// AssigneeNotAllowed is the envelope of issues.ErrAssigneeNotAllowed
// (INV-30, sk-674.18 T028), one for every tool that writes an assignee.
func AssigneeNotAllowed(fields map[string]any) *Failure {
	return Fail(ErrCodeAssigneeNotAllowed,
		"the assignee chosen by id may not work in this project; nothing was changed", fields)
}

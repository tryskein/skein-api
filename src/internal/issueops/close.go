package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"strings"
	"time"

	"github.com/google/uuid"
)

// close.go — the close of an issue (MCP skein_close, the public API's
// closeIssue): reason, outcome, target and commits in one transaction
// (US-14).

// CloseResult is the outcome of Close.
type CloseResult struct {
	// IssueID and ProjectID name the issue closed.
	IssueID   string
	ProjectID uuid.UUID
	// Issue is the issue after the close; nil when the read-back failed
	// (ReadBack).
	Issue *issues.IssueDetail
	// Link is the state of the close-target link.
	Link issues.CloseLinkState
	// ReadBack is set when the close committed but the issue could not be
	// read back.
	ReadBack *issues.ErrReadBackFailed
	// OpenChildren are the open children of an epic at the close, read
	// before it.
	OpenChildren []issues.BlockerRef
}

// Close closes an issue for the caller userID from the arguments of
// skein_close.
func Close(ctx context.Context, d Deps, userID uuid.UUID, a Args) (CloseResult, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	rawID, err := a.RequireString("id")
	if err != nil {
		return CloseResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return CloseResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"",
			map[string]any{"id": rawID})
	}

	expectedVersion := a.GetInt("expectedVersion", 0)
	if expectedVersion < 1 {
		return CloseResult{}, Fail(ErrCodeValidationError,
			"expectedVersion is required and must be >= 1", nil)
	}

	rawReason, err := a.RequireString("reason")
	if err != nil {
		return CloseResult{}, Fail(ErrCodeValidationError,
			"reason is required", nil)
	}
	reason := strings.TrimSpace(rawReason)
	if reason == "" {
		return CloseResult{}, Fail(ErrCodeValidationError,
			"reason must be non-empty after trim", nil)
	}
	if vErr := issues.ValidateProse("reason", reason); vErr != nil {
		return CloseResult{}, ValidationFailure(vErr)
	}

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// per issue on its project (forceUnlockGate), not by the role.

	commits, parseErr := ParseCommitInputs(a.GetArguments()["commits"], "commits")
	if parseErr != nil {
		return CloseResult{}, parseErr
	}

	// sk-489 (US-1, US-2). One of FOUR input builders: the package
	// does not go in here, so the same helper sits in
	// close_many.go#parseCloseManyItem — see close_outcome_arg.go.
	outcome, outcomeErr := ParseCloseOutcomeArgs(a.GetArguments(), id, "")
	if outcomeErr != nil {
		return CloseResult{}, outcomeErr
	}

	// Visibility.
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return CloseResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_close",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return CloseResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if f := IssueActionGate(ctx, d.Pool, "skein_close", "id", id, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return CloseResult{}, f
	}
	if force {
		if f := ForceUnlockGate(ctx, d.Pool, "skein_close", "id", id, issue.ProjectID, userID); f != nil {
			return CloseResult{}, f
		}
	}

	sessionID := auth.SessionIDFrom(ctx)
	actor := IssuesAgentActor(ctx, d.Pool, userID)

	// sk-32 slice 5: when the caller closes an epic that still has
	// open children, surface them on the response as a non-blocking
	// warning. The close itself proceeds regardless — the gate is
	// agent-facing UX, not a hard enforcement (deferred to sk-24).
	// We capture the snapshot BEFORE the close so the close
	// transition (which clears nothing about children, but is the
	// natural read-point) reflects what the agent saw at decision
	// time.
	var openChildren []issues.BlockerRef
	if issue.Type == issues.TypeEpic {
		oc, ocErr := d.Issues.EpicOpenChildrenFor(ctx, id)
		if ocErr != nil {
			slog.WarnContext(ctx, logMessage(ctx, "warning"),
				"tool", "skein_close",
				"reason", "epic open-children snapshot failed; closing without warning",
				"issueId", id, "error", ocErr.Error())
		} else {
			openChildren = oc
		}
	}

	dto, link, err := d.Issues.CloseWithResult(ctx, issues.CloseInput{
		ID:              id,
		ExpectedVersion: expectedVersion,
		Reason:          reason,
		Force:           force,
		SessionID:       sessionID,
		Actor:           actor,
		Commits:         commits,
		CloseOutcome:    outcome.Outcome,
		CloseTarget:     outcome.Target,
		// sk-540.1: an explicit outcome is mandatory on a transition.
		RequireOutcomeOnTransition: true,
		// The system role, not Actor.Role: the target visibility key is the
		// same as for a manual link (skein_dep_add), otherwise an admin and a
		// non-admin would get the same answer for an invisible target.
		DependenciesCaller: &issues.DependenciesCaller{UserID: userID, Role: role},
	})
	res := CloseResult{IssueID: id, ProjectID: issue.ProjectID, Link: link, OpenChildren: openChildren}
	if err != nil {
		if rb, committed := asReadBackFailed(err); committed {
			res.ReadBack = rb
			return res, nil
		}
		return CloseResult{}, MapCloseError(ctx, d.Pool, issue.ProjectID, err, "skein_close",
			userID.String(), id, outcome.Target)
	}
	res.Issue = dto
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", "skein_close",
		"userId", userID.String(), "issueId", id,
		"version", dto.Version,
		"latencyMs", time.Since(start).Milliseconds())
	return res, nil
}

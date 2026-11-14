package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"strings"
	"time"

	"github.com/google/uuid"
)

// lock.go — the operations on one issue that end in its card: unclaim,
// accept, reparent, the delegation drop and resume, the commits attach and
// detach (MCP skein_*, the public API's operations of the same names).

// IssueResult is the outcome of an operation on one issue.
type IssueResult struct {
	// IssueID is the issue the operation acted on.
	IssueID string
	// Issue is the issue after the operation.
	Issue *issues.IssueDetail
	// Detach is the report of DetachCommits (nil otherwise).
	Detach *issues.DetachCommitsResult
}

// Unclaim runs the operation of skein_unclaim for the caller userID.
func Unclaim(ctx context.Context, d Deps, userID uuid.UUID, a Args) (IssueResult, *Failure) {
	start := time.Now()

	rawID, err := a.RequireString("id")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"",
			map[string]any{"id": rawID})
	}

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// on the issue's project (forceUnlockGate), not by the role.

	// sk-19: nextStatus + reason are required. Validate at the
	// MCP boundary so failures short-circuit before the visibility
	// check + the SERIALIZABLE tx the store layer would open.
	nextStatusRaw, err := a.RequireString("nextStatus")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"nextStatus is required — one of: in_progress, open, deferred",
			nil)
	}
	nextStatus := strings.TrimSpace(nextStatusRaw)
	switch nextStatus {
	case "in_progress", "open", "deferred":
	case "closed":
		return IssueResult{}, Fail(ErrCodeValidationError,
			"nextStatus=closed is rejected — use skein_close to close an issue",
			map[string]any{"nextStatus": nextStatus})
	default:
		return IssueResult{}, Fail(ErrCodeValidationError,
			"nextStatus must be one of: in_progress, open, deferred",
			map[string]any{"nextStatus": nextStatus})
	}

	reasonRaw, err := a.RequireString("reason")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"reason is required (non-empty after trim)", nil)
	}
	reason := strings.TrimSpace(reasonRaw)
	if reason == "" {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"reason is required (non-empty after trim)", nil)
	}
	if vErr := issues.ValidateProse("reason", reason); vErr != nil {
		return IssueResult{}, ValidationFailure(vErr)
	}

	// Visibility check.
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_unclaim",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	// sk-674.18 T066: releasing one's own claim moves the issue's
	// status — ActionWrite of US-6. force=true releases ANOTHER
	// holder's claim: that is force_unlock (T034, K-28) — the project
	// owner, the space owner, the instance admin.
	if force {
		if f := ForceUnlockGate(ctx, d.Pool, "skein_unclaim", "id", id, issue.ProjectID, userID); f != nil {
			return IssueResult{}, f
		}
	} else if f := IssueActionGate(ctx, d.Pool, "skein_unclaim", "id", id, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return IssueResult{}, f
	}

	sessionID := auth.SessionIDFrom(ctx)
	actor := IssuesAgentActor(ctx, d.Pool, userID)

	dto, err := d.Issues.Unclaim(ctx, issues.UnclaimInput{
		ID:         id,
		SessionID:  sessionID,
		Force:      force,
		NextStatus: nextStatus,
		Reason:     reason,
		Actor:      actor,
	})
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		if errors.Is(err, issues.ErrNotLockHolder) {
			return IssueResult{}, Fail(ErrCodeNotLockHolder,
				"caller session does not hold the lock on this issue",
				map[string]any{"id": id})
		}
		if vErr, ok := issues.IsValidationError(err); ok {
			return IssueResult{}, Fail(ErrCodeValidationError, vErr.Message,
				map[string]any{"code": vErr.Code})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_unclaim",
			"reason", "store call failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "unclaim failed", nil)
	}

	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", "skein_unclaim",
		"userId", userID.String(), "issueId", id,
		"version", dto.Version,
		"latencyMs", time.Since(start).Milliseconds())
	return IssueResult{IssueID: id, Issue: dto}, nil
}

// Accept runs the operation of skein_accept for the caller userID.
func Accept(ctx context.Context, d Deps, userID uuid.UUID, a Args) (IssueResult, *Failure) {
	start := time.Now()
	rawID, err := a.RequireString("id")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	// INV-17 (sk-674.18 T029): the Inbox shows the caller's pending
	// space invitations too; accepting or declining one is a person's
	// act in the web UI, so an invitation id is refused with its own
	// code, not answered as a missing issue.
	if invID, perr := uuid.Parse(id); perr == nil {
		isInv, ierr := spaces.IsInvitationTo(ctx, d.Pool, invID, userID)
		if ierr != nil {
			slog.ErrorContext(ctx, logMessage(ctx, "error"),
				"tool", "skein_accept", "reason", "invitation lookup failed",
				"userId", userID.String(), "error", ierr.Error())
			return IssueResult{}, Fail(ErrCodeInternalError, "failed to read the invitation", nil)
		}
		if isInv {
			return IssueResult{}, Fail(ErrCodeInvitationAgentForbidden,
				"an agent cannot accept or decline a space invitation; the person does it in the web UI",
				map[string]any{"id": id})
		}
	}
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"",
			map[string]any{"id": rawID})
	}

	// expectedVersion is optional: GetInt returns 0 when the param
	// was omitted (matches MCP-go's `default` zero behaviour). We
	// translate 0/absent → nil ExpectedVersion so the store skips
	// the optimistic-concurrency check entirely. Any positive
	// value is honoured.
	var expectedVersionPtr *int
	if v := a.GetInt("expectedVersion", 0); v > 0 {
		expectedVersionPtr = &v
	}

	// Visibility check — must come before the store call so a
	// caller who can't see the project gets `issue_not_found`
	// rather than `forbidden` (matches the unified visibility
	// pattern used by skein_close / skein_unclaim).
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_accept",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, d.Pool, issue.ProjectID, userID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_accept",
			"reason", "visibility check failed",
			"userId", userID.String(), "issueId", id, "error", vErr.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "visibility check failed", nil)
	}

	// Actor.Role stays 'agent' (audit-trail actor type); only the
	// assignee accepts — there is no admin override (US-6).
	actor := IssuesAgentActor(ctx, d.Pool, userID)

	dto, err := d.Issues.Accept(ctx, issues.AcceptInput{
		ID:              id,
		ExpectedVersion: expectedVersionPtr,
		Actor:           actor,
	})
	if err != nil {
		return IssueResult{}, MapAcceptError(ctx, err, userID, id)
	}

	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", "skein_accept",
		"userId", userID.String(), "issueId", id,
		"version", dto.Version,
		"latencyMs", time.Since(start).Milliseconds())
	// The acceptance stamp is the ONLY proof this write applied —
	// accept changes no status and a repeat is a version-less no-op
	// — so this surface, and only this one, carries it. See
	// ackFromAcceptedIssue.
	return IssueResult{IssueID: id, Issue: dto}, nil
}

// Reparent runs the operation of skein_reparent for the caller userID.
func Reparent(ctx context.Context, d Deps, userID uuid.UUID, a Args) (IssueResult, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	// ---- Required: id / newParentId / expectedVersion ----
	rawID, err := a.RequireString("id")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"",
			map[string]any{"id": rawID})
	}

	rawNewParentID, err := a.RequireString("newParentId")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "newParentId is required", nil)
	}
	newParentID := strings.TrimSpace(rawNewParentID)
	if !LooksLikeHierarchicalID(newParentID) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"newParentId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"newParentId": rawNewParentID})
	}

	expectedVersion := a.GetInt("expectedVersion", 0)
	if expectedVersion < 1 {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"expectedVersion is required and must be >= 1", nil)
	}

	// ---- Visibility: load the issue once, ensure project visible ----
	// Same pre-check shape as skein_update / skein_dep_add: surface
	// issue_not_found for both "missing" and "not visible" so we don't
	// leak the existence of a foreign issue.
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_reparent",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if f := IssueActionGate(ctx, d.Pool, "skein_reparent", "id", id, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return IssueResult{}, f
	}

	// ---- Reparent ----
	actor := DependenciesAgentActor(ctx, d.Pool, userID)
	_, err = d.Deps.Reparent(ctx, id, newParentID, expectedVersion,
		dependencies.CallerAccess{UserID: userID, Role: role}, actor)
	if err != nil {
		return IssueResult{}, MapReparentError(ctx, err, userID, id, newParentID)
	}

	// ---- Re-fetch the updated IssueDetail to return ----
	// Reparent does not return an IssueDetail (it can't import issues),
	// so we re-fetch the same way every other write tool returns its
	// post-state. The version is now bumped and parentIdCurrent reflects
	// the new parent.
	final, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_reparent",
			"reason", "re-fetch after reparent failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError,
			"reparent committed but re-fetch failed; re-read with skein_show", nil)
	}

	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", "skein_reparent",
		"userId", userID.String(), "issueId", id, "newParentId", newParentID,
		"version", final.Version,
		"latencyMs", time.Since(start).Milliseconds())
	return IssueResult{IssueID: id, Issue: final}, nil
}

// Delegation runs the operation of skein_drop_delegated / skein_resume_delegated for the caller userID.
func Delegation(ctx context.Context, d Deps, userID uuid.UUID, a Args, drop bool) (IssueResult, *Failure) {
	start := time.Now()
	toolName := "skein_resume_delegated"
	if drop {
		toolName = "skein_drop_delegated"
	}
	rawID, err := a.RequireString("id")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"",
			map[string]any{"id": rawID})
	}

	// Visibility check — same pattern as skein_accept. A caller
	// who can't see the project gets issue_not_found rather than
	// a confusing forbidden, matching the unified visibility
	// pattern used by every issue-write tool.
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", toolName,
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, d.Pool, issue.ProjectID, userID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", toolName,
			"reason", "visibility check failed",
			"userId", userID.String(), "issueId", id, "error", vErr.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "visibility check failed", nil)
	}

	actor := IssuesAgentActor(ctx, d.Pool, userID)
	input := issues.DelegationActionInput{ID: id, Actor: actor}

	var dto *issues.IssueDetail
	var actionErr error
	if drop {
		dto, actionErr = d.Issues.DismissDelegated(ctx, input)
	} else {
		dto, actionErr = d.Issues.ResumeDelegated(ctx, input)
	}
	if actionErr != nil {
		if errors.Is(actionErr, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		// sk-34 review Go-W2: DismissDelegated rejects callers who
		// aren't in the delegation chain (creator or past assignee)
		// with code="not_in_chain". Surface as a validation_error
		// so MCP clients get the structured payload + code.
		if vErr, ok := issues.IsValidationError(actionErr); ok {
			return IssueResult{}, Fail(ErrCodeValidationError, vErr.Message,
				map[string]any{"code": vErr.Code, "id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", toolName,
			"reason", "store call failed",
			"userId", userID.String(), "issueId", id, "error", actionErr.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, toolName+" failed", nil)
	}

	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", toolName,
		"userId", userID.String(), "issueId", id,
		"version", dto.Version,
		"latencyMs", time.Since(start).Milliseconds())
	// These two tools were wired before any of the response work and
	// kept returning the full IssueDetail, prose included — an echo the
	// caller never asked for, since the arguments are just an id
	// (sk-248 audit). sk-233 cut the prose; sk-282 replaced the card.
	//
	// The DELEGATION acknowledgement rather than the common one: these
	// two tools move neither status, nor version, nor any lock stamp,
	// so the common row would answer drop and resume identically — and
	// identically to the state before the call. `dismissed` is the one
	// thing they actually change. See ackFromDelegatedIssue.
	return IssueResult{IssueID: id, Issue: dto}, nil
}

// AttachCommits runs the operation of skein_commits_attach for the caller userID.
func AttachCommits(ctx context.Context, d Deps, userID uuid.UUID, a Args) (IssueResult, *Failure) {
	start := time.Now()

	rawID, err := a.RequireString("issueId")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "issueId is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": rawID})
	}

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// on the issue's project (forceUnlockGate), not by the role.

	rawCommits, ok := a.GetArguments()["commits"]
	if !ok {
		return IssueResult{}, Fail(ErrCodeValidationError, "commits is required", nil)
	}
	commits, parseErr := ParseCommitInputs(rawCommits, "commits")
	if parseErr != nil {
		return IssueResult{}, parseErr
	}
	if len(commits) == 0 {
		return IssueResult{}, Fail(ErrCodeValidationError, "commits must be non-empty", nil)
	}

	// Visibility check before delegating to the store. Mirrors the
	// skein_close pattern: load issue → ensure caller can see the
	// owning project → otherwise render as issue_not_found (don't
	// leak project existence).
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_commits_attach",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if f := IssueActionGate(ctx, d.Pool, "skein_commits_attach", "id", id, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return IssueResult{}, f
	}
	if force {
		if f := ForceUnlockGate(ctx, d.Pool, "skein_commits_attach", "id", id, issue.ProjectID, userID); f != nil {
			return IssueResult{}, f
		}
	}

	actor := IssuesAgentActor(ctx, d.Pool, userID)

	dto, err := d.Issues.AttachCommits(ctx, issues.AttachCommitsInput{
		ID:        id,
		Commits:   commits,
		SessionID: actor.SessionID,
		Actor:     actor,
		Force:     force,
	})
	if err != nil {
		// Reuse the close-error classifier: validation_error
		// (invalid hash / too-many), repo_not_found, repo_ambiguous,
		// issue_not_found, locked_by_other, and the
		// serialization-failure → version_conflict fallback are
		// already handled there. pinned_no_force won't fire because
		// AttachCommits doesn't gate on the pin flag.
		return IssueResult{}, MapCloseError(ctx, d.Pool, issue.ProjectID, err, "skein_commits_attach", userID.String(), id, "")
	}

	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", "skein_commits_attach",
		"userId", userID.String(), "issueId", id,
		"commitCount", len(commits),
		"latencyMs", time.Since(start).Milliseconds())

	return IssueResult{IssueID: id, Issue: dto}, nil
}

// DetachCommits runs the operation of skein_commits_detach for the caller userID.
func DetachCommits(ctx context.Context, d Deps, userID uuid.UUID, a Args) (IssueResult, *Failure) {
	start := time.Now()

	rawID, err := a.RequireString("issueId")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "issueId is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": rawID})
	}

	force := a.GetBool("force", false)
	// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
	// on the issue's project (forceUnlockGate), not by the role.

	rawHashes, ok := a.GetArguments()["hashes"]
	if !ok || rawHashes == nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "hashes is required", nil)
	}
	hashes, parseErr := ParseDetachHashes(rawHashes)
	if parseErr != nil {
		return IssueResult{}, parseErr
	}

	// Visibility before the store, exactly as skein_commits_attach
	// does it: load the issue, check the owning project, and render
	// an invisible project as issue_not_found so the answer never
	// betrays that the id exists somewhere the caller cannot see.
	issue, err := d.Issues.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return IssueResult{}, Fail(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": id})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_commits_detach",
			"reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if f := IssueActionGate(ctx, d.Pool, "skein_commits_detach", "id", id, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return IssueResult{}, f
	}
	if force {
		if f := ForceUnlockGate(ctx, d.Pool, "skein_commits_detach", "id", id, issue.ProjectID, userID); f != nil {
			return IssueResult{}, f
		}
	}

	actor := IssuesAgentActor(ctx, d.Pool, userID)

	res, err := d.Issues.DetachCommits(ctx, issues.DetachCommitsInput{
		ID:        id,
		Hashes:    hashes,
		SessionID: actor.SessionID,
		Actor:     actor,
		Force:     force,
	})
	if err != nil {
		// The close-error classifier already covers every failure
		// this path can produce: validation_error (malformed or
		// over-cap hashes), issue_not_found, locked_by_other, and
		// the serialization-abort fallback. Repo resolution is not
		// among them — detach matches on hash alone.
		return IssueResult{}, MapCloseError(ctx, d.Pool, issue.ProjectID, err, "skein_commits_detach", userID.String(), id, "")
	}

	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", "skein_commits_detach",
		"userId", userID.String(), "issueId", id,
		"requestedCount", len(hashes),
		"detachedCount", len(res.Detached),
		"notFoundCount", len(res.NotFound),
		"latencyMs", time.Since(start).Milliseconds())

	return IssueResult{IssueID: id, Issue: res.Issue, Detach: res}, nil
}

// ReleaseLock releases the claim on an issue without bypassing its version
// or its pin (the public API's releaseIssueLock; the UI's «release the
// lock»): the caller's own claim — taken by any of his keys, executors or
// MCP sessions — without a role, another holder's with force_unlock. The
// version grows; an issue nobody holds is answered as it is.
func ReleaseLock(ctx context.Context, d Deps, userID uuid.UUID, a Args) (IssueResult, *Failure) {
	rawID, err := a.RequireString("id")
	if err != nil {
		return IssueResult{}, Fail(ErrCodeValidationError, "id is required", nil)
	}
	id := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(id) {
		return IssueResult{}, Fail(ErrCodeValidationError,
			"id must be a full hierarchical id like \"demo-42\"", map[string]any{"id": rawID})
	}
	issue, err := d.Issues.GetIssue(ctx, id)
	if errors.Is(err, issues.ErrNotFound) {
		return IssueResult{}, Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible", map[string]any{"id": id})
	}
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "operationId", "releaseIssueLock", "reason", "load issue failed",
			"userId", userID.String(), "issueId", id, "error", err.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if f := IssueActionGate(ctx, d.Pool, "release_lock", "id", id, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return IssueResult{}, f
	}
	_, ferr := projects.EnsureProjectAction(ctx, d.Pool, issue.ProjectID, userID, authz.ActionForceUnlock)
	switch {
	case ferr == nil, errors.Is(ferr, projects.ErrProjectActionForbidden):
	default:
		slog.ErrorContext(ctx, logMessage(ctx, "error"), "operationId", "releaseIssueLock", "reason", "force_unlock check failed",
			"userId", userID.String(), "issueId", id, "error", ferr.Error())
		return IssueResult{}, Fail(ErrCodeInternalError, "permission check failed", nil)
	}
	dto, err := d.Issues.ReleaseLock(ctx, issues.ReleaseLockInput{
		ID: id, CanForceUnlock: ferr == nil, Actor: IssuesAgentActor(ctx, d.Pool, userID),
	})
	switch {
	case err == nil:
		return IssueResult{IssueID: id, Issue: dto}, nil
	case errors.Is(err, issues.ErrForceUnlockForbidden):
		return IssueResult{}, Fail(ErrCodeForceUnlockForbidden, MsgForceUnlockForbidden, map[string]any{"id": id})
	case errors.Is(err, issues.ErrNotLockHolder):
		return IssueResult{}, Fail(ErrCodeNotLockHolder,
			"the claim changed while it was being released; read the issue again", map[string]any{"id": id})
	case errors.Is(err, issues.ErrNotFound):
		return IssueResult{}, Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible", map[string]any{"id": id})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"), "operationId", "releaseIssueLock", "reason", "store call failed",
		"userId", userID.String(), "issueId", id, "error", err.Error())
	return IssueResult{}, Fail(ErrCodeInternalError, "release failed", nil)
}

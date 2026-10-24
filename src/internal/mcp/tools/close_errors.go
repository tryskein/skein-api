package tools

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// maxReposInErrorDetails caps the repo list embedded in a repo_ambiguous /
// repo_not_found error body (sk-303.36). A project's git_repos[] is small
// in practice — the whole reason the ambiguous case exists at all is
// "more than one, but still a handful" — but the field is caller-visible
// JSON, so it is capped defensively rather than trusted to stay small
// forever.
const maxReposInErrorDetails = 20

// repoErrorSummary is the minimal shape of one project repo surfaced in a
// repo_ambiguous / repo_not_found error body — enough for the caller to
// retry immediately with a valid repoId or repoUrl. Nothing here is more
// than the caller could already see: EnsureProjectVisible has already
// run by the time this classifier is reached, so the caller can see the
// owning project and, with it, its declared repos.
type repoErrorSummary struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// repoResolutionDetails loads the project's git_repos[] and shapes it
// into the "field" + "repos" details a repo_ambiguous / repo_not_found
// error body carries, so the caller can retry with a concrete repoId or
// repoUrl instead of a separate lookup round-trip (sk-303.36).
//
// Best-effort: a load failure here must not mask the original
// classification (the caller still needs the repo_ambiguous /
// repo_not_found code), so on error this logs and returns just the
// "field" hint that existed before the repo list did.
func repoResolutionDetails(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, tool string) map[string]any {
	details := map[string]any{"field": "commits[].repoUrl|repoId"}

	repos, err := projects.LoadProjectRepos(ctx, pool, projectID)
	if err != nil {
		slog.WarnContext(ctx, "mcp tool error",
			"tool", tool,
			"reason", "loading project repos for error details failed",
			"projectId", projectID.String(),
			"error", err.Error())
		return details
	}

	details["reposTotal"] = len(repos)
	n := len(repos)
	truncated := false
	if n > maxReposInErrorDetails {
		n = maxReposInErrorDetails
		truncated = true
	}
	summaries := make([]repoErrorSummary, n)
	for i := 0; i < n; i++ {
		summaries[i] = repoErrorSummary{ID: repos[i].ID.String(), URL: repos[i].URL, Name: repos[i].Name}
	}
	details["repos"] = summaries
	if truncated {
		details["reposTruncated"] = true
	}
	return details
}

// closeErrorClassification is the per-row error envelope shared by
// skein_close (wrapped via skmcp.ToolError) and skein_close_many
// (returned as one entry in the results[] array). Code is the stable
// short string ("version_conflict" etc.); Message is the human-readable
// detail; Details is the structured payload (currentVersion, lockedBy,
// etc.) the agent may inspect.
type closeErrorClassification struct {
	Code    string
	Message string
	Details map[string]any
}

// classifyCloseError translates the typed errors emitted by
// issues.Store.Close into a structured envelope. Shared by skein_close
// (single) and skein_close_many (batch) so both surface the same error
// code set with the same structured fields.
//
// `tool` is the calling tool name used in the structured log line on
// the internal_error fall-through — keeps "this code path errored"
// search queries scoped to the tool that hit the bug. `projectID` is the
// issue's owning project, used only to load git_repos[] for the
// repo_ambiguous / repo_not_found detail list (sk-303.36) — pass
// uuid.Nil when the project isn't known yet (e.g. the failure happened
// before the issue could be loaded); that never matches a repo error, so
// the extra lookup it would otherwise trigger just never fires.
//
// `closeTarget` is the id the caller named for the duplicate / superseded
// link (sk-489), echoed back in the details of the two target refusals.
// Empty on every call that named no target — those errors cannot arise
// then, so the empty string is never observed.
func classifyCloseError(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, err error, tool, userID, issueID, closeTarget string) closeErrorClassification {
	// sk-118 (F2e): retire-block on a standing issue with non-closed children.
	// It embeds a *ValidationError (so it also flows through IsValidationError
	// on the REST boundary), but here we promote the code to the top-level
	// envelope `error` and attach the offending child ids so agents branch on
	// the specific cause and close/retire the children first. Checked BEFORE
	// the generic IsValidationError so it doesn't collapse into validation_error.
	var standingErr *issues.ErrStandingHasOpenChildren
	if errors.As(err, &standingErr) {
		return closeErrorClassification{
			Code:    ErrCodeStandingHasOpenChildren,
			Message: standingErr.Error(),
			Details: map[string]any{"id": issueID, "childIds": standingErr.ChildIDs},
		}
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		return closeErrorClassification{
			Code:    ErrCodeValidationError,
			Message: vErr.Message,
			Details: map[string]any{"code": vErr.Code},
		}
	}
	// sk-489 (US-2) — the close-target link. Both refusals come out of the
	// SAME transaction as the close itself, and neither is a
	// ValidationError, so without this branch they would fall all the way
	// through to the internal_error tail below: an agent that pointed
	// closeTarget at a typo'd id would be told the server broke. The
	// rendering is shared with the update surfaces — see
	// classifyCloseTargetError for why it is one function and not four.
	if cls := classifyCloseTargetError(err, closeTarget); cls != nil {
		return *cls
	}
	// Repo resolution errors from commit-on-close. Top-level codes so
	// agents can branch on body.error == "repo_not_found" / "repo_ambiguous"
	// directly (mirrors attachments_unavailable's shape rather than
	// the nested-under-validation_error pattern).
	if errors.Is(err, projects.ErrRepoNotFound) {
		return closeErrorClassification{
			Code:    ErrCodeRepoNotFound,
			Message: "commit references a repo not declared in the project's gitRepos[]",
			Details: repoResolutionDetails(ctx, pool, projectID, tool),
		}
	}
	if errors.Is(err, projects.ErrRepoAmbiguous) {
		details := repoResolutionDetails(ctx, pool, projectID, tool)
		// sk-303.36: "zero or multiple repos" conflates two incompatible
		// situations. With multiple repos, naming one is possible and the
		// repo list in `details` is the fix. With zero, naming one is
		// impossible — the old message demanded it anyway. Split the
		// message on the repo count we just loaded; the wire error code
		// stays repo_ambiguous either way (agents already branch on it,
		// and this is a message fix, not a new failure mode).
		message := "project has multiple repos; specify repoUrl or repoId"
		if total, ok := details["reposTotal"].(int); ok && total == 0 {
			message = "project has no repos declared in gitRepos[]; there is nothing to attach this commit to — " +
				"add one first via skein_project_update(gitReposAdd:[...])"
		}
		return closeErrorClassification{
			Code:    ErrCodeRepoAmbiguous,
			Message: message,
			Details: details,
		}
	}
	if errors.Is(err, issues.ErrNotFound) {
		return closeErrorClassification{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"id": issueID},
		}
	}
	if errors.Is(err, issues.ErrPinnedNoForce) {
		return closeErrorClassification{
			Code:    ErrCodePinnedNoForce,
			Message: "pinned issues require force=true to close",
			Details: map[string]any{"id": issueID},
		}
	}
	var lockErr *issues.ErrLockedByOther
	if errors.As(err, &lockErr) {
		return closeErrorClassification{
			Code:    ErrCodeLockedByOther,
			Message: "issue is locked by another session",
			Details: map[string]any{"id": issueID, "lockedBy": lockErr.LockedBy},
		}
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return closeErrorClassification{
			Code:    ErrCodeVersionConflict,
			Message: "the issue was modified by another writer",
			Details: map[string]any{"currentVersion": verr.CurrentVersion},
		}
	}
	// sk-607: the caller's context ended while the close was in flight. Not
	// a failure of the server, and filing it under internal_error "close
	// failed" made it indistinguishable from one. skein_close_many checks
	// its loop context first (classifyCloseRowError); here the caller's is
	// the only one.
	if code, message, ok := contextStop(ctx, ctx, err); ok {
		return closeErrorClassification{
			Code:    code,
			Message: message,
			Details: map[string]any{"id": issueID},
		}
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", tool,
		"reason", "store call failed",
		"userId", userID, "issueId", issueID, "error", err.Error())
	return closeErrorClassification{
		Code:    ErrCodeInternalError,
		Message: "close failed",
	}
}

// classifyCloseRowError is classifyCloseError for one row of
// skein_close_many, whose store calls run under the batch loop's context
// (batchLoopContext) rather than the caller's. A row cut by the loop's own
// deadline answers batch_deadline_exceeded — the code the caller already
// resends on — and not internal_error, which is what production returned
// for exactly that (sk-607). Everything else is classifyCloseError's.
func classifyCloseRowError(ctx, loopCtx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, err error, userID, issueID, closeTarget string) closeErrorClassification {
	if code, message, ok := contextStop(ctx, loopCtx, err); ok {
		return closeErrorClassification{
			Code:    code,
			Message: message,
			Details: map[string]any{"id": issueID},
		}
	}
	return classifyCloseError(ctx, pool, projectID, err, "skein_close_many", userID, issueID, closeTarget)
}

// classifyCloseTargetError renders the two close-target refusals, or nil
// when err is neither (sk-489, US-2).
//
// It has TWO callers, and the count is exact — the previous wording named a
// third that does not exist. They are close_errors.go#classifyCloseError
// (skein_close / skein_close_many) and update.go#mapUpdateError
// (skein_update). The reason for sharing is that the same two store errors
// reach those surfaces through the same two writers, and an agent that
// learned `issue_not_found` from one of them must not get
// `internal_error` from another. Copies would be chances to diverge; the
// divergence would be silent, and its symptom — a fall-through to
// internal_error — is precisely the defect this pair was written to stop.
//
// update_many.go#classifyUpdateError does NOT call it, and that is correct
// rather than an omission: skein_update_many refuses `closeTarget` in its
// input builder (update_run.go#buildUpdateOneInput), so neither store error
// can reach its table. The edge is named here because it is one decision
// away from becoming the defect above: the day that surface accepts a
// target, this call has to be added in the same change.
func classifyCloseTargetError(err error, closeTarget string) *closeErrorClassification {
	// The code is the one skein_dep_add answers for the same conditions
	// (dep_add.go#classifyDepStoreError). Since sk-674.18 T033 (INV-3) a
	// target in a project the caller cannot see — like one of another space
	// (INV-24), which the store already reports as not found — answers
	// exactly as a nonexistent one: the split code
	// dependency_target_not_visible was an existence oracle.
	switch {
	case errors.Is(err, dependencies.ErrTargetNotFound),
		errors.Is(err, dependencies.ErrDependencyTargetNotVisible):
		return &closeErrorClassification{
			Code:    ErrCodeIssueNotFound,
			Message: "closeTarget issue does not exist or is not visible",
			Details: closeTargetDetails(closeTarget),
		}
	}
	return nil
}

// mapCloseError is the single-tool wrapper that emits a CallToolResult
// from a classified close error. Used by skein_close and
// skein_commits_attach.
func mapCloseError(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, err error, tool, userID, issueID, closeTarget string) *mcpgo.CallToolResult {
	c := classifyCloseError(ctx, pool, projectID, err, tool, userID, issueID, closeTarget)
	return skmcp.ToolError(c.Code, c.Message, c.Details)
}

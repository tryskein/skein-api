package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterCommitsAttach wires the skein_commits_attach tool. Attaches
// commit refs to an existing issue without touching status,
// closed_reason, closed_at, version, or locked_by. The retroactive
// counterpart to skein_close's `commits[]`: when the agent closed
// the issue without commits[] (or wants to add a hash that landed
// after the close), this tool extends the link set without
// overwriting the original close metadata.
//
// Why it exists (sk-35): re-running skein_close with the same id
// silently overwrites closed_reason and closed_at because the
// "Re-close with the same hashes is idempotent" line in the
// skein_close description was misread as "the whole re-close is
// idempotent". The hash dedup is real (issue_commits unique
// constraint + ON CONFLICT DO NOTHING), but every re-close is still
// a full UPDATE on the issues row. This tool is the atomic add-only
// path with no such side effect.
//
// Predicate: same lock guard as skein_close (caller must hold the
// lock OR force=true with force_unlock OR issue is unlocked). Version is
// NOT checked — attach is metadata-add, and pure-add operations
// shouldn't force the agent into a version-conflict retry loop.
// Pinned issues are NOT gated — the pin flag protects against
// destructive lifecycle changes, not metadata additions.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false (attach is purely additive — never
//     removes existing rows)
//   - idempotentHint: true  (re-attach of the same hash is a silent
//     no-op at the row level via ON CONFLICT DO NOTHING; the event
//     is still written so the audit trail records the intent)
//   - openWorldHint: false
func RegisterCommitsAttach(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_commits_attach",
		mcpgo.WithDescription(withAckAnswerNote(
			"Attach git commit refs to an existing issue without touching "+
				"status, closed_reason, closed_at, version or locked_by. "+
				"Use this — NOT a second skein_close — when the original "+
				"close forgot `commits[]` or a closing commit landed "+
				"afterwards: re-closing silently overwrites the original "+
				"closed_reason, this tool is the additive path.\n\n"+
				"Issue may be in any status. Re-attaching the same hash is "+
				"a silent no-op; the issue.commits_attached event is still "+
				"emitted for the audit trail. Caller must hold the lock OR "+
				"pass force=true (force_unlock: project owner, space owner, instance admin) — unlocked issues are open to "+
				"any agent. No expectedVersion: the attach does not bump "+
				"version.",
		)),
		mcpgo.WithString("issueId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithArray("commits",
			mcpgo.Required(),
			mcpgo.Description(commitsArrayDescription+" Must be non-empty."),
			mcpgo.MinItems(1),
			mcpgo.MaxItems(maxCommitsPerCloseCall),
			mcpgo.Items(commitInputItemSchema()),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass lock predicate. Needs force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleCommitsAttach(pool, issuesStore))
}

func handleCommitsAttach(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		id := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": rawID}), nil
		}

		force := req.GetBool("force", false)
		// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
		// on the issue's project (forceUnlockGate), not by the role.

		rawCommits, ok := req.GetArguments()["commits"]
		if !ok {
			return skmcp.ToolError(ErrCodeValidationError, "commits is required", nil), nil
		}
		commits, parseErr := parseCommitInputs(rawCommits, "commits")
		if parseErr != nil {
			return skmcp.ToolError(parseErr.Code, parseErr.Message, parseErr.Details), nil
		}
		if len(commits) == 0 {
			return skmcp.ToolError(ErrCodeValidationError, "commits must be non-empty", nil), nil
		}

		// Visibility check before delegating to the store. Mirrors the
		// skein_close pattern: load issue → ensure caller can see the
		// owning project → otherwise render as issue_not_found (don't
		// leak project existence).
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_commits_attach",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		if res := issueActionGate(ctx, pool, "skein_commits_attach", "id", id, issue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}
		if force {
			if res := forceUnlockGate(ctx, pool, "skein_commits_attach", "id", id, issue.ProjectID, userID); res != nil {
				return res, nil
			}
		}

		actor := IssuesAgentActor(ctx, pool, userID)

		dto, err := issuesStore.AttachCommits(ctx, issues.AttachCommitsInput{
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
			return mapCloseError(ctx, pool, issue.ProjectID, err, "skein_commits_attach", userID.String(), id, ""), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_commits_attach",
			"userId", userID.String(), "issueId", id,
			"commitCount", len(commits),
			"latencyMs", time.Since(start).Milliseconds())

		return writeIssueAck(ctx, "skein_commits_attach", userID.String(), id,
			dto, req.GetBool("verbose", false))
	}
}

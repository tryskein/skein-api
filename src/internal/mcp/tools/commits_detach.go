package tools

import (
	"context"
	"errors"
	"fmt"
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

// RegisterCommitsDetach wires the skein_commits_detach tool — the
// removal counterpart of skein_commits_attach (sk-394).
//
// Why it exists. A commit hash is accepted on its FORM alone: lowercase
// hex, 7..64 chars. The server holds no clone of the repository and
// cannot ask git whether the object exists, so an abbreviation padded
// out by hand satisfies the contract and is written — this happened, on
// sk-285.7.2.88, and the fabricated ref sat in commitRefs beside three
// real ones. Until this tool existed there was no way back: no MCP tool,
// no REST route, only hand-edited SQL. Taken together, «accepted on
// faith» and «never removable» turn one mistyped argument into a
// permanent lie in the evidentiary part of the tracker. This tool fixes
// the second half, which is the half that is fixable here.
//
// It does NOT make the first half true: nothing verifies that a hash
// names a real object, on attach or on detach. What changes is that a
// wrong ref is now an error an agent can correct, rather than one it can
// only confess.
//
// Predicate: skein_commits_attach's, verbatim — caller holds the lock,
// OR the issue is unlocked, OR force=true with force_unlock. No
// expectedVersion: removing a commit link does not bump the issue's
// version any more than adding one does.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: TRUE — and this is the one annotation that
//     differs from attach. Rows really are deleted, and the deletion is
//     not recoverable from Skein: re-attaching restores the hash but not
//     linked_at, linked_by or the optional metadata the original row
//     carried.
//   - idempotentHint: true (a second detach of the same hash removes
//     nothing and answers notFound — the end state is the same)
//   - openWorldHint: false
func RegisterCommitsDetach(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_commits_detach",
		mcpgo.WithDescription(withAckAnswerNote(
			"Remove git commit refs from an issue without touching "+
				"status, closed_reason, closed_at, version or locked_by. "+
				"The counterpart of skein_commits_attach, and the only way "+
				"back from a hash that should not have been linked — a "+
				"hash is accepted on its FORM alone (lowercase hex, 7..64), "+
				"so a fabricated one passes validation and, before this "+
				"tool, stayed linked forever.\n\n"+
				"Matches on HASH, not on (repo, hash): every row of this "+
				"issue carrying the hash is removed, whichever repo it was "+
				"filed under, so a ref attached to the WRONG repo is "+
				"removable too. Issue may be in any status. Caller must "+
				"hold the lock OR pass force=true (force_unlock: project owner, space owner, instance admin) — unlocked "+
				"issues are open to any agent.\n\n"+
				"The response carries `detached` (the rows actually removed, "+
				"each with the repoId you never named) and `notFound` (the "+
				"hashes that had no row on this issue) at the TOP LEVEL in "+
				"both lean and verbose mode. Read them: a typo in the hash "+
				"produces the same success as a real removal, and only "+
				"`notFound` tells the two apart.",
		)),
		mcpgo.WithString("issueId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithArray("hashes",
			mcpgo.Required(),
			mcpgo.Description("Commit hashes to unlink from this issue. Max "+
				"10. Lowercase hex, 7..64 chars — the same shape skein_close "+
				"and skein_commits_attach accept, validated here too so a "+
				"malformed hash is refused instead of silently reported as "+
				"`notFound`. Must match the stored hash exactly: this is a "+
				"string match, not a git lookup, so an abbreviation does not "+
				"remove the full hash it prefixes."),
			mcpgo.MinItems(1),
			mcpgo.MaxItems(maxCommitsPerCloseCall),
			mcpgo.Items(map[string]any{
				"type":      "string",
				"minLength": 7,
				"maxLength": 64,
				"pattern":   "^[0-9a-f]{7,64}$",
			}),
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
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleCommitsDetach(pool, issuesStore))
}

// parseDetachHashes reads the `hashes` argument: a non-empty array of
// strings, capped at the same maximum the attach side uses.
//
// Shape errors are classified here rather than at the store boundary so
// the caller is told WHICH element was wrong. The hash CONTENT is left
// to the store's commitHashRegex — one validator, one message, whichever
// door the request came through.
func parseDetachHashes(raw any) ([]string, *batchErrorDTO) {
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "hashes must be an array",
		}
	}
	if len(rawSlice) == 0 {
		return nil, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "hashes must be non-empty",
		}
	}
	if len(rawSlice) > maxCommitsPerCloseCall {
		return nil, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: fmt.Sprintf("hashes may carry at most %d items (got %d)", maxCommitsPerCloseCall, len(rawSlice)),
			Details: map[string]any{"max": maxCommitsPerCloseCall, "got": len(rawSlice)},
		}
	}

	out := make([]string, 0, len(rawSlice))
	for i, entry := range rawSlice {
		s, ok := entry.(string)
		if !ok {
			return nil, &batchErrorDTO{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("hashes[%d] must be a string", i),
				Details: map[string]any{"field": fmt.Sprintf("hashes[%d]", i)},
			}
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, &batchErrorDTO{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("hashes[%d] is empty", i),
				Details: map[string]any{"field": fmt.Sprintf("hashes[%d]", i)},
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// detachReportKeys renders the per-hash verdict for the response's top
// level.
//
// Both keys are ALWAYS present, empty arrays included, because their
// absence and their emptiness mean opposite things here: «nothing was
// removed» is the outcome the caller most needs to notice, and a missing
// key reads as «this response does not carry that information» (the rule
// the acknowledgement contract states for issue fields). The one thing
// the caller must never have to guess is whether the bad ref is gone.
func detachReportKeys(res *issues.DetachCommitsResult) map[string]any {
	detached := make([]map[string]any, 0, len(res.Detached))
	for _, ref := range res.Detached {
		detached = append(detached, map[string]any{
			"hash":   ref.Hash,
			"repoId": ref.RepoID.String(),
		})
	}
	notFound := res.NotFound
	if notFound == nil {
		notFound = []string{}
	}
	return map[string]any{
		"detached": detached,
		"notFound": notFound,
	}
}

func handleCommitsDetach(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
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

		rawHashes, ok := req.GetArguments()["hashes"]
		if !ok || rawHashes == nil {
			return skmcp.ToolError(ErrCodeValidationError, "hashes is required", nil), nil
		}
		hashes, parseErr := parseDetachHashes(rawHashes)
		if parseErr != nil {
			return skmcp.ToolError(parseErr.Code, parseErr.Message, parseErr.Details), nil
		}

		// Visibility before the store, exactly as skein_commits_attach
		// does it: load the issue, check the owning project, and render
		// an invisible project as issue_not_found so the answer never
		// betrays that the id exists somewhere the caller cannot see.
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_commits_detach",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		if res := issueActionGate(ctx, pool, "skein_commits_detach", "id", id, issue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}
		if force {
			if res := forceUnlockGate(ctx, pool, "skein_commits_detach", "id", id, issue.ProjectID, userID); res != nil {
				return res, nil
			}
		}

		actor := IssuesAgentActor(ctx, pool, userID)

		res, err := issuesStore.DetachCommits(ctx, issues.DetachCommitsInput{
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
			return mapCloseError(ctx, pool, issue.ProjectID, err, "skein_commits_detach", userID.String(), id, ""), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_commits_detach",
			"userId", userID.String(), "issueId", id,
			"requestedCount", len(hashes),
			"detachedCount", len(res.Detached),
			"notFoundCount", len(res.NotFound),
			"latencyMs", time.Since(start).Milliseconds())

		return writeIssueAckWith(ctx, "skein_commits_detach", userID.String(), id,
			res.Issue, req.GetBool("verbose", false), detachReportKeys(res))
	}
}

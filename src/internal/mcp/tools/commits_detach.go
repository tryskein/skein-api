package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
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
			mcpgo.Description("Bypass lock predicate. Needs the key right `manage` (else `insufficient_scope`) and force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
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

// parseDetachHashes — see issueops.ParseDetachHashes; the refusal as a
// batch row error.
func parseDetachHashes(raw any) ([]string, *batchErrorDTO) {
	h, f := issueops.ParseDetachHashes(raw)
	return h, batchErrorFrom(f)
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
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		out, f := issueops.DetachCommits(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		id, res := out.IssueID, out.Detach
		return writeIssueAckWith(ctx, "skein_commits_detach", userID.String(), id,
			res.Issue, req.GetBool("verbose", false), detachReportKeys(res))
	}
}

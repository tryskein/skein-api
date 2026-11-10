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

// RegisterUnclaim wires the skein_unclaim tool. Releases the caller's
// lock on an issue AND (sk-19) atomically transitions the status to the
// caller-declared `nextStatus`.
//
// Required parameters:
//
//   - id          — full hierarchical issue id
//   - nextStatus — one of: "in_progress" (handoff), "open" (give up),
//     "deferred" (punt). "closed" is rejected; closing has its own
//     tool (skein_close) which carries reason + commits[].
//   - reason — non-empty after trim, lands on the issue.unclaimed
//     event AND (when status actually changed) the
//     issue.status_changed event.
//
// Predicate:
//
//   - locked_by IS NULL → idempotent no-op (returns the existing
//     IssueDetail unchanged; nextStatus is ignored on a free issue,
//     because "unclaim" semantically means "release my lock" — there's
//     no lock to release, nothing to do. Use skein_update to flip status
//     without a lock).
//   - locked_by = caller's session → success.
//   - locked_by = another session AND force=false → `not_lock_holder`.
//   - locked_by = another session AND force=true (force_unlock) → success
//     with event reason="<reason> (force-overridden)".
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false (first call mutates state; we keep the
//     conservative default for consistency with other write tools)
//   - openWorldHint: false
func RegisterUnclaim(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_unclaim",
		mcpgo.WithDescription(withAckAnswerNote(
			"Release the caller's lock on an issue AND atomically "+
				"transition status to the caller-declared `nextStatus`. "+
				"Three valid intents: `nextStatus=\"in_progress\"` "+
				"= handoff (status stays in_progress so a teammate can "+
				"claim and continue; pair with a comment describing "+
				"what's done); `nextStatus=\"open\"` = give up (status "+
				"reverts to open so the issue is visible again in "+
				"skein_ready); `nextStatus=\"deferred\"` = punt (status "+
				"→ deferred; set defer_until via a follow-up "+
				"skein_update if needed). `nextStatus=\"closed\"` is "+
				"rejected — use skein_close instead, which carries "+
				"reason + commits[]. `reason` is required (non-empty "+
				"after trim) and lands on both the issue.unclaimed "+
				"event and (when status actually changed) the "+
				"issue.status_changed event. Idempotent on an "+
				"already-unlocked issue (returns the issue post-state "+
				"unchanged; nextStatus is ignored — use skein_update "+
				"directly if you want to flip status without a lock). "+
				"Caller must hold the lock or pass `force=true` "+
				"(force_unlock: project owner, space owner, instance admin; "+
				"others get `force_unlock_forbidden`); event reason "+
				"becomes \"<reason> (force-overridden)\" on the force "+
				"path.",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithString("nextStatus",
			mcpgo.Required(),
			mcpgo.Description("Required intent declaration: \"in_progress\" (handoff), \"open\" (give up), or \"deferred\" (punt). \"closed\" is rejected — use skein_close."),
			mcpgo.Enum("in_progress", "open", "deferred"),
		),
		mcpgo.WithString("reason",
			mcpgo.Required(),
			mcpgo.Description("Free-form reason annotation (non-empty after trim). Lands on issue.unclaimed.reason and, when status changed, issue.status_changed.reason."),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass the lock-holder predicate. Needs the key right `manage` (else `insufficient_scope`) and force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleUnclaim(pool, issuesStore))
}

func handleUnclaim(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Unclaim(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		id, dto := res.IssueID, res.Issue
		return writeIssueAck(ctx, "skein_unclaim", userID.String(), id,
			dto, req.GetBool("verbose", false))
	}
}

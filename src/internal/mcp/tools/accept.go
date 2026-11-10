package tools

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterAccept wires the skein_accept tool. Phase 2 of the ui-redesign
// acceptance flow — gives the issue's creator an explicit ack from the
// assignee (vs the auto-accept-on-self-assign and reset-on-reassign
// mechanics shipped in Phase 1).
//
// Predicate (in order — first failure wins):
//
//   - issue not found / not visible → `issue_not_found`
//   - issue status = closed → `cannot_accept_closed` (sk-25; reopen
//     via skein_update(status=open) first)
//   - caller is not the assignee → `forbidden` (owner rights and
//     role=admin do not let anyone accept for another)
//   - expectedVersion supplied AND mismatch → `version_conflict`
//
// Idempotent on already-accepted issues: re-calling the tool on an
// issue with accepted_at IS NOT NULL returns the current IssueDetail
// without writing — no duplicate event row, no version bump.
//
// Annotations:
//
//   - readOnlyHint: false (mutates state on first call)
//   - destructiveHint: false (acceptance can be reversed by a re-
//     assignment, which clears accepted_at)
//   - idempotentHint: true (the spec contract is silent no-op on
//     re-accept — the agent can safely retry without side-effects)
//   - openWorldHint: false
func RegisterAccept(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_accept",
		mcpgo.WithDescription(withAckAnswerNote(
			"Accept an issue assigned to you — explicit acknowledgement that "+
				"you've seen the issue, understood its scope and are taking it. "+
				"Sets accepted_at = now() and accepted_by = caller. The issue's "+
				"creator and watchers receive a notification.\n\n"+
				"When an abort survives retries while expectedVersion still matches, serialization_retry means no acceptance was written; retry unchanged.\n\n"+
				"Use this when the issue lands in your inbox (assigned to you, "+
				"accepted_at IS NULL). Without acceptance, the creator has no "+
				"guarantee you've actually seen the issue — Skein's inbox model "+
				"exists precisely to provide that signal.\n\n"+
				"Only the current assignee accepts — owner rights and the admin role do not. Idempotent: "+
				"calling on an already-accepted issue is a silent no-op (no "+
				"duplicate event, no version bump).\n\n"+
				"Optional expectedVersion enforces optimistic concurrency same "+
				"as skein_update / skein_close.",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Hierarchical issue id, e.g. demo-42 or sk-14. A space invitation id from the inbox is refused with invitation_agent_forbidden: a person accepts invitations in the web UI."),
		),
		mcpgo.WithNumber("expectedVersion",
			mcpgo.Description("Optional optimistic version check. "+
				"Mismatch returns version_conflict."),
			mcpgo.Min(1),
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

	addTool(s, tool, handleAccept(pool, issuesStore))
}

func handleAccept(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Accept(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		id, dto := res.IssueID, res.Issue
		return writeIssueAcceptanceAck(ctx, "skein_accept", userID.String(), id,
			dto, req.GetBool("verbose", false))
	}
}

func mapAcceptError(ctx context.Context, err error, userID uuid.UUID, id string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapAcceptError(ctx, err, userID, id))
}

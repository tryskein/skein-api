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

// RegisterDropDelegated wires the skein_drop_delegated tool. sk-34 —
// "I delegated this task further and don't want to track it anymore"
// affordance. Adds the caller to issues.dismissed_by + runs the explicit
// unwatch (couples bell-silence, sk-622.21, US-1): a subscription, if the
// caller has one, is deleted with an opt-out; without one, nothing is
// recorded — only the issue's Watching card is cleared (R3). Audit-only event
// (issue.delegation_dropped) — no FanOut, since broadcasting would
// undo the bell-silence we just installed.
//
// Visibility check runs identically to skein_accept; a caller who
// can't see the project gets issue_not_found.
//
// Annotations:
//
//   - readOnlyHint: false (mutates state on first call)
//   - destructiveHint: false (reversible via skein_resume_delegated)
//   - idempotentHint: true (silent no-op when already dismissed)
//   - openWorldHint: false
func RegisterDropDelegated(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_drop_delegated",
		mcpgo.WithDescription(withAckAnswerNote(
			"Drop this issue from your Delegated tab: you stop seeing it there "+
				"AND stop receiving bell notifications. Use it when you have handed "+
				"off the work and don't need to track it anymore.\n\n"+
				"Effect: appends your user id to the issue's dismissed_by array (sticky — "+
				"persists across re-assignments unless you become the current assignee "+
				"again, in which case it's cleared automatically). Also unsubscribes you from "+
				"the issue exactly like skein_watchers(action=\"unwatch\"): IF you are subscribed "+
				"to it, the subscription is deleted and an opt-out stops the automatic paths "+
				"(assignment, reopen) from re-subscribing you; if you are NOT subscribed (for "+
				"example, auto-unsubscribed when the issue closed), no opt-out is recorded, so a "+
				"later reopen or assignment can subscribe you again. Either way the issue's card "+
				"leaves your Watching column.\n\n"+
				"Idempotent: a second call on an issue you've already dropped is a silent "+
				"no-op — no duplicate event, no version bump. The acknowledgement carries "+
				"`dismissed` — YOUR membership in dismissed_by after the call (true here) — "+
				"because neither status nor version moves, so it is the only proof the write "+
				"applied.\n\n"+
				"To resume, call skein_resume_delegated. To resume bell notifications "+
				"only, call skein_watchers(action=\"watch\") (the two intents are "+
				"decoupled — undismissing the delegation does NOT re-arm notifications).",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Hierarchical issue id, e.g. demo-42 or sk-14."),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	addTool(s, tool, handleDelegationAction(pool, issuesStore, true))
}

// RegisterResumeDelegated wires the skein_resume_delegated tool. sk-34
// — symmetric inverse of skein_drop_delegated. Removes the caller from
// issues.dismissed_by so the issue surfaces in Delegated again.
//
// Does NOT re-add a watcher row — bell notifications stay silenced
// until the caller explicitly calls skein_watchers(action="watch"). The two intents
// (re-surface in Delegated tab / re-arm bell) are decoupled by design.
//
// Idempotent: caller not in dismissed_by → silent no-op.
//
// Annotations: matches skein_drop_delegated except destructiveHint —
// resume is the recovery direction, never destructive.
func RegisterResumeDelegated(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_resume_delegated",
		mcpgo.WithDescription(withAckAnswerNote(
			"Undo skein_drop_delegated — bring this issue back to your Delegated tab. "+
				"Removes your user id from the issue's dismissed_by array so the "+
				"Delegated predicate stops filtering it out.\n\n"+
				"Does NOT re-arm bell notifications: the unsubscribe done by "+
				"skein_drop_delegated (and its opt-out, when one was recorded) stays in effect until you call "+
				"skein_watchers(action=\"watch\") separately. Two distinct intents — "+
				"Delegated lens visibility vs bell-"+
				"notification flow — and resume only restores the first.\n\n"+
				"Idempotent: calling on an issue you haven't dropped is a silent no-op. "+
				"The acknowledgement carries `dismissed` — YOUR membership in dismissed_by "+
				"after the call (false here) — because neither status nor version moves, so "+
				"it is the only proof the write applied.",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Hierarchical issue id, e.g. demo-42 or sk-14."),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	addTool(s, tool, handleDelegationAction(pool, issuesStore, false))
}

// handleDelegationAction is the shared handler for both drop / resume —
// they differ only in which store method they call. drop=true routes
// to DismissDelegated; drop=false routes to ResumeDelegated.
func handleDelegationAction(pool *pgxpool.Pool, issuesStore *issues.Store, drop bool) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	toolName := "skein_resume_delegated"
	if drop {
		toolName = "skein_drop_delegated"
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Delegation(ctx, d, userID, issueops.Args(req.GetArguments()), drop)
		if f != nil {
			return toolFailure(f), nil
		}
		id, dto := res.IssueID, res.Issue
		return writeIssueDelegationAck(ctx, toolName,
			userID, id, dto, req.GetBool("verbose", false))
	}
}

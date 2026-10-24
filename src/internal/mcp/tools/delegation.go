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

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
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
	toolName := "skein_resume_delegated"
	if drop {
		toolName = "skein_drop_delegated"
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		id := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"demo-42\"",
				map[string]any{"id": rawID}), nil
		}

		// Visibility check — same pattern as skein_accept. A caller
		// who can't see the project gets issue_not_found rather than
		// a confusing forbidden, matching the unified visibility
		// pattern used by every issue-write tool.
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", toolName,
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, issue.ProjectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", toolName,
				"reason", "visibility check failed",
				"userId", userID.String(), "issueId", id, "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil), nil
		}

		actor := IssuesAgentActor(ctx, pool, userID)
		input := issues.DelegationActionInput{ID: id, Actor: actor}

		var dto *issues.IssueDetail
		var actionErr error
		if drop {
			dto, actionErr = issuesStore.DismissDelegated(ctx, input)
		} else {
			dto, actionErr = issuesStore.ResumeDelegated(ctx, input)
		}
		if actionErr != nil {
			if errors.Is(actionErr, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			// sk-34 review Go-W2: DismissDelegated rejects callers who
			// aren't in the delegation chain (creator or past assignee)
			// with code="not_in_chain". Surface as a validation_error
			// so MCP clients get the structured payload + code.
			if vErr, ok := issues.IsValidationError(actionErr); ok {
				return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
					map[string]any{"code": vErr.Code, "id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", toolName,
				"reason", "store call failed",
				"userId", userID.String(), "issueId", id, "error", actionErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, toolName+" failed", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
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
		return writeIssueDelegationAck(ctx, toolName,
			userID, id, dto, req.GetBool("verbose", false))
	}
}

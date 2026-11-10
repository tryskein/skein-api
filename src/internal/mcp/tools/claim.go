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

// RegisterClaim wires the skein_claim tool. Atomically takes the lock on
// an issue AND transitions its status (sk-18) for the caller's MCP
// session.
//
// Predicate (success when ANY one):
//
//   - issue's assignee IS NULL (free), OR
//   - issue's assignee resolves to the CALLER AND locked_by IS NULL
//     (R2.2-created issue with assignee=me, never explicitly claimed).
//     sk-294: the comparison is on user identity, so all three documented
//     handle forms — username, e-mail, user UUID — work; before that only
//     a literal username match did, and a UUID-assigned task was refused
//     to its own assignee with a misleading `already_claimed`.
//
// Status transition (sk-18, all in the same SERIALIZABLE tx as the lock):
//
//   - open      → in_progress  (started_at stamped if first entry)
//   - deferred  → in_progress  (un-defer)
//   - in_progress → in_progress (no-op for same-session re-claim)
//   - closed    → rejected with `cannot_claim_closed`
//   - standing  → rejected with `cannot_claim_standing` (sk-118 F2d) —
//     spawn a child subtask and claim that instead
//
// Re-claim by the SAME session on an in_progress issue is truly
// idempotent — the tool acknowledges the existing post-state unchanged, no
// version bump, no event written. Different session of the same user,
// or any other locked-by-someone-else case, returns `already_claimed`.
//
// sk-202: `already_claimed` is only ever returned after reading a row
// that shows another holder — it is never used for transient database
// conflicts. A claim that loses a SERIALIZABLE race is retried in the
// store and, if every attempt loses, surfaces as `serialization_retry`
// (nothing was written; the call is safe to repeat verbatim). Every
// rejection is logged at WARN with the predicate that fired and the row
// state it read.
//
// Events:
//   - issue.claimed       — always on a real claim
//   - issue.status_changed (reason="claim") — when status actually
//     transitioned to in_progress
//
// Admin-only: passing `assignee` that names a DIFFERENT USER than the
// caller is the "claim on behalf of <user>" path; non-admin callers passing
// assignee != self get `forbidden`. "Self" is decided by identity, not by
// string: a non-admin may name themselves by username, e-mail or their own
// user UUID (sk-294).
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false (re-claim is technically a no-op, but the
//     first claim mutates state — we keep this false for consistency
//     with other write tools)
//   - openWorldHint: false
func RegisterClaim(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_claim",
		mcpgo.WithDescription(withAckAnswerNote(
			"Take locks for the caller's MCP session using exactly one of `id` "+
				"or `ids`. A batch contains 1..100 unique issue ids and is atomic: "+
				"every issue is claimed or the whole transaction rolls back; "+
				"the successful response preserves caller order. "+
				"Claim transitions status to `in_progress` atomically. "+
				"open/deferred → in_progress in the same transaction; "+
				"started_at is stamped on first entry. Re-claiming the same "+
				"in_progress issue from the same session is idempotent (no "+
				"version bump, no event). Locked by another session → "+
				"`already_claimed`. Closed issues cannot be claimed — they "+
				"return `cannot_claim_closed`; reopen via skein_update first. "+
				"Standing tasks return `cannot_claim_standing` — spawn a "+
				"child (skein_create parentId=<this>) and claim that. "+
				"`already_claimed` always reflects a real other holder; a "+
				"claim that loses a database serialization race returns "+
				"`serialization_retry` instead — nothing was written, so "+
				"retry the call verbatim. "+
				"A successful claim emits `issue.claimed` plus "+
				"`issue.status_changed` (reason=\"claim\") when status "+
				"transitioned, and acknowledges with the post-state: id, "+
				"`status=in_progress`, `lockedBy`, the claim stamp and the "+
				"version (see `verbose` for the full issue). A BATCH "+
				"acknowledges with `{results:[…one row per input id, in "+
				"caller order…], summary:{requested, succeeded, failed}}` "+
				"— the same form skein_close_many uses. That envelope is "+
				"NOT an invitation to partial success: a batch claim is "+
				"still all-or-nothing, so `failed` is 0 on every response "+
				"you will ever see and a failing batch is a tool error "+
				"with nothing written. Admins may "+
				"pass `assignee` to claim on behalf of another user; "+
				"non-admins setting `assignee` to anyone other than "+
				"themselves get `forbidden`.",
		)),
		mcpgo.WithString("id",
			mcpgo.Description("One full hierarchical issue id. Exactly one of id or ids is required."),
		),
		mcpgo.WithArray("ids",
			mcpgo.Description("Atomic claim batch: 1..100 unique full hierarchical issue ids. Exactly one of id or ids is required; response order matches this array."),
			mcpgo.MinItems(1),
			mcpgo.MaxItems(batchMaxItems),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithString("assignee",
			mcpgo.Description("Optional assignee handle. Defaults to caller's username (null or blank counts as omitted). Admin-only when set to a different user — non-admins get `forbidden`. A user UUID that does not name a person who may work in the project is refused with assignee_not_allowed. A non-string value (number, array, object, boolean) is refused with validation_error, code assignee_not_string, and nothing is claimed."),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription+" On a batch it applies to every "+
				"row; the `{results, summary}` envelope is the same in both modes."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleClaim(pool, issuesStore))
}

func handleClaim(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Claim(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		if res.Batch {
			return writeIssueBatchAck(ctx, "skein_claim", userID.String(),
				res.Issues, req.GetBool("verbose", false))
		}
		return writeIssueAck(ctx, "skein_claim", userID.String(), res.IDs[0],
			res.Issue, req.GetBool("verbose", false))
	}
}

func parseClaimIDs(args map[string]any) ([]string, bool, *mcpgo.CallToolResult) {
	v0, v1, f := issueops.ParseClaimIDs(args)
	return v0, v1, toolFailure(f)
}

func logClaimRejected(ctx context.Context, outcome, userID, sessionID, issueID string, extra ...any) {
	issueops.LogClaimRejected(ctx, outcome, userID, sessionID, issueID, extra...)
}

func mapClaimError(ctx context.Context, err error, userID, sessionID, issueID string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapClaimError(ctx, err, userID, sessionID, issueID))
}

func assigneeHandleIsCaller(
	ctx context.Context,
	pool *pgxpool.Pool,
	handle, callerUsername string,
	callerID uuid.UUID,
) (bool, error) {
	return issueops.AssigneeHandleIsCaller(ctx, pool, handle, callerUsername, callerID)
}

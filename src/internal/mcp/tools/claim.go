package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/notifications"
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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		ids, batch, parseErr := parseClaimIDs(req.GetArguments())
		if parseErr != nil {
			return parseErr, nil
		}

		// Mcp-Session-Id is required for claim — without it we can't bind
		// the lock to a session. Surface a stable error code so agents
		// can detect "MCP transport not initialised" cases distinctly.
		sessionID := auth.SessionIDFrom(ctx)
		if sessionID == "" {
			return skmcp.ToolError(ErrCodeLockRequiredSessionID,
				"Mcp-Session-Id header is required for claim", nil), nil
		}

		// Visibility is checked for every member before opening the atomic
		// claim transaction. Invisible and missing rows share one envelope.
		for _, id := range ids {
			issue, err := issuesStore.GetIssue(ctx, id)
			if err != nil {
				if errors.Is(err, issues.ErrNotFound) {
					return skmcp.ToolError(ErrCodeIssueNotFound,
						"issue does not exist or is not visible",
						map[string]any{"id": id}), nil
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_claim",
					"reason", "load issue failed",
					"userId", userID.String(), "issueId", id, "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
			}
			if res := issueActionGate(ctx, pool, "skein_claim", "id", id, issue.ProjectID, userID, authz.ActionAssign); res != nil {
				return res, nil
			}
		}

		// Resolve actor first so we know caller's username for the
		// admin-on-behalf check below.
		actor := IssuesAgentActor(ctx, pool, userID)

		// assignee param. Empty/missing → default to caller's username
		// (the store fills it in). When explicitly set, non-admin users
		// can only name THEMSELVES; admins may name any user.
		//
		// sk-294: "themselves" is resolved to a users.id rather than
		// string-matched against the username. The schema advertises three
		// handle forms and a non-admin naming their own UUID was getting
		// `forbidden` for claiming their own task. A handle that resolves
		// to nobody (a free-form label) is not the caller, so it keeps
		// failing the gate exactly as before.
		//
		// sk-621: parsed by assigneeFromArgs — absent, null or blank is
		// «not passed», a non-string value is refused before any write
		// (it used to read as «not passed» and claim onto the caller).
		assigneeArg, aErr := assigneeFromArgs(req.GetArguments(), "", nil)
		if aErr != nil {
			return aErr, nil
		}
		if assigneeArg != nil && !auth.IsAdminRole(role, auth.IsExternalFrom(ctx)) {
			a := *assigneeArg
			self, rerr := assigneeHandleIsCaller(ctx, pool, a, actor.Username, userID)
			if rerr != nil {
				slog.WarnContext(ctx, "skein_claim assignee resolution failed",
					"userId", userID.String(), "error", rerr.Error())
				return skmcp.ToolError(ErrCodeInternalError,
					"assignee resolution failed", nil), nil
			}
			if !self {
				return skmcp.ToolError(ErrCodeForbidden,
					"only admins may claim on behalf of other users",
					map[string]any{"assignee": a}), nil
			}
		}

		if batch {
			dtos, err := issuesStore.ClaimMany(ctx, issues.ClaimManyInput{
				IDs: ids, SessionID: sessionID, OptionalAssignee: assigneeArg, Actor: actor,
			})
			if err != nil {
				failedID := strings.Join(ids, ",")
				var batchErr *issues.ClaimManyError
				if errors.As(err, &batchErr) {
					failedID = batchErr.ID
				}
				return mapClaimError(ctx, err, userID.String(), sessionID, failedID), nil
			}
			slog.InfoContext(ctx, "mcp tool ok", "tool", "skein_claim", "userId", userID.String(), "issueCount", len(ids), "latencyMs", time.Since(start).Milliseconds())
			// sk-282 T023 / D-6, FP-9: this branch used to build its own
			// response — a bare JSON array of issue cards — and was one of
			// the four sites that never reached the common write-response
			// point. It now answers in the reference group form, one
			// acknowledgement row per input id plus the summary, in caller
			// order.
			//
			// ATOMICITY IS UNTOUCHED, and that is the load-bearing part of
			// this change rather than a caveat. ClaimMany above is still
			// one transaction whose first failure rolls the whole batch
			// back and returns a tool ERROR; nothing below it can produce
			// a per-row summary of zeros. Reading the new envelope as
			// «partial success is now normal for groups» would turn
			// «claimed none» into «claimed nine of ten» and put nine locks
			// in front of other agents that do not exist today — a
			// write-semantics change, not a response-size one (FP-10).
			return writeIssueBatchAck(ctx, "skein_claim", userID.String(),
				dtos, req.GetBool("verbose", false))
		}

		id := ids[0]
		dto, err := issuesStore.Claim(ctx, issues.ClaimInput{
			ID:               id,
			SessionID:        sessionID,
			OptionalAssignee: assigneeArg,
			Actor:            actor,
		})
		if err != nil {
			return mapClaimError(ctx, err, userID.String(), sessionID, id), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_claim",
			"userId", userID.String(), "issueId", id,
			"version", dto.Version,
			"latencyMs", time.Since(start).Milliseconds())
		return writeIssueAck(ctx, "skein_claim", userID.String(), id,
			dto, req.GetBool("verbose", false))
	}
}

func parseClaimIDs(args map[string]any) ([]string, bool, *mcpgo.CallToolResult) {
	rawID, hasID := args["id"]
	rawIDs, hasIDs := args["ids"]
	if hasID == hasIDs {
		return nil, false, skmcp.ToolError(ErrCodeValidationError, "exactly one of id or ids is required", nil)
	}
	values := make([]any, 0, 1)
	batch := hasIDs
	if hasIDs {
		var ok bool
		values, ok = rawIDs.([]any)
		if !ok || len(values) == 0 || len(values) > batchMaxItems {
			return nil, true, skmcp.ToolError(ErrCodeValidationError, "ids must contain 1..100 issue ids", map[string]any{"max": batchMaxItems})
		}
	} else {
		values = append(values, rawID)
	}
	ids := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		raw, ok := value.(string)
		id := strings.TrimSpace(raw)
		if !ok || !looksLikeHierarchicalID(id) {
			return nil, batch, skmcp.ToolError(ErrCodeValidationError, "claim ids must be full hierarchical ids like \"demo-42\"", map[string]any{"index": i, "id": value})
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, batch, skmcp.ToolError(ErrCodeValidationError, "ids must be unique", map[string]any{"id": id})
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, batch, nil
}

// logClaimRejected emits the WARN line for a business rejection of
// skein_claim.
//
// sk-202: every non-success exit from this tool must leave a trace. The
// pre-sk-202 code logged only success and internal_error, so a claim that
// answered `already_claimed` produced no log line at all — when a false
// already_claimed showed up in production there was literally nothing to
// grep, and no way to tell which predicate had rejected or what row state
// it had seen. `outcome` carries the tool code, and callers pass the
// branch-specific state as extra key/value pairs.
func logClaimRejected(ctx context.Context, outcome, userID, sessionID, issueID string, extra ...any) {
	args := make([]any, 0, 10+len(extra))
	args = append(args,
		"tool", "skein_claim",
		"outcome", outcome,
		"userId", userID,
		"sessionId", sessionID,
		"issueId", issueID,
	)
	args = append(args, extra...)
	slog.WarnContext(ctx, "mcp tool rejected", args...)
}

// mapClaimError translates the typed errors emitted by issues.Store.Claim
// into stable MCP error codes, logging each rejection at WARN (sk-202).
//
// sessionID is the caller's Mcp-Session-Id — the value the lock predicate
// compared against — so a log line is self-contained: it says who asked,
// for what, which predicate rejected, and what the transaction read.
func mapClaimError(ctx context.Context, err error, userID, sessionID, issueID string) *mcpgo.CallToolResult {
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		logClaimRejected(ctx, ErrCodeAssigneeNotAllowed, userID, sessionID, issueID)
		return assigneeNotAllowedResult(map[string]any{"id": issueID})
	}
	// sk-118 (F2d): standing-claim rejection is a *ValidationError but carries
	// an actionable, branch-on-able code. Promote it to the top-level envelope
	// `error` BEFORE the generic IsValidationError branch below (which would
	// otherwise flatten it to validation_error), mirroring how mapUpdateError
	// promotes the F2c standing transition codes.
	if errors.Is(err, issues.ErrCannotClaimStanding) {
		msg := "cannot claim a standing task"
		if vErr, ok := issues.IsValidationError(err); ok && vErr != nil {
			msg = vErr.Message
		}
		logClaimRejected(ctx, ErrCodeCannotClaimStanding, userID, sessionID, issueID)
		return skmcp.ToolError(ErrCodeCannotClaimStanding, msg,
			map[string]any{"id": issueID})
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		logClaimRejected(ctx, ErrCodeValidationError, userID, sessionID, issueID,
			"validationCode", vErr.Code)
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrNotFound) {
		logClaimRejected(ctx, ErrCodeIssueNotFound, userID, sessionID, issueID)
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": issueID})
	}
	// sk-202: a claim lost every retry against SQLSTATE 40001. Nothing was
	// written — tell the agent to retry rather than that somebody else owns
	// the issue.
	if errors.Is(err, issues.ErrSerializationRetry) {
		logClaimRejected(ctx, ErrCodeSerializationRetry, userID, sessionID, issueID)
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"claim lost a serialization race and made no change — retry the call",
			map[string]any{"id": issueID})
	}
	if errors.Is(err, issues.ErrAlreadyClaimed) {
		// The typed detail carries the predicate that rejected plus the row
		// state it read, which is exactly what a false already_claimed needs
		// in order to be diagnosable from logs alone.
		var acErr *issues.AlreadyClaimedError
		if errors.As(err, &acErr) {
			logClaimRejected(ctx, ErrCodeAlreadyClaimed, userID, sessionID, issueID,
				"branch", acErr.Branch,
				"readStatus", acErr.Status,
				"readLockedBy", acErr.LockedBy,
				"readAssignee", acErr.Assignee)
		} else {
			// No detail attached — a bare ErrAlreadyClaimed means some path
			// returned the sentinel without reading a row to justify it.
			// That is the sk-202 failure shape; make it loud.
			logClaimRejected(ctx, ErrCodeAlreadyClaimed, userID, sessionID, issueID,
				"branch", "unknown",
				"warning", "already_claimed without row-state detail")
		}
		fields := map[string]any{"id": issueID}
		if acErr != nil {
			if acErr.LockedBy != "" {
				fields["lockedBy"] = acErr.LockedBy
			}
			if acErr.Assignee != "" {
				fields["assignee"] = acErr.Assignee
			}
		}
		return skmcp.ToolError(ErrCodeAlreadyClaimed,
			"issue is already claimed by another session",
			fields)
	}
	if errors.Is(err, issues.ErrCannotClaimClosed) {
		logClaimRejected(ctx, ErrCodeCannotClaimClosed, userID, sessionID, issueID)
		return skmcp.ToolError(ErrCodeCannotClaimClosed,
			"cannot claim a closed issue — reopen it first",
			map[string]any{"id": issueID})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_claim",
		"reason", "store call failed",
		"userId", userID, "sessionId", sessionID,
		"issueId", issueID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "claim failed", nil)
}

// assigneeHandleIsCaller reports whether an assignee handle names the
// calling user. Mirrors issues.assigneeHandleIsActor — same resolver, same
// fallback — but runs on the pool at the tool layer, before the store's
// transaction opens, because the admin gate has to answer before we hand
// the handle down.
//
// Kept as a tiny local rather than exported from the issues package: the
// store's copy takes an issues.Actor and runs inside the claim tx, and
// giving one function two call shapes is how a permission predicate
// acquires a second, subtly different meaning.
func assigneeHandleIsCaller(
	ctx context.Context,
	pool *pgxpool.Pool,
	handle, callerUsername string,
	callerID uuid.UUID,
) (bool, error) {
	// Identity, not delivery (sk-674.18 T028): «is this handle me».
	uid, err := notifications.ResolveAssigneeIdentity(ctx, pool, handle)
	if err != nil {
		return false, err
	}
	if uid != uuid.Nil {
		return uid == callerID, nil
	}
	return strings.EqualFold(handle, callerUsername), nil
}

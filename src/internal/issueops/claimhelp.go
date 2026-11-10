package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func ParseClaimIDs(args map[string]any) ([]string, bool, *Failure) {
	rawID, hasID := args["id"]
	rawIDs, hasIDs := args["ids"]
	if hasID == hasIDs {
		return nil, false, Fail(ErrCodeValidationError, "exactly one of id or ids is required", nil)
	}
	values := make([]any, 0, 1)
	batch := hasIDs
	if hasIDs {
		var ok bool
		values, ok = rawIDs.([]any)
		if !ok || len(values) == 0 || len(values) > BatchMaxItems {
			return nil, true, Fail(ErrCodeValidationError, "ids must contain 1..100 issue ids", map[string]any{"max": BatchMaxItems})
		}
	} else {
		values = append(values, rawID)
	}
	ids := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		raw, ok := value.(string)
		id := strings.TrimSpace(raw)
		if !ok || !LooksLikeHierarchicalID(id) {
			return nil, batch, Fail(ErrCodeValidationError, "claim ids must be full hierarchical ids like \"demo-42\"", map[string]any{"index": i, "id": value})
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, batch, Fail(ErrCodeValidationError, "ids must be unique", map[string]any{"id": id})
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, batch, nil
}

// LogClaimRejected emits the WARN line for a business rejection of
// skein_claim.
//
// sk-202: every non-success exit from this tool must leave a trace. The
// pre-sk-202 code logged only success and internal_error, so a claim that
// answered `already_claimed` produced no log line at all — when a false
// already_claimed showed up in production there was literally nothing to
// grep, and no way to tell which predicate had rejected or what row state
// it had seen. `outcome` carries the tool code, and callers pass the
// branch-specific state as extra key/value pairs.
func LogClaimRejected(ctx context.Context, outcome, userID, sessionID, issueID string, extra ...any) {
	args := make([]any, 0, 10+len(extra))
	args = append(args,
		"op", "claim",
		"outcome", outcome,
		"userId", userID,
		"sessionId", sessionID,
		"issueId", issueID,
	)
	args = append(args, extra...)
	slog.WarnContext(ctx, "mcp tool rejected", args...)
}

// MapClaimError translates the typed errors emitted by issues.Store.Claim
// into stable MCP error codes, logging each rejection at WARN (sk-202).
//
// sessionID is the caller's Mcp-Session-Id — the value the lock predicate
// compared against — so a log line is self-contained: it says who asked,
// for what, which predicate rejected, and what the transaction read.
func MapClaimError(ctx context.Context, err error, userID, sessionID, issueID string) *Failure {
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		LogClaimRejected(ctx, ErrCodeAssigneeNotAllowed, userID, sessionID, issueID)
		return AssigneeNotAllowed(map[string]any{"id": issueID})
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
		LogClaimRejected(ctx, ErrCodeCannotClaimStanding, userID, sessionID, issueID)
		return Fail(ErrCodeCannotClaimStanding, msg,
			map[string]any{"id": issueID})
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		LogClaimRejected(ctx, ErrCodeValidationError, userID, sessionID, issueID,
			"validationCode", vErr.Code)
		return Fail(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrNotFound) {
		LogClaimRejected(ctx, ErrCodeIssueNotFound, userID, sessionID, issueID)
		return Fail(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": issueID})
	}
	// sk-202: a claim lost every retry against SQLSTATE 40001. Nothing was
	// written — tell the agent to retry rather than that somebody else owns
	// the issue.
	if errors.Is(err, issues.ErrSerializationRetry) {
		LogClaimRejected(ctx, ErrCodeSerializationRetry, userID, sessionID, issueID)
		return Fail(ErrCodeSerializationRetry,
			"claim lost a serialization race and made no change — retry the call",
			map[string]any{"id": issueID})
	}
	if errors.Is(err, issues.ErrAlreadyClaimed) {
		// The typed detail carries the predicate that rejected plus the row
		// state it read, which is exactly what a false already_claimed needs
		// in order to be diagnosable from logs alone.
		var acErr *issues.AlreadyClaimedError
		if errors.As(err, &acErr) {
			LogClaimRejected(ctx, ErrCodeAlreadyClaimed, userID, sessionID, issueID,
				"branch", acErr.Branch,
				"readStatus", acErr.Status,
				"readLockedBy", acErr.LockedBy,
				"readAssignee", acErr.Assignee)
		} else {
			// No detail attached — a bare ErrAlreadyClaimed means some path
			// returned the sentinel without reading a row to justify it.
			// That is the sk-202 failure shape; make it loud.
			LogClaimRejected(ctx, ErrCodeAlreadyClaimed, userID, sessionID, issueID,
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
		return Fail(ErrCodeAlreadyClaimed,
			"issue is already claimed by another session",
			fields)
	}
	if errors.Is(err, issues.ErrCannotClaimClosed) {
		LogClaimRejected(ctx, ErrCodeCannotClaimClosed, userID, sessionID, issueID)
		return Fail(ErrCodeCannotClaimClosed,
			"cannot claim a closed issue — reopen it first",
			map[string]any{"id": issueID})
	}
	slog.ErrorContext(ctx, "issue operation error",
		"op", "claim",
		"reason", "store call failed",
		"userId", userID, "sessionId", sessionID,
		"issueId", issueID, "error", err.Error())
	return Fail(ErrCodeInternalError, "claim failed", nil)
}

// AssigneeHandleIsCaller reports whether an assignee handle names the
// calling user. Mirrors issues.assigneeHandleIsActor — same resolver, same
// fallback — but runs on the pool at the tool layer, before the store's
// transaction opens, because the admin gate has to answer before we hand
// the handle down.
//
// Kept as a tiny local rather than exported from the issues package: the
// store's copy takes an issues.Actor and runs inside the claim tx, and
// giving one function two call shapes is how a permission predicate
// acquires a second, subtly different meaning.
func AssigneeHandleIsCaller(
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

// BatchMaxItems caps the issues of one batch call (claim ids, update and close batches).
const BatchMaxItems = 100

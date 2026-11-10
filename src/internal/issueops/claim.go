package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"strings"
	"time"

	"github.com/google/uuid"
)

// claim.go — the claim of one issue or of a batch, all or nothing (MCP
// skein_claim, the public API's claimIssue and claimIssues). The holder is
// the session of the request (auth.SessionIDFrom): an MCP session, or the
// REST executor api:<keyId>[:<workerId>].

// ClaimResult is the outcome of Claim.
type ClaimResult struct {
	// IDs are the issues claimed, in the order asked.
	IDs []string
	// Batch — the call named ids, not id.
	Batch bool
	// Issue is the issue claimed by a single call; Issues those of a batch.
	Issue  *issues.IssueDetail
	Issues []*issues.IssueDetail
}

// Claim claims for the caller userID from the arguments of skein_claim.
func Claim(ctx context.Context, d Deps, userID uuid.UUID, a Args) (ClaimResult, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	ids, batch, parseErr := ParseClaimIDs(a.GetArguments())
	if parseErr != nil {
		return ClaimResult{}, parseErr
	}

	// Mcp-Session-Id is required for claim — without it we can't bind
	// the lock to a session. Surface a stable error code so agents
	// can detect "MCP transport not initialised" cases distinctly.
	sessionID := auth.SessionIDFrom(ctx)
	if sessionID == "" {
		return ClaimResult{}, Fail(ErrCodeLockRequiredSessionID,
			"Mcp-Session-Id header is required for claim", nil)
	}

	// Visibility is checked for every member before opening the atomic
	// claim transaction. Invisible and missing rows share one envelope.
	for _, id := range ids {
		issue, err := d.Issues.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return ClaimResult{}, Fail(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id})
			}
			slog.ErrorContext(ctx, "issue operation error",
				"op", "claim",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return ClaimResult{}, Fail(ErrCodeInternalError, "failed to load issue", nil)
		}
		if f := IssueActionGate(ctx, d.Pool, "skein_claim", "id", id, issue.ProjectID, userID, authz.ActionAssign); f != nil {
			return ClaimResult{}, f
		}
	}

	// Resolve actor first so we know caller's username for the
	// admin-on-behalf check below.
	actor := IssuesAgentActor(ctx, d.Pool, userID)

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
	assigneeArg, aErr := AssigneeFromArgs(a.GetArguments(), "", nil)
	if aErr != nil {
		return ClaimResult{}, aErr
	}
	if assigneeArg != nil && !auth.IsAdminRole(role, auth.IsExternalFrom(ctx)) {
		a := *assigneeArg
		self, rerr := AssigneeHandleIsCaller(ctx, d.Pool, a, actor.Username, userID)
		if rerr != nil {
			slog.WarnContext(ctx, "skein_claim assignee resolution failed",
				"userId", userID.String(), "error", rerr.Error())
			return ClaimResult{}, Fail(ErrCodeInternalError,
				"assignee resolution failed", nil)
		}
		if !self {
			return ClaimResult{}, Fail(ErrCodeForbidden,
				"only admins may claim on behalf of other users",
				map[string]any{"assignee": a})
		}
	}

	if batch {
		dtos, err := d.Issues.ClaimMany(ctx, issues.ClaimManyInput{
			IDs: ids, SessionID: sessionID, OptionalAssignee: assigneeArg, Actor: actor,
		})
		if err != nil {
			failedID := strings.Join(ids, ",")
			var batchErr *issues.ClaimManyError
			if errors.As(err, &batchErr) {
				failedID = batchErr.ID
			}
			return ClaimResult{}, MapClaimError(ctx, err, userID.String(), sessionID, failedID)
		}
		slog.InfoContext(ctx, "issue operation ok", "op", "claim", "userId", userID.String(), "issueCount", len(ids), "latencyMs", time.Since(start).Milliseconds())
		return ClaimResult{IDs: ids, Batch: true, Issues: dtos}, nil
	}

	id := ids[0]
	dto, err := d.Issues.Claim(ctx, issues.ClaimInput{
		ID:               id,
		SessionID:        sessionID,
		OptionalAssignee: assigneeArg,
		Actor:            actor,
	})
	if err != nil {
		return ClaimResult{}, MapClaimError(ctx, err, userID.String(), sessionID, id)
	}
	slog.InfoContext(ctx, "issue operation ok",
		"op", "claim",
		"userId", userID.String(), "issueId", id,
		"version", dto.Version,
		"latencyMs", time.Since(start).Milliseconds())
	return ClaimResult{IDs: ids, Issue: dto}, nil
}

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

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
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
		// INV-17 (sk-674.18 T029): the Inbox shows the caller's pending
		// space invitations too; accepting or declining one is a person's
		// act in the web UI, so an invitation id is refused with its own
		// code, not answered as a missing issue.
		if invID, perr := uuid.Parse(id); perr == nil {
			isInv, ierr := spaces.IsInvitationTo(ctx, pool, invID, userID)
			if ierr != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_accept", "reason", "invitation lookup failed",
					"userId", userID.String(), "error", ierr.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to read the invitation", nil), nil
			}
			if isInv {
				return skmcp.ToolError(ErrCodeInvitationAgentForbidden,
					"an agent cannot accept or decline a space invitation; the person does it in the web UI",
					map[string]any{"id": id}), nil
			}
		}
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"demo-42\"",
				map[string]any{"id": rawID}), nil
		}

		// expectedVersion is optional: GetInt returns 0 when the param
		// was omitted (matches MCP-go's `default` zero behaviour). We
		// translate 0/absent → nil ExpectedVersion so the store skips
		// the optimistic-concurrency check entirely. Any positive
		// value is honoured.
		var expectedVersionPtr *int
		if v := req.GetInt("expectedVersion", 0); v > 0 {
			expectedVersionPtr = &v
		}

		// Visibility check — must come before the store call so a
		// caller who can't see the project gets `issue_not_found`
		// rather than `forbidden` (matches the unified visibility
		// pattern used by skein_close / skein_unclaim).
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_accept",
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
				"tool", "skein_accept",
				"reason", "visibility check failed",
				"userId", userID.String(), "issueId", id, "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil), nil
		}

		// Actor.Role stays 'agent' (audit-trail actor type); only the
		// assignee accepts — there is no admin override (US-6).
		actor := IssuesAgentActor(ctx, pool, userID)

		dto, err := issuesStore.Accept(ctx, issues.AcceptInput{
			ID:              id,
			ExpectedVersion: expectedVersionPtr,
			Actor:           actor,
		})
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			if errors.Is(err, issues.ErrCannotAcceptClosed) {
				return skmcp.ToolError(ErrCodeCannotAcceptClosed,
					"cannot accept a closed issue — reopen it first",
					map[string]any{"id": id}), nil
			}
			if errors.Is(err, issues.ErrForbidden) {
				return skmcp.ToolError(ErrCodeForbidden,
					"only the issue's assignee can accept",
					map[string]any{"id": id}), nil
			}
			var verr *issues.ErrVersionConflict
			if errors.As(err, &verr) {
				return skmcp.ToolError(ErrCodeVersionConflict,
					"the issue was modified by another writer",
					map[string]any{"currentVersion": verr.CurrentVersion}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_accept",
				"reason", "store call failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "accept failed", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_accept",
			"userId", userID.String(), "issueId", id,
			"version", dto.Version,
			"latencyMs", time.Since(start).Milliseconds())
		// The acceptance stamp is the ONLY proof this write applied —
		// accept changes no status and a repeat is a version-less no-op
		// — so this surface, and only this one, carries it. See
		// ackFromAcceptedIssue.
		return writeIssueAcceptanceAck(ctx, "skein_accept", userID.String(), id,
			dto, req.GetBool("verbose", false))
	}
}

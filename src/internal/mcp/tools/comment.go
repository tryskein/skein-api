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

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterComment wires the skein_comment tool. Adds a comment to an
// existing issue with author_type='agent'. Body is required and trimmed
// to non-empty before insertion.
//
// sk-16 — `kind` is REQUIRED for agent callers (mirrors skein_create's
// kind parameter): "directed" = the human user told the agent to post,
// "autonomous" = the agent posted on its own initiative (handoff note,
// progress, side-quest). The wire token maps to
// comments.author_kind = agent_directed | agent_autonomous and powers
// the Humans / Agents filter chips on the comment thread. Cookie-session
// callers reach this through a future BFF passthrough; they will force
// author_kind=human and ignore `kind`.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false
//   - openWorldHint: false
func RegisterComment(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_comment",
		mcpgo.WithDescription(
			"Add a comment to an existing issue. The comment is recorded "+
				"with `authorType=\"agent\"` so the timeline distinguishes "+
				"agent-authored notes from human ones. `body` must be "+
				"non-empty after trim and at most 100k characters.\n\n"+
				"The `kind` parameter (REQUIRED for agent callers) "+
				"determines the resulting comment's `authorKind`: "+
				"`kind=\"directed\"` → `authorKind=\"agent_directed\"` (the "+
				"human user told the agent to post this comment, e.g. "+
				"\"reply to demo-42 with my answer below\"); "+
				"`kind=\"autonomous\"` → `authorKind=\"agent_autonomous\"` "+
				"(the agent posted on its own initiative — handoff note, "+
				"progress update, side-quest finding). The Humans/Agents "+
				"filter chips on the comment thread fold human + "+
				"agent_directed into \"Humans\" and surface agent_autonomous "+
				"separately as \"Agents\". `authorType` (transport) stays "+
				"agent regardless of kind.\n\n"+
				"The response is the created comment MINUS `body`, plus "+
				"`bodyOmitted:true` — every other field of the comment is "+
				"returned untouched, so this is an omission of one key, not "+
				"an allowlist. You just supplied the text, and an echoed copy "+
				"would sit in your context for the rest of the session and be "+
				"re-sent on every later turn. What that leaves you includes "+
				"`id` (for a follow-up skein_comment_update / "+
				"skein_comment_delete) and `version` (the expectedVersion "+
				"token). To read a body back — yours or anyone's — use "+
				"skein_comment_get (by id) or skein_show (the thread). A "+
				"write that lost a database serialization race (nothing "+
				"written) returns `serialization_retry` — retry the call "+
				"verbatim.",
		),
		mcpgo.WithString("issueId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithString("body",
			mcpgo.Required(),
			mcpgo.Description("Comment body (markdown). Non-empty after trim."),
		),
		// `kind` is REQUIRED for agent callers — enforced in the handler
		// rather than at the schema level so a future cookie-auth BFF
		// passthrough can omit it without a hard schema break.
		mcpgo.WithString("kind",
			mcpgo.Description("Comment origin classification (REQUIRED for agent "+
				"callers): \"directed\" = the human user told the agent to post "+
				"this comment; \"autonomous\" = the agent posted on its own "+
				"initiative (handoff note, progress, side-quest finding). "+
				"Cookie-session callers ignore this parameter and force "+
				"authorKind=human; MCP agent callers must specify it."),
			mcpgo.Enum("directed", "autonomous"),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleComment(pool, issuesStore))
}

// commentResponse is the wire shape — just the new comment row.
//
// The field is `any` rather than *issues.CommentDTO because the write
// path returns the lean projection (leanComment: the DTO minus `body`,
// plus `bodyOmitted:true`) instead of the struct itself (sk-248).
type commentResponse struct {
	Comment any `json:"comment"`
}

func handleComment(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rawIssueID, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		issueID := strings.TrimSpace(rawIssueID)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": rawIssueID}), nil
		}

		rawBody, err := req.RequireString("body")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "body is required", nil), nil
		}
		body := strings.TrimSpace(rawBody)
		if body == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"body must be non-empty after trim", nil), nil
		}
		if vErr := issues.ValidateProse("body", body); vErr != nil {
			return validationToolError(vErr), nil
		}

		// sk-16 — derive author_kind from caller mode + kind param.
		// Mirrors skein_create's callerOriginMode pattern.
		kindArg := strings.TrimSpace(req.GetString("kind", ""))
		callerMode, modeOK := callerOriginMode(ctx)
		if !modeOK {
			slog.ErrorContext(ctx, "callerOriginMode could not classify caller — neither api-key nor cookie signal present",
				"tool", "skein_comment", "userId", userID)
			return skmcp.ToolError(ErrCodeInternalError,
				"ambiguous caller mode; cannot determine comment kind", nil), nil
		}
		var authorKind string
		switch callerMode {
		case "human":
			authorKind = issues.CommentAuthorKindHuman
			// kindArg is silently ignored on the cookie path — same
			// semantics as the REST handler.
		case "agent":
			switch kindArg {
			case "directed":
				authorKind = issues.CommentAuthorKindAgentDirected
			case "autonomous":
				authorKind = issues.CommentAuthorKindAgentAutonomous
			case "":
				return skmcp.ToolError(ErrCodeValidationError,
					`kind is required for agent callers; pass "directed" (user-requested) or "autonomous" (side-quest)`,
					nil), nil
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"kind must be one of: directed, autonomous",
					map[string]any{"kind": kindArg}), nil
			}
		default:
			slog.ErrorContext(ctx, "callerOriginMode returned unexpected mode",
				"tool", "skein_comment", "mode", callerMode, "userId", userID)
			return skmcp.ToolError(ErrCodeInternalError,
				"unknown caller mode; cannot determine comment kind",
				map[string]any{"mode": callerMode}), nil
		}

		// Visibility: load issue + ensure project visible. Same anti-leak
		// rule as the REST handler — non-members see issue_not_found.
		issue, err := issuesStore.GetIssue(ctx, issueID)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"issueId": issueID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_comment",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		if res := issueActionGate(ctx, pool, "skein_comment", "issueId", issueID, issue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		actor := IssuesAgentActor(ctx, pool, userID)
		// authorName lands in comments.author (text). Use the actor's
		// resolved username — agentFallbackLabel ensures non-empty.
		authorName := actor.Username

		// authorType is fixed to 'agent' on the MCP path (transport axis);
		// authorKind reflects the resolved intent from above.
		commentType := issues.CommentAuthorTypeAgent
		if callerMode == "human" {
			commentType = issues.CommentAuthorTypeHuman
		}
		dto, err := issuesStore.AddComment(ctx, issueID, body,
			userID, authorName, commentType, authorKind, actor)
		if err != nil {
			return mapAddCommentError(ctx, err, userID.String(), issueID), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_comment",
			"userId", userID.String(), "issueId", issueID,
			"commentId", dto.ID.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return writeCommentResult(ctx, "skein_comment",
			userID.String(), dto.ID.String(), dto)
	}
}

// mapAddCommentError translates store-layer errors from AddComment into
// the MCP closed-set error codes. Extracted out of handleComment (rather
// than left inline) so the mapping can be pinned by a unit test the same
// way mapCommentWriteError / mapEpicCreateError / mapClaimError already
// are, without spinning up a database.
//
// sk-303.17 coderev: AddComment lost EVERY commentMentionMaxAttempts
// retry against a genuine SERIALIZABLE abort used to fall straight
// through every branch below into the ErrCodeInternalError default — the
// exact sk-305 defect shape this diff's retry was meant to close, just
// pushed to the tail of the budget. issues.Store now degrades that
// exhaustion to issues.ErrSerializationRetry (AddComment is an INSERT —
// nothing was ever written, so there is no version to blame it on); map
// it to the SAME `serialization_retry` code Claim already uses for the
// identical situation.
func mapAddCommentError(ctx context.Context, err error, userID, issueID string) *mcpgo.CallToolResult {
	if errors.Is(err, issues.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"the comment write lost a serialization race and made no change — retry the call",
			map[string]any{"issueId": issueID})
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"issueId": issueID})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_comment",
		"reason", "add comment failed",
		"userId", userID, "issueId", issueID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to add comment", nil)
}

// resolveCommentForWrite is the shared preamble for the edit/delete tools.
// It parses the commentId, loads the comment's meta (issue + project +
// author), and enforces project visibility — returning the resolved meta
// plus the caller's project AccessLevel. On any failure it returns a
// ready-to-send error result (the bool is true) so the handler can
// short-circuit. The authorization decision (author-only vs author-or-
// owner) is left to the caller since it differs per tool.
func resolveCommentForWrite(
	ctx context.Context,
	pool *pgxpool.Pool,
	issuesStore *issues.Store,
	tool string,
	userID uuid.UUID,
	role string,
	rawCommentID string,
) (*issues.CommentMeta, projects.AccessLevel, *mcpgo.CallToolResult) {
	commentID, err := uuid.Parse(strings.TrimSpace(rawCommentID))
	if err != nil {
		return nil, "", skmcp.ToolError(ErrCodeValidationError,
			"commentId must be a valid uuid",
			map[string]any{"commentId": rawCommentID})
	}
	meta, err := issuesStore.GetCommentMeta(ctx, commentID)
	if err != nil {
		if errors.Is(err, issues.ErrCommentNotFound) {
			return nil, "", skmcp.ToolError(ErrCodeIssueNotFound,
				"comment does not exist or is not visible",
				map[string]any{"commentId": commentID.String()})
		}
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", tool, "reason", "load comment meta failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", err.Error())
		return nil, "", skmcp.ToolError(ErrCodeInternalError, "failed to load comment", nil)
	}
	// sk-674.18 T066: editing or deleting a comment is ActionWrite of US-6.
	level, vErr := projects.EnsureProjectAction(ctx, pool, meta.ProjectID, userID, authz.ActionWrite)
	if vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			// Anti-leak: invisible project ⇒ comment "not found".
			return nil, "", skmcp.ToolError(ErrCodeIssueNotFound,
				"comment does not exist or is not visible",
				map[string]any{"commentId": commentID.String()})
		}
		if errors.Is(vErr, projects.ErrProjectActionForbidden) {
			return nil, "", skmcp.ToolError(ErrCodeForbidden, msgActionForbidden,
				map[string]any{"commentId": commentID.String()})
		}
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", tool, "reason", "visibility check failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", vErr.Error())
		return nil, "", skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil)
	}
	return meta, level, nil
}

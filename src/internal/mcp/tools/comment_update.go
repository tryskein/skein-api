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
)

// RegisterCommentUpdate wires the skein_comment_update tool (sk-57).
// Edits the body of an existing comment. AUTHOR-ONLY — the caller's
// resolved user id must equal the comment's author_user_id.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false
//   - openWorldHint: false
func RegisterCommentUpdate(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_comment_update",
		mcpgo.WithDescription(
			"Edit the body of a comment you authored. AUTHOR-ONLY — only "+
				"the user who posted the comment may edit it (a different "+
				"user, even a project owner, gets `forbidden`). "+
				"`expectedVersion` is the optimistic-concurrency token: pass "+
				"the `version` you last read on the comment; if it was edited "+
				"by another writer in between you get `version_conflict` with "+
				"the live `currentVersion` so you can re-read and retry. "+
				"A write that instead lost a database serialization race "+
				"(nothing written, no version moved) returns "+
				"`serialization_retry` — retry the call verbatim, do not "+
				"re-read first. "+
				"Editing a deleted comment fails with a validation error. "+
				"Authorship is never rewritten — the edit is recorded only in "+
				"the comment.edited event actor. @mentions newly introduced "+
				"by the edit fire the usual mention notification + auto-watch; "+
				"mentions removed by the edit are left alone.\n\n"+
				"`kind` mirrors skein_comment (REQUIRED for agent callers): "+
				"\"directed\" = the human told you to make this edit; "+
				"\"autonomous\" = your own initiative. It does not change the "+
				"comment's authorKind — it only classifies the mention "+
				"fan-out for newly-introduced @mentions.\n\n"+
				"The response is the edited comment MINUS `body`, plus "+
				"`bodyOmitted:true` — one key omitted, everything else "+
				"returned untouched (same contract as skein_comment). You "+
				"just supplied the text, and an echoed copy would stay in "+
				"your context for the rest of the session. The post-edit "+
				"`version` comes back, so a further edit chains straight off "+
				"it. Read the body back with skein_comment_get or "+
				"skein_show.",
		),
		mcpgo.WithString("commentId",
			mcpgo.Required(),
			mcpgo.Description("UUID of the comment to edit."),
		),
		mcpgo.WithString("body",
			mcpgo.Required(),
			mcpgo.Description("Replacement comment body (markdown). Non-empty after trim, <=100k chars."),
		),
		mcpgo.WithNumber("expectedVersion",
			mcpgo.Required(),
			mcpgo.Description("The comment version you expect (optimistic concurrency). "+
				"Mismatch returns version_conflict with the live currentVersion."),
		),
		mcpgo.WithString("kind",
			mcpgo.Description("Edit origin classification (REQUIRED for agent "+
				"callers): \"directed\" = the human user told you to make this "+
				"edit; \"autonomous\" = your own initiative. Cookie-session "+
				"callers ignore this. Does not change the comment's authorKind."),
			mcpgo.Enum("directed", "autonomous"),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	addTool(s, tool, handleCommentUpdate(pool, issuesStore))
}

func handleCommentUpdate(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		rawCommentID, err := req.RequireString("commentId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "commentId is required", nil), nil
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
		expectedVersion := req.GetInt("expectedVersion", 0)
		if expectedVersion < 1 {
			return skmcp.ToolError(ErrCodeValidationError,
				"expectedVersion is required and must be >= 1", nil), nil
		}

		// `kind` validation mirrors skein_comment. It does not change the
		// stored authorKind on an edit, but agent callers must still
		// declare intent (keeps the contract uniform); the resolved value
		// is otherwise unused on this path.
		kindArg := strings.TrimSpace(req.GetString("kind", ""))
		callerMode, modeOK := callerOriginMode(ctx)
		if !modeOK {
			slog.ErrorContext(ctx, "callerOriginMode could not classify caller",
				"tool", "skein_comment_update", "userId", userID)
			return skmcp.ToolError(ErrCodeInternalError,
				"ambiguous caller mode; cannot determine edit kind", nil), nil
		}
		if callerMode == "agent" {
			switch kindArg {
			case "directed", "autonomous":
				// ok
			case "":
				return skmcp.ToolError(ErrCodeValidationError,
					`kind is required for agent callers; pass "directed" or "autonomous"`, nil), nil
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"kind must be one of: directed, autonomous",
					map[string]any{"kind": kindArg}), nil
			}
		}

		meta, _, errRes := resolveCommentForWrite(ctx, pool, issuesStore,
			"skein_comment_update", userID, role, rawCommentID)
		if errRes != nil {
			return errRes, nil
		}
		// AUTHOR-ONLY.
		if meta.AuthorUserID == nil || *meta.AuthorUserID != userID {
			return skmcp.ToolError(ErrCodeForbidden,
				"only the comment author can edit this comment", nil), nil
		}

		actor := IssuesAgentActor(ctx, pool, userID)
		dto, err := issuesStore.UpdateComment(ctx, meta.CommentID, body, expectedVersion, actor)
		if err != nil {
			return mapCommentWriteError(ctx, err, "skein_comment_update", userID.String(), meta.CommentID.String()), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_comment_update",
			"userId", userID.String(), "commentId", meta.CommentID.String(),
			"version", dto.Version,
			"latencyMs", time.Since(start).Milliseconds())
		return writeCommentResult(ctx, "skein_comment_update",
			userID.String(), meta.CommentID.String(), dto)
	}
}

// mapCommentWriteError translates store-layer errors from UpdateComment /
// DeleteComment into the MCP closed-set error codes. Mirrors the REST
// handler's mapping: version_conflict (with currentVersion), the
// comment_deleted business error (422 → validation_error code on the MCP
// wire), comment-not-found, and validation errors.
//
// sk-303.17 coderev: UpdateComment/DeleteComment lost EVERY
// commentRowMaxAttempts retry against a genuine SERIALIZABLE abort used to
// fall straight through every branch below into the ErrCodeInternalError
// default — the same defect shape this diff's retry was meant to close,
// just pushed to the tail of the budget. issues.Store now degrades that
// exhaustion to issues.ErrSerializationRetry (nothing was written — the
// comment's version genuinely did not move, so ErrVersionConflict would
// be a lie here); map it to the SAME `serialization_retry` code Claim
// already uses for the identical situation, checked BEFORE
// ErrVersionConflict so a genuine version race and a lost serialization
// race are never confused.
func mapCommentWriteError(ctx context.Context, err error, tool, userID, commentID string) *mcpgo.CallToolResult {
	if errors.Is(err, issues.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"the comment write lost a serialization race and made no change — retry the call",
			map[string]any{"commentId": commentID})
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return skmcp.ToolError(ErrCodeVersionConflict,
			"the comment was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrCommentNotFound) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"comment does not exist or is not visible",
			map[string]any{"commentId": commentID})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible", nil)
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", tool, "reason", "comment write failed",
		"userId", userID, "commentId", commentID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "comment operation failed", nil)
}

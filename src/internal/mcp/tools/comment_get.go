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
)

// RegisterCommentGet wires the skein_comment_get tool (sk-242.1 US-4).
// Returns one comment by UUID as a full CommentDTO (mentions hydrated
// like ListComments). Soft-deleted rows come back as tombstones (body
// "", mentions []). Missing or invisible comments both map to
// comment_not_found (anti-oracle, FR-003 / INV-6).
//
// Annotations:
//
//   - readOnlyHint: true
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
func RegisterCommentGet(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_comment_get",
		mcpgo.WithDescription(
			"Fetch one comment by UUID as a full CommentDTO (id, issueId, "+
				"author, authorUserId, authorType, authorKind, body, createdAt, "+
				"updatedAt, editedAt, deletedAt, version, mentions). Mentions "+
				"are hydrated like ListComments (always a non-nil array). "+
				"Soft-deleted comments return as tombstones: body is \"\" and "+
				"mentions is []. Use this after a monitor poll surfaces a "+
				"truncated bodyPreview (or when you already hold a commentId) "+
				"instead of re-fetching the whole thread via skein_show.\n\n"+
				"Errors: invalid UUID → validation_error; missing id OR "+
				"comment whose project is not visible → comment_not_found "+
				"(same code — no existence leak).",
		),
		mcpgo.WithString("commentId",
			mcpgo.Required(),
			mcpgo.Description("UUID of the comment to fetch."),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleCommentGet(pool, issuesStore))
}

// commentGetResponse is the wire shape for skein_comment_get — mirrors
// skein_comment's envelope so agents branch on the same `comment` key.
type commentGetResponse struct {
	Comment *issues.CommentDTO `json:"comment"`
}

func handleCommentGet(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rawCommentID, err := req.RequireString("commentId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "commentId is required", nil), nil
		}
		commentID, err := uuid.Parse(strings.TrimSpace(rawCommentID))
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"commentId must be a valid uuid",
				map[string]any{"commentId": rawCommentID}), nil
		}

		// Meta-first (sibling write tools): authorize before loading full
		// body + mentions hydrate so invisible projects never pull secret
		// text into process memory (found in review). Miss and invisible both
		// map to comment_not_found (INV-6 anti-oracle).
		meta, err := issuesStore.GetCommentMeta(ctx, commentID)
		if err != nil {
			if errors.Is(err, issues.ErrCommentNotFound) {
				return skmcp.ToolError(ErrCodeCommentNotFound,
					"comment does not exist or is not visible",
					map[string]any{"commentId": commentID.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_comment_get",
				"reason", "get comment meta failed",
				"userId", userID.String(), "commentId", commentID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load comment", nil), nil
		}
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, meta.ProjectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return skmcp.ToolError(ErrCodeCommentNotFound,
					"comment does not exist or is not visible",
					map[string]any{"commentId": commentID.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_comment_get",
				"reason", "visibility check failed",
				"userId", userID.String(), "commentId", commentID.String(), "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil), nil
		}

		dto, _, err := issuesStore.GetComment(ctx, commentID)
		if err != nil {
			// Race: deleted between meta and full load → still not_found.
			if errors.Is(err, issues.ErrCommentNotFound) {
				return skmcp.ToolError(ErrCodeCommentNotFound,
					"comment does not exist or is not visible",
					map[string]any{"commentId": commentID.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_comment_get",
				"reason", "get comment failed",
				"userId", userID.String(), "commentId", commentID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load comment", nil), nil
		}

		// Defensive: store already guarantees non-nil mentions, but keep
		// wire contract even if a future path forgets.
		if dto.Mentions == nil {
			dto.Mentions = []issues.MentionRef{}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_comment_get",
			"userId", userID.String(), "commentId", commentID.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(commentGetResponse{Comment: dto})
	}
}

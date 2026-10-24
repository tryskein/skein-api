package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterCommentDelete wires the skein_comment_delete tool (sk-57).
// Soft-deletes a comment. AUTHOR OR project owner/admin may delete.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: true
//   - idempotentHint: true (idempotent — re-deleting is a no-op success)
//   - openWorldHint: false
func RegisterCommentDelete(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_comment_delete",
		mcpgo.WithDescription(
			"Soft-delete a comment. The comment author may always delete "+
				"their own comment; otherwise the caller must be an "+
				"owner/admin of the comment's project (a plain member who is "+
				"not the author gets `forbidden`). The comment body is "+
				"retained server-side for audit but blanked on read — the "+
				"row stays in the thread as a tombstone (\"comment deleted\"). "+
				"Idempotent: deleting an already-deleted comment is a no-op "+
				"success. Optional `reason` is recorded on the comment.deleted "+
				"event. A write that lost a database serialization race "+
				"(nothing written) returns `serialization_retry` — retry "+
				"the call verbatim.",
		),
		mcpgo.WithString("commentId",
			mcpgo.Required(),
			mcpgo.Description("UUID of the comment to delete."),
		),
		mcpgo.WithString("reason",
			mcpgo.Description("Optional free-form reason recorded on the comment.deleted event."),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	addTool(s, tool, handleCommentDelete(pool, issuesStore))
}

func handleCommentDelete(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
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
		reason := strings.TrimSpace(req.GetString("reason", ""))

		meta, level, errRes := resolveCommentForWrite(ctx, pool, issuesStore,
			"skein_comment_delete", userID, role, rawCommentID)
		if errRes != nil {
			return errRes, nil
		}
		// AUTHOR-OR-OWNER: the project owner's rights (an instance admin
		// has them, US-6, sk-741).
		isAuthor := meta.AuthorUserID != nil && *meta.AuthorUserID == userID
		isOwner := authz.Allows(level, authz.ActionDeleteAnyComment)
		if !isAuthor && !isOwner {
			return skmcp.ToolError(ErrCodeForbidden,
				"only the comment author, the project owner, the space owner or the instance admin can delete this comment", nil), nil
		}

		actor := IssuesAgentActor(ctx, pool, userID)
		if err := issuesStore.DeleteComment(ctx, meta.CommentID, reason, actor); err != nil {
			return mapCommentWriteError(ctx, err, "skein_comment_delete", userID.String(), meta.CommentID.String()), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_comment_delete",
			"userId", userID.String(), "commentId", meta.CommentID.String(),
			"latencyMs", time.Since(start).Milliseconds())
		// Tombstone form — minimal ack (the row's body is already blanked
		// in any subsequent ListComments).
		return skmcp.ToolJSON(map[string]any{
			"ok":      true,
			"id":      meta.CommentID,
			"issueId": meta.IssueID,
			"deleted": true,
		})
	}
}

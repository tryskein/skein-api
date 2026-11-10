package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
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
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.DeleteComment(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		// Tombstone form — minimal ack (the row's body is already blanked
		// in any subsequent ListComments).
		return skmcp.ToolJSON(map[string]any{
			"ok":      true,
			"id":      res.ID,
			"issueId": res.IssueID,
			"deleted": true,
		})
	}
}

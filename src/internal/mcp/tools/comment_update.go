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
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		dto, f := issueops.UpdateComment(ctx, d, userID, issueops.Args(req.GetArguments()), issueops.OriginMCP)
		if f != nil {
			return toolFailure(f), nil
		}
		return writeCommentResult(ctx, "skein_comment_update", userID.String(), dto.ID.String(), dto)
	}
}

// mapCommentWriteError maps a refusal of UpdateComment or DeleteComment to
// its envelope (issueops.MapCommentWriteError).
func mapCommentWriteError(ctx context.Context, err error, tool, userID, commentID string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapCommentWriteError(ctx, err, tool, userID, commentID))
}

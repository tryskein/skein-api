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
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		dto, f := issueops.CreateComment(ctx, d, userID, issueops.Args(req.GetArguments()), issueops.OriginMCP)
		if f != nil {
			return toolFailure(f), nil
		}
		return writeCommentResult(ctx, "skein_comment", userID.String(), dto.ID.String(), dto)
	}
}

// mapAddCommentError maps a refusal of AddComment to its envelope
// (issueops.MapAddCommentError).
func mapAddCommentError(ctx context.Context, err error, userID, issueID string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapAddCommentError(ctx, err, userID, issueID))
}

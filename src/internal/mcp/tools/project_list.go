package tools

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterProjectList wires the skein_project_list tool. Returns the
// projects the calling user can see — the access relation (INV-1): the
// instance admin → every project but those of other users' "Personal spaces";
// anyone else → the projects INV-1 gives him (his spaces' projects he has
// access to, his own "Personal space", an external user's memberships).
//
// Annotations:
//
//   - readOnlyHint: true
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta: none — the response is small (one row per project, slim
// fields), so the default 25k token budget is plenty.
//
// When the optional embedder is wired (OLLAMA_URL set in main.go) the
// `q` parameter triggers FTS + cosine-similarity rerank; otherwise
// `q` falls through to FTS-only.
func RegisterProjectList(s *server.MCPServer, projectsStore *projects.Store, embedder projects.QueryEmbedder) {
	tool := mcpgo.NewTool(
		"skein_project_list",
		mcpgo.WithDescription(
			"List projects visible to the calling user. BY DEFAULT a page is "+
				strconv.Itoa(ProjectListDefaultLimit)+" rows (ceiling "+
				strconv.Itoa(projects.ListProjectsMaxLimit)+"), and each row's "+
				"`description` is cut to the first "+
				strconv.Itoa(ProjectDescriptionPreviewMaxRunes)+" characters. When a "+
				"description was actually cut the row carries "+
				"`descriptionTruncated: true`; a short or empty description "+
				"carries no such key, so the marker means «there is more», "+
				"never «this might be all». The full text is not reachable "+
				"through any MCP tool today — this list is the only MCP "+
				"surface that renders it — so treat the preview as the "+
				"orientation aid it is and open the project in the web UI "+
				"when you need the whole thing.\n\n"+
				"Useful for orientation when an agent doesn't yet know the "+
				"project handle. Pair with skein_project_resolve when the "+
				"user types a fuzzy name. Honours `archived` to include "+
				"archived projects in the result. When `q` is set, items are "+
				"ordered by relevance score (FTS + optional semantic rerank) "+
				"instead of recency.",
		),
		mcpgo.WithBoolean("archived",
			mcpgo.Description("Include archived projects. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Page size, 1..100. Default: 50."),
			mcpgo.Min(1),
			mcpgo.Max(100),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Opaque cursor returned by a previous call's `nextCursor` field."),
		),
		mcpgo.WithString("q",
			mcpgo.Description("Optional free-form search query. Matches against project name, "+
				"description (Russian + English FTS), idPrefix, and aliases. When set, results "+
				"are ordered by relevance score; cursor pagination still works across pages."),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleProjectList(projectsStore, embedder))
}

func handleProjectList(projectsStore *projects.Store, embedder projects.QueryEmbedder) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		archived := "false"
		if req.GetBool("archived", false) {
			archived = "true"
		}
		limit := resolveMCPLimit(req.GetInt("limit", 0), ProjectListDefaultLimit, projects.ListProjectsMaxLimit)
		// The underlying store clamps to [1, 100]; the schema enforces
		// the same upper bound so callers see an honest contract.
		// sk-524 — the name is REMOVED from here, and that is the fix. Over MCP
		// `auth.UsernameFrom` returns nothing, and the gate of the lens
		// counters stood on exactly it: the same user
		// saw real numbers in the web and zeros here. The gate now
		// asks for CallerID, which was always available here.
		args := projects.ListProjectsArgs{
			CallerID: userID,
			Archived: archived,
			Query:    strings.TrimSpace(req.GetString("q", "")),
			Embedder: embedder,
			Limit:    limit,
		}
		if cursor := strings.TrimSpace(req.GetString("cursor", "")); cursor != "" {
			cur, decErr := projects.DecodeListCursor(cursor)
			if decErr != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"invalid cursor: "+decErr.Error(), nil), nil
			}
			args.Cursor = &cur
		}

		res, err := projectsStore.ListProjects(ctx, args)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_list",
				"reason", "list failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list projects", nil), nil
		}

		payload, perr := projectListPageRows(res)
		if perr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_list",
				"reason", "row projection failed",
				"userId", userID.String(), "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_list",
			"userId", userID.String(), "count", len(payload.Items),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(payload)
	}
}

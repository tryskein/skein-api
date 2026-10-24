package tools

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/search"
)

// RegisterSearch wires the skein_search tool. Wraps search.Service.Search;
// scope defaults to cross-project across the user's visible projects, with
// optional narrow to one project. Honours the same filter set as the REST
// /search endpoint.
//
// In R2.2 the embedder is allowed to be nil — the service falls into
// "degraded:true" FTS-only mode and returns the same shape with the
// degraded flag set. The MCP tool just forwards that flag to the agent.
//
// Annotations:
//
//   - readOnlyHint: true
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta:
//
//   - anthropic/maxResultSizeChars: 200000 — search may return up to 100
//     items with snippets; the cap mirrors skein_list.
func RegisterSearch(s *server.MCPServer, pool *pgxpool.Pool, svc *search.Service, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_search",
		mcpgo.WithDescription(
			"Hybrid search across visible projects (FTS + vector when "+
				"available, FTS-only when not). BY DEFAULT a page is "+
				strconv.Itoa(SearchLimitDefault)+" hits (ceiling "+
				strconv.Itoa(search.MaxPageLimit)+"), and a hit carries a PLAIN "+
				"snippet — no highlight markup of any kind, so nothing has to "+
				"be stripped before the text is read or matched. Returns "+
				"issues, comments, projects, and components by default; "+
				"narrow with `kinds`. To read a hit in full, follow its id: "+
				"skein_show for an issue, skein_comment_get for a comment. "+
				"Use this for free-form questions / fuzzy matches; use "+
				"skein_list when you have a specific project + filter "+
				"combination. Response includes `degraded:true` when the "+
				"vector retriever was unavailable.",
		),
		mcpgo.WithString("query",
			mcpgo.Required(),
			mcpgo.Description("Search query, 1..500 characters."),
		),
		mcpgo.WithString("project",
			mcpgo.Description("Optional project handle (alias|name|UUID) to scope to one project. Default: all visible projects."),
		),
		mcpgo.WithString("space",
			mcpgo.Description("Optional space short id (from skein_whoami spaces) to scope to one space's projects. Default: every space you see."),
		),
		mcpgo.WithArray("kinds",
			mcpgo.Description("Restrict to entity kinds: any of issue|comment|project|component. Default: all four."),
			mcpgo.WithStringEnumItems([]string{"issue", "comment", "project", "component"}),
		),
		mcpgo.WithArray("statuses",
			mcpgo.Description("Issue status filter (only honoured when kinds = issue). "+
				"`standing` selects perpetual obligations. "+
				"`blocked` is no longer a writable status."),
			mcpgo.WithStringEnumItems([]string{"open", "in_progress", "deferred", "closed", "standing"}),
		),
		mcpgo.WithArray("priorities",
			mcpgo.Description("Issue priority filter (0..4)."),
			mcpgo.WithNumberItems(mcpgo.Min(0), mcpgo.Max(4)),
		),
		mcpgo.WithArray("types",
			mcpgo.Description("Issue type filter."),
			mcpgo.WithStringEnumItems([]string{"bug", "feature", "task", "epic", "chore", "decision", "idea", "note"}),
		),
		mcpgo.WithArray("labels",
			mcpgo.Description("Issue label filter (any-of)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithString("source",
			mcpgo.Description("Issue origin filter: human|agent_directed|agent_autonomous."),
			mcpgo.Enum("human", "agent_directed", "agent_autonomous"),
		),
		mcpgo.WithArray("commentAuthorKinds",
			mcpgo.Description("Comment author-kind filter (only restricts COMMENT results; any-of). "+
				"Restricts comments by comments.author_kind — independent of `source`, "+
				"which filters issues. Does not affect issue/project/component results."),
			mcpgo.WithStringEnumItems([]string{"human", "agent_directed", "agent_autonomous"}),
		),
		mcpgo.WithNumber("limit",
			// The bounds are the APPLIED constants, not literals: INV-5
			// says the schema may not announce a number the handler does
			// not use, and a literal here is exactly how the two drift
			// apart without anything failing (sk-282 T026).
			mcpgo.Description("Page size, 1.."+strconv.Itoa(search.MaxPageLimit)+
				". Default: "+strconv.Itoa(SearchLimitDefault)+"."),
			mcpgo.Min(1),
			mcpgo.Max(search.MaxPageLimit),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Opaque cursor returned by a previous call's `nextCursor` field."),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 200000,
	})

	addTool(s, tool, handleSearch(pool, svc, projectsStore))
}

func handleSearch(pool *pgxpool.Pool, svc *search.Service, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		query, err := req.RequireString("query")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "query is required", nil), nil
		}
		query = strings.TrimSpace(query)
		if query == "" {
			return skmcp.ToolError(ErrCodeValidationError, "query is required", nil), nil
		}
		if len(query) > 500 {
			return skmcp.ToolError(ErrCodeValidationError,
				"query must be at most 500 characters", nil), nil
		}

		// Optional project narrow.
		var projectID *uuid.UUID
		if rawProj := strings.TrimSpace(req.GetString("project", "")); rawProj != "" {
			pid, perr := ResolveProjectHandle(ctx, pool, projectsStore, rawProj, userID)
			if perr != nil {
				return projectErrorResult(perr)
			}
			projectID = &pid
		}

		// Kinds default = all.
		var kinds []search.Kind
		if rawKinds := req.GetStringSlice("kinds", nil); len(rawKinds) > 0 {
			seen := make(map[search.Kind]struct{}, len(rawKinds))
			for _, k := range rawKinds {
				k = strings.TrimSpace(k)
				if !search.IsValidKind(k) {
					return skmcp.ToolError(ErrCodeValidationError,
						"kinds[i] must be one of issue|comment|project|component",
						map[string]any{"kind": k}), nil
				}
				kk := search.Kind(k)
				if _, dup := seen[kk]; !dup {
					seen[kk] = struct{}{}
					kinds = append(kinds, kk)
				}
			}
		}
		if len(kinds) == 0 {
			kinds = search.AllKinds
		}

		filters := search.Filters{}
		if statuses := req.GetStringSlice("statuses", nil); len(statuses) > 0 {
			// sk-31: validate each status through ValidateStatus so the
			// `blocked_is_derived` code surfaces as a top-level error
			// (via validationToolError) instead of leaking a stale
			// "blocked" string into the search filter set.
			canon := make([]string, 0, len(statuses))
			for _, s := range statuses {
				v, vErr := issues.ValidateStatus(strings.TrimSpace(s))
				if vErr != nil {
					return validationToolError(vErr), nil
				}
				canon = append(canon, v)
			}
			filters.Statuses = canon
		}
		if priorities := req.GetIntSlice("priorities", nil); len(priorities) > 0 {
			for _, p := range priorities {
				if p < 0 || p > 4 {
					return skmcp.ToolError(ErrCodeValidationError,
						"priorities[i] must be in [0, 4]",
						map[string]any{"priority": p}), nil
				}
			}
			filters.Priorities = priorities
		}
		if types := req.GetStringSlice("types", nil); len(types) > 0 {
			filters.Types = trimAll(types)
		}
		if labels := req.GetStringSlice("labels", nil); len(labels) > 0 {
			filters.Labels = trimAll(labels)
		}
		if src := strings.TrimSpace(req.GetString("source", "")); src != "" {
			switch src {
			case issues.SourceHuman, issues.SourceAgentDirected, issues.SourceAgentAutonomous:
				v := src
				filters.Source = &v
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"source must be one of: human, agent_directed, agent_autonomous",
					map[string]any{"source": src}), nil
			}
		}
		// commentAuthorKinds: comment-only filter (restricts COMMENT results
		// by comments.author_kind). Independent of `source` (which filters
		// issues). Validate each value against the author_kind enum; unknown
		// → validation_error.
		if rawKinds := req.GetStringSlice("commentAuthorKinds", nil); len(rawKinds) > 0 {
			canon := make([]string, 0, len(rawKinds))
			for _, k := range rawKinds {
				k = strings.TrimSpace(k)
				switch k {
				case issues.CommentAuthorKindHuman,
					issues.CommentAuthorKindAgentDirected,
					issues.CommentAuthorKindAgentAutonomous:
					canon = append(canon, k)
				default:
					return skmcp.ToolError(ErrCodeValidationError,
						"commentAuthorKinds[i] must be one of: human, agent_directed, agent_autonomous",
						map[string]any{"commentAuthorKind": k}), nil
				}
			}
			filters.CommentAuthorKinds = canon
		}

		limit := req.GetInt("limit", 0)
		if limit < 0 {
			return skmcp.ToolError(ErrCodeValidationError,
				"limit must be a positive integer", nil), nil
		}
		// sk-282 T015: the default page size is decided HERE. The search
		// service is shared with the REST search endpoint, whose page size
		// INV-2 freezes; the ceiling stays the service's own.
		limit = resolveMCPLimit(limit, SearchLimitDefault, search.MaxPageLimit)
		cursor := strings.TrimSpace(req.GetString("cursor", ""))

		params := search.Params{
			Query:    query,
			Project:  projectID,
			Space:    strings.TrimSpace(req.GetString("space", "")),
			Kinds:    kinds,
			Limit:    limit,
			Cursor:   cursor,
			Filters:  filters,
			CallerID: userID,
		}

		res, err := svc.Search(ctx, params)
		if err != nil {
			if errors.Is(err, search.ErrInvalidCursor) {
				return skmcp.ToolError(ErrCodeValidationError,
					"invalid cursor: "+err.Error(), nil), nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return skmcp.ToolError(ErrCodeInternalError,
					"search cancelled before completion", nil), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_search",
				"reason", "search failed",
				"userId", userID.String(), "queryLen", len(query), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "search failed", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_search",
			"userId", userID.String(), "queryLen", len(query),
			"resultCount", len(res.Items), "degraded", res.Degraded,
			"latencyMs", time.Since(start).Milliseconds())
		// The highlight markup is removed at the MCP wire edge only — the
		// same service instance feeds the REST endpoint that renders it.
		return skmcp.ToolJSON(projectSearchResult(res))
	}
}

// trimAll strips whitespace from every element and drops empties. Used
// for filter slices where the JSON Schema can't enforce non-empty
// strings.
func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterProjectResolve wires the skein_project_resolve tool. Wraps the
// projects.Resolve trigram resolver — the same path used by the REST
// /projects/resolve endpoint and by the BFF for fuzzy name lookups.
//
// Annotations:
//
//   - readOnlyHint: true
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta: none — response is at most 10 candidates, well under any cap.
func RegisterProjectResolve(s *server.MCPServer, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_project_resolve",
		mcpgo.WithDescription(
			"Resolve a free-form project handle (alias, name, repo URL, or "+
				"fuzzy/typo input) into a canonical project. Returns "+
				"`match: {id, name, ...}` when the resolver is confident, "+
				"or `match: null` plus a `candidates` list of up to 10 "+
				"alternatives when ambiguous. Use before any tool that "+
				"takes a `project` arg if the input came from a human "+
				"(\"demo-service\", \"the api one\", etc.).",
		),
		mcpgo.WithString("q",
			mcpgo.Required(),
			mcpgo.Description("Free-form project handle to resolve."),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleProjectResolve(projectsStore))
}

// projectResolveCandidate is the slim shape we emit per candidate. We
// alias projects.ResolveMatch fields onto string ids and a single
// `score` for the agent — cleaner than dumping the full match-via taxonomy.
type projectResolveCandidate struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	IDPrefix   string  `json:"idPrefix"`
	Score      float64 `json:"score"`
	MatchedVia string  `json:"matchedVia"`
}

type projectResolveResponse struct {
	Match      *projectResolveCandidate  `json:"match"`
	Candidates []projectResolveCandidate `json:"candidates"`
}

func handleProjectResolve(projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		q, err := req.RequireString("q")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "q is required", nil), nil
		}

		res, err := projectsStore.Resolve(ctx, q, userID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_resolve",
				"reason", "resolve failed",
				"userId", userID.String(), "qLen", len(q), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "resolve failed", nil), nil
		}

		if res.Ambiguous {
			// INV-29, SC-024: the exact name of several accessible projects
			// is the final refusal on every path — the same body as REST.
			return skmcp.ToolError(ErrCodeAmbiguousProject,
				"the name matches several projects; name one by its prefix",
				map[string]any{"q": strings.TrimSpace(q), "candidates": projects.AmbiguousCandidates(res.Candidates)}), nil
		}
		out := projectResolveResponse{
			Candidates: make([]projectResolveCandidate, 0, len(res.Candidates)),
		}
		if res.Match != nil {
			m := projectResolveCandidate{
				ID:         res.Match.ID.String(),
				Name:       res.Match.Name,
				IDPrefix:   res.Match.IDPrefix,
				Score:      res.Match.Similarity,
				MatchedVia: res.Match.MatchedVia,
			}
			out.Match = &m
		}
		for _, c := range res.Candidates {
			out.Candidates = append(out.Candidates, projectResolveCandidate{
				ID:         c.ID.String(),
				Name:       c.Name,
				IDPrefix:   c.IDPrefix,
				Score:      c.Similarity,
				MatchedVia: c.MatchedVia,
			})
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_resolve",
			"userId", userID.String(),
			"matched", out.Match != nil, "candidates", len(out.Candidates),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(out)
	}
}

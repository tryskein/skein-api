package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterReady wires the skein_ready tool. Returns the open-and-not-
// blocked issues for one project, sorted by priority then last-update.
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
//   - anthropic/maxResultSizeChars: 200000 — RAISED from 100000 by sk-282
//     T026, because the old number was below a response this tool can
//     legally produce and INV-5 does not tolerate that in either direction.
//
//     Measured, not estimated (2026-08-20, throwaway harness on the
//     integration database): a full legal page — the schema ceiling of 100
//     rows, each with a 1000-character title, which is the maximum a title
//     may be and which is deliberately never truncated — serialises to
//     127 130 bytes. The previous declaration was 100 000. The rows here
//     really are slim (no labels, no components, no prose), and that is
//     exactly why the estimate behind the old number felt safe: it counted
//     the service part and forgot that the page is bounded by titles.
//
//     Under-declaring is not a truncated answer: an over-budget result is
//     persisted to disk and handed back as a file reference in the
//     conversation (result_size.go). So the failure mode this fixes is a
//     hundred-row ready list silently leaving the conversation.
func RegisterReady(s *server.MCPServer, pool *pgxpool.Pool, depsStore *dependencies.Store, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_ready",
		mcpgo.WithDescription(
			"List unblocked open issues for a project, ordered by priority "+
				"(P0 first) then most-recently-updated. \"Ready\" means "+
				"status=open AND no open blocks/waits-for dependencies. By "+
				"default — and in every mode, there is no other one — a row is "+
				"the lean summary: no description, no comments, and two keys "+
				"missing outright (below). Pair with skein_show on the top hit "+
				"to start work.\n\n"+
				"Rows carry NO `labels` and NO `componentIds` keys: this query "+
				"does not load them (the aggregate joins would make it N+1), "+
				"and returning an empty array would assert that the issue has "+
				"none. The keys are absent rather than empty so you can tell "+
				"«not supplied here» from «none». Read both with skein_show "+
				"on the issue — its card carries them. skein_list carries "+
				"`labels` in its default row, but NOT `componentIds`: there "+
				"you have to name it, skein_list(fields:[\"componentIds\"]). "+
				"There is no `isBlocked` key here either, and that "+
				"one needs no lookup: every row on this list is unblocked — "+
				"that is the predicate the list selects on.",
		),
		mcpgo.WithString("project",
			mcpgo.Required(),
			mcpgo.Description("Project handle: alias, name, or UUID."),
		),
		mcpgo.WithString("assignee",
			mcpgo.Description("Exact assignee match."),
		),
		mcpgo.WithNumber("priorityMax",
			mcpgo.Description("Upper bound on priority — issues with priority <= N are returned."),
			mcpgo.Min(0),
			mcpgo.Max(4),
		),
		mcpgo.WithString("type",
			mcpgo.Description("Exact issue-type match (bug|feature|task|epic|chore|decision|idea|note)."),
			mcpgo.Enum("bug", "feature", "task", "epic", "chore", "decision", "idea", "note"),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Page size, 1..100. Default: 25."),
			mcpgo.Min(1),
			mcpgo.Max(100),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 200000,
	})

	addTool(s, tool, handleReady(pool, depsStore, projectsStore))
}

// readyResponse is the wire shape — items + count for cheap UI rendering.
type readyResponse struct {
	// Items are projected rows (ready_rows.go), not the carrier struct:
	// the `labels` and `componentIds` keys are removed because this query
	// never loads them, and an empty array in their place asserts «this
	// issue has no labels» on every row (US-9 / SC-010).
	Items []map[string]any `json:"items"`
	Count int              `json:"count"`
}

func handleReady(pool *pgxpool.Pool, depsStore *dependencies.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		project, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, project, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		filters := dependencies.ReadyFilters{}
		if a := strings.TrimSpace(req.GetString("assignee", "")); a != "" {
			filters.Assignee = &a
		}
		if v, ok := req.GetArguments()["priorityMax"]; ok && v != nil {
			p := req.GetInt("priorityMax", -1)
			if p < 0 || p > 4 {
				return skmcp.ToolError(ErrCodeValidationError,
					"priorityMax must be in [0, 4]", nil), nil
			}
			filters.Priority = &p
		}
		if t := strings.TrimSpace(req.GetString("type", "")); t != "" {
			canon, vErr := issues.ValidateType(t)
			if vErr != nil {
				return skmcp.ToolError(ErrCodeValidationError, vErr.Error(), nil), nil
			}
			filters.Type = &canon
		}

		// Default 25 (per spec) — the store clamps to [1, 100] anyway, but
		// we override the store's 50-default so an unconfigured caller
		// gets the spec's preferred page size.
		limit := req.GetInt("limit", 25)
		if limit < 0 {
			return skmcp.ToolError(ErrCodeValidationError,
				"limit must be a positive integer", nil), nil
		}

		// Visibility check is the project_members + admin path; we run
		// it even though the store doesn't filter (the store has no
		// notion of "caller can see this project") so we don't leak the
		// existence of issues in invisible projects.
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, projectID, userID); vErr != nil {
			return projectErrorResult(&ProjectNotFoundError{Query: project})
		}

		items, err := depsStore.ListReady(ctx, projectID, filters, limit)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_ready",
				"reason", "list failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list ready issues", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_ready",
			"userId", userID.String(), "projectId", projectID.String(),
			"count", len(items), "latencyMs", time.Since(start).Milliseconds())
		rows, perr := projectReadyRows(items)
		if perr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_ready",
				"reason", "row projection failed",
				"userId", userID.String(), "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}
		return skmcp.ToolJSON(readyResponse{Items: rows, Count: len(rows)})
	}
}

package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// componentListResponse is the wire shape — no pagination, because
// component counts are bounded.
//
// ArchivedCount (sk-359) is how many components the project has in the
// archived state, ALWAYS present and independent of `archivedFilter`. Under
// the default filter it is therefore exactly the number of rows the filter
// held back, which is the question the field exists to answer: an empty
// `items` with archivedCount=0 means the project has no components at all
// (a project-level issue is the right call), while an empty `items` with
// archivedCount>0 means every component is archived — unarchive one or ask
// the user rather than silently creating an untagged issue. It is reported
// beside a non-empty `items` too, so "is anything hidden?" is answerable on
// every call, not only on an empty one.
type componentListResponse struct {
	Items         []projects.ComponentDTO `json:"items"`
	ArchivedCount int                     `json:"archivedCount"`
}

// handleComponentList backs skein_component(action="list") — the
// components of one project, with the same archived semantics as the REST
// endpoint (default: active only).
//
// The filter arrives as `archivedFilter`, not `archived` (sk-255.3). This
// is the one rename in the consolidation that was not a matter of taste:
// the same flat schema also serves action="archive", where `archived` is
// a BOOLEAN that decides archive-vs-unarchive, while here it is a string
// enum with three values ("false" | "true" | "all"). JSON Schema gives one
// type per property name, so the two could not both keep the name — and
// the read side is the cheaper one for callers to relearn.
func handleComponentList(pool *pgxpool.Pool, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// Stale-discriminator guard. `archived` was this filter's name five
		// commits ago and it still exists in this same flat schema — as the
		// BOOLEAN that action="archive" reads. A caller that sends
		// archived:"true" here is asking for archived rows and, ignored,
		// gets the active ones back: a silently inverted answer, which is
		// the failure mode mark_read.go rejects rather than swallows. Runs
		// before project resolution — a rejected request must not cost a
		// database round trip.
		if _, stale := req.GetArguments()["archived"]; stale {
			return skmcp.ToolError(ErrCodeValidationError,
				"archived is the boolean read by action=\"archive\" — the list filter is "+
					"archivedFilter (\"false\" | \"true\" | \"all\")",
				map[string]any{"action": "list"}), nil
		}

		project, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, project, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		// Belt-and-braces visibility check. ResolveProjectHandle already
		// vets visibility, but a fast-path UUID with a stale cache could
		// in theory slip through; running the check twice is cheap.
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, projectID, userID); vErr != nil {
			return projectErrorResult(&ProjectNotFoundError{Query: project})
		}

		archived := strings.TrimSpace(req.GetString("archivedFilter", "false"))
		switch archived {
		case "false", "true", "all":
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"archivedFilter must be one of: false, true, all", nil), nil
		}

		// Username threads through for the My-assigned-mode (outbox)
		// per-status counts on each ComponentDTO. MCP tools rarely surface
		// the My* counts, but populating them keeps the wire shape
		// consistent with the REST path so a frontend mirror stays correct.
		callerUsername, _ := auth.UsernameFrom(ctx)
		items, err := projectsStore.ListComponents(ctx, projectID, archived, userID, callerUsername)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_component", "action", "list",
				"reason", "list failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list components", nil), nil
		}

		archivedCount, err := projectsStore.CountArchivedComponents(ctx, projectID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_component", "action", "list",
				"reason", "archived count failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list components", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_component", "action", "list",
			"userId", userID.String(), "projectId", projectID.String(),
			"count", len(items), "archivedCount", archivedCount, "latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(componentListResponse{Items: items, ArchivedCount: archivedCount})
	}
}

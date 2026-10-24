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

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// componentArchiveResponse is the wire shape — full ComponentDTO post-state.
type componentArchiveResponse struct {
	Component *projects.ComponentDTO `json:"component"`
}

// handleComponentArchive backs skein_component(action="archive") — wraps
// projects.Store.ArchiveComponent / UnarchiveComponent. Project owner, space owner or instance admin only. The
// store exposes the two directions as separate methods (mirroring the REST
// POST /archive vs /unarchive endpoints); the boolean `archived` picks
// between them, defaulting to true.
//
// Idempotent intent: "archived=true on an already-archived component" and
// "archived=false on an active component" return the post-state without
// error. The store surfaces those as ErrAlreadyArchived / ErrNotArchived;
// we swallow them, re-fetch, and return the same shape so the agent does
// not have to handle a no-op specially.
//
// Note the asymmetry with action="list", which filters on `archivedFilter`
// — a three-valued string enum, not this boolean. Same word, two types,
// so the flat schema had to give them two names.
func handleComponentArchive(pool *pgxpool.Pool, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// ---- Resolve project ----
		projectArg, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, projectArg, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		// ---- Owner gate ----
		if _, vErr := projects.EnsureProjectOwner(ctx, pool, projectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return skmcp.ToolError(ErrCodeProjectNotFound,
					"project does not exist or is not visible",
					map[string]any{"project": projectArg}), nil
			}
			if errors.Is(vErr, projects.ErrProjectNotOwner) {
				return skmcp.ToolError(ErrCodeForbidden,
					"only the project owner, the space owner or the instance admin can archive components",
					map[string]any{"project": projectArg}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_component", "action", "archive",
				"reason", "owner check failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "owner check failed", nil), nil
		}

		// ---- Required: id ----
		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		componentID, perr2 := uuid.Parse(strings.TrimSpace(rawID))
		if perr2 != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a UUID",
				map[string]any{"id": rawID}), nil
		}

		// archived defaults to true (= archive). false toggles to unarchive.
		archive := req.GetBool("archived", true)
		actor := ProjectsAgentActor(ctx, pool, userID)

		// Route to the right store call. For the no-op branch (already in
		// the requested state) we swallow the typed error and fall through
		// to the post-state fetch — that's the documented idempotent
		// behaviour.
		if archive {
			if err := projectsStore.ArchiveComponent(ctx, projectID, componentID, actor); err != nil {
				if errors.Is(err, projects.ErrAlreadyArchived) {
					// Idempotent: fall through to post-state fetch.
				} else {
					return mapComponentArchiveError(ctx, err, userID, projectID, componentID, true), nil
				}
			}
		} else {
			if err := projectsStore.UnarchiveComponent(ctx, projectID, componentID, actor); err != nil {
				if errors.Is(err, projects.ErrNotArchived) {
					// Idempotent: fall through to post-state fetch.
				} else {
					return mapComponentArchiveError(ctx, err, userID, projectID, componentID, false), nil
				}
			}
		}

		// Always return the post-state so the agent has a single contract
		// regardless of whether the call mutated state or short-circuited
		// idempotently.
		dto, err := projectsStore.GetComponent(ctx, projectID, componentID)
		if err != nil {
			if errors.Is(err, projects.ErrNotFound) {
				return skmcp.ToolError(ErrCodeValidationError,
					"component not found in this project",
					map[string]any{"componentId": componentID.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_component", "action", "archive",
				"reason", "post-state load failed",
				"userId", userID.String(), "projectId", projectID.String(),
				"componentId", componentID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "post-state load failed", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_component", "action", "archive",
			"userId", userID.String(), "projectId", projectID.String(),
			"componentId", componentID.String(), "archived", archive,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(componentArchiveResponse{Component: dto})
	}
}

// mapComponentArchiveError translates archive/unarchive errors into MCP
// codes. ErrAlreadyArchived / ErrNotArchived are NOT routed here — the
// caller swallows them up the stack to make the tool idempotent.
func mapComponentArchiveError(ctx context.Context, err error, userID, projectID, componentID uuid.UUID, archive bool) *mcpgo.CallToolResult {
	if vErr, ok := projects.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, projects.ErrNotFound) {
		return skmcp.ToolError(ErrCodeValidationError,
			"component not found in this project",
			map[string]any{"componentId": componentID.String()})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_component", "action", "archive",
		"reason", "store call failed",
		"userId", userID.String(),
		"projectId", projectID.String(),
		"componentId", componentID.String(),
		"archive", archive, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to update archive state", nil)
}

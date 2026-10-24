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

// componentCreateResponse is the wire shape returned by
// skein_component(action="create").
type componentCreateResponse struct {
	Component *projects.ComponentDTO `json:"component"`
}

// handleComponentCreate backs skein_component(action="create") — wraps
// projects.Store.CreateComponent. Components are project-scoped and
// managed by the project owner, the space owner or the instance admin
// (members cannot create components — same rule as REST): the action
// surfaces forbidden when the caller is a member but not an owner,
// and project_not_found when the caller cannot see the project at all.
// `name` must be unique within the project (the DB UNIQUE(project_id,
// name) is case-sensitive and exact); path-list (paths[]) is not exposed
// here, use the REST PATCH endpoint once the component exists.
func handleComponentCreate(pool *pgxpool.Pool, projectsStore *projects.Store) server.ToolHandlerFunc {
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
		// EnsureProjectOwner returns ErrProjectNotVisible (→ project_not_found)
		// for non-members and ErrProjectNotOwner (→ forbidden) for plain
		// members. We deliberately surface forbidden as a distinct code so
		// the agent can prompt the human to either escalate or stop.
		if _, vErr := projects.EnsureProjectOwner(ctx, pool, projectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return skmcp.ToolError(ErrCodeProjectNotFound,
					"project does not exist or is not visible",
					map[string]any{"project": projectArg}), nil
			}
			if errors.Is(vErr, projects.ErrProjectNotOwner) {
				return skmcp.ToolError(ErrCodeForbidden,
					"only the project owner, the space owner or the instance admin can create components",
					map[string]any{"project": projectArg}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_component", "action", "create",
				"reason", "owner check failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "owner check failed", nil), nil
		}

		// ---- Required: name ----
		rawName, err := req.RequireString("name")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "name is required", nil), nil
		}
		name := strings.TrimSpace(rawName)
		if name == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"name must be non-empty after trim", nil), nil
		}
		if vErr := projects.ValidateProse("name", name); vErr != nil {
			return projectsValidationToolError(vErr), nil
		}

		description := req.GetString("description", "")
		if vErr := projects.ValidateProse("description", description); vErr != nil {
			return projectsValidationToolError(vErr), nil
		}

		// ---- Create ----
		actor := ProjectsAgentActor(ctx, pool, userID)
		dto, err := projectsStore.CreateComponent(ctx, projectID, userID,
			projects.CreateComponentArgs{
				Name:        name,
				Description: description,
				Paths:       nil, // path-list not exposed in R2.2c
			}, actor)
		if err != nil {
			return mapComponentCreateError(ctx, err, userID, projectID, name), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_component", "action", "create",
			"userId", userID.String(), "projectId", projectID.String(),
			"componentId", dto.ID.String(), "name", dto.Name,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(componentCreateResponse{Component: dto})
	}
}

// mapComponentCreateError translates store errors into stable MCP codes.
// Constraint matching mirrors handler.mapConflict for the "name already
// exists in project" case.
func mapComponentCreateError(ctx context.Context, err error, userID, projectID uuid.UUID, name string) *mcpgo.CallToolResult {
	if vErr, ok := projects.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var conflict *projects.ErrConflict
	if errors.As(err, &conflict) {
		if strings.Contains(conflict.Constraint, "components_project_id_name") {
			return skmcp.ToolError(ErrCodeIDTaken,
				"a component with this name already exists in the project",
				map[string]any{"name": name, "constraint": conflict.Constraint})
		}
		return skmcp.ToolError(ErrCodeIDTaken,
			"unique constraint violation",
			map[string]any{"constraint": conflict.Constraint})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_component", "action", "create",
		"reason", "create failed",
		"userId", userID.String(), "projectId", projectID.String(),
		"name", name, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to create component", nil)
}

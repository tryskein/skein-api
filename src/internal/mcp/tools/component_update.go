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

// componentUpdateResponse is the wire shape — full ComponentDTO post-state.
type componentUpdateResponse struct {
	Component *projects.ComponentDTO `json:"component"`
}

// handleComponentUpdate backs skein_component(action="update") — wraps
// projects.Store.UpdateComponent. Project owner, space owner or instance admin only. Components do not carry an
// explicit `version` column (no optimistic concurrency on this entity), so
// the action takes no `expectedVersion` — the store does a straight UPDATE
// inside a SERIALIZABLE transaction and the second writer wins.
//
// `name` and `description` are independent patches: omit a field to leave
// it unchanged. Passing an empty string clears the field (description =
// ""); the store COALESCEs only when the arg is nil.
func handleComponentUpdate(pool *pgxpool.Pool, projectsStore *projects.Store) server.ToolHandlerFunc {
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
					"only the project owner, the space owner or the instance admin can update components",
					map[string]any{"project": projectArg}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_component", "action", "update",
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

		// ---- Build patch input. Distinguish "absent" from "empty string". ----
		args := projects.UpdateComponentArgs{}
		argsMap := req.GetArguments()

		if _, ok := argsMap["name"]; ok {
			v := strings.TrimSpace(req.GetString("name", ""))
			if v == "" {
				return skmcp.ToolError(ErrCodeValidationError,
					"name must be non-empty after trim", nil), nil
			}
			if vErr := projects.ValidateProse("name", v); vErr != nil {
				return projectsValidationToolError(vErr), nil
			}
			args.Name = &v
		}
		if _, ok := argsMap["description"]; ok {
			v := req.GetString("description", "")
			if vErr := projects.ValidateProse("description", v); vErr != nil {
				return projectsValidationToolError(vErr), nil
			}
			args.Description = &v
		}

		// ---- Update ----
		actor := ProjectsAgentActor(ctx, pool, userID)
		dto, err := projectsStore.UpdateComponent(ctx, projectID, componentID, args, actor)
		if err != nil {
			return mapComponentUpdateError(ctx, err, userID, projectID, componentID), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_component", "action", "update",
			"userId", userID.String(), "projectId", projectID.String(),
			"componentId", componentID.String(),
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(componentUpdateResponse{Component: dto})
	}
}

// mapComponentUpdateError translates UpdateComponent errors into MCP codes.
func mapComponentUpdateError(ctx context.Context, err error, userID, projectID, componentID uuid.UUID) *mcpgo.CallToolResult {
	if vErr, ok := projects.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, projects.ErrNotFound) {
		// Surface as validation_error (component_id refers to a row that
		// either doesn't exist or belongs to a different project). The
		// closed error-code set has no dedicated component_not_found —
		// validation_error with code carries the same actionable info.
		return skmcp.ToolError(ErrCodeValidationError,
			"component not found in this project",
			map[string]any{"componentId": componentID.String()})
	}
	var conflict *projects.ErrConflict
	if errors.As(err, &conflict) {
		if strings.Contains(conflict.Constraint, "components_project_id_name") {
			return skmcp.ToolError(ErrCodeIDTaken,
				"a component with this name already exists in the project",
				map[string]any{"constraint": conflict.Constraint})
		}
		return skmcp.ToolError(ErrCodeIDTaken,
			"unique constraint violation",
			map[string]any{"constraint": conflict.Constraint})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_component", "action", "update",
		"reason", "update failed",
		"userId", userID.String(), "projectId", projectID.String(),
		"componentId", componentID.String(), "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to update component", nil)
}

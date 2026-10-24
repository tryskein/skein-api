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
	"skein-api/internal/spaces"
)

// RegisterProjectUpdate wires the skein_project_update tool. Wraps
// projects.Store.PatchProject — a wide-scope project patch covering name,
// description, gitlabGroup, archive flag, alias add/remove batches, and
// gitRepos add/remove batches. Every change lands inside one
// SERIALIZABLE transaction so partial-write failures (e.g. one alias of
// the batch is taken) roll back the entire patch.
//
// Permission: caller must be admin OR project owner. Plain members are
// rejected with `forbidden`. Invisible projects collapse to
// `project_not_found` (no leak — see vision §10).
//
// At least one of name / description / gitlabGroup / archived /
// aliasesAdd / aliasesRemove / gitReposAdd / gitReposRemove must be
// present, else `validation_error` (no-op).
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: true     — gitReposRemove / aliasesRemove / archived
//     are destructive operations
//   - idempotentHint: false     — re-running with the same gitReposAdd
//     entries appends duplicates
//   - openWorldHint: false
func RegisterProjectUpdate(s *server.MCPServer, pool *pgxpool.Pool, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_project_update",
		mcpgo.WithDescription(
			"Wide-scope patch for an existing project (its owner, the owner of its space or the instance admin only). "+
				"Optional fields: `name`, `description`, `gitlabGroup` (empty "+
				"string clears), `archived` (true|false), `aliasesAdd`, "+
				"`aliasesRemove`, `gitReposAdd`, `gitReposRemove` (by repo "+
				"UUID). At least one must be present. Every change lands in "+
				"one SERIALIZABLE tx — a single bad alias collision or repo "+
				"removal that hits an in-use repo rolls back the entire "+
				"patch. `visibility` (space|members) switches who sees the "+
				"project: members → space gives it to every member of the "+
				"space, space → members takes it from everyone without a "+
				"project membership. A project never changes its space: "+
				"a `space` value is refused with `space_change_forbidden` and nothing "+
				"changes. Returns the updated ProjectDTO.",
		),
		mcpgo.WithString("project",
			mcpgo.Required(),
			mcpgo.Description("Project handle: alias, name, idPrefix, or UUID."),
		),
		mcpgo.WithString("name",
			mcpgo.Description("Replace project name. 1..200 chars after trim."),
		),
		mcpgo.WithString("description",
			mcpgo.Description("Replace description. Pass an empty string to clear."),
		),
		mcpgo.WithString("gitlabGroup",
			mcpgo.Description("Replace the optional GitLab group binding. Pass an empty string to clear."),
		),
		mcpgo.WithString("visibility",
			mcpgo.Description("Switch visibility: \"space\" — every member of the space sees the project; \"members\" — only its members and the space owner."),
			mcpgo.Enum("space", "members"),
		),
		mcpgo.WithString("space",
			mcpgo.Description("Not supported: a project cannot move to another space. Any value but null is refused with space_change_forbidden."),
		),
		mcpgo.WithBoolean("archived",
			mcpgo.Description("true = archive the project (no-op if already archived); false = unarchive (no-op if active)."),
		),
		mcpgo.WithArray("aliasesAdd",
			mcpgo.Description("Aliases to add. Each is unique across projects — a collision rolls back the whole patch."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("aliasesRemove",
			mcpgo.Description("Aliases to remove. An alias not present on the project rolls back the patch."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("gitReposAdd",
			mcpgo.Description("Repositories to append to gitRepos[]. Each entry: {name, url, defaultBranch}. Server generates the id."),
			mcpgo.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Human-readable repo name within the project.",
					},
					"url": map[string]any{
						"type":        "string",
						"description": "Git remote URL (https/ssh/git).",
					},
					"defaultBranch": map[string]any{
						"type":        "string",
						"description": "Default branch (e.g. \"main\"). Optional.",
					},
				},
				"required": []string{"name", "url"},
			}),
		),
		mcpgo.WithArray("gitReposRemove",
			mcpgo.Description("Repository UUIDs to remove from gitRepos[]. A repo still referenced by a non-archived component rolls back the patch."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleProjectUpdate(pool, projectsStore))
}

// projectUpdateResponse is the wire shape — the updated ProjectDTO plus a
// concise summary of what materialised so the agent can surface it without
// diffing client-side.
type projectUpdateResponse struct {
	Project        *projects.ProjectDTO `json:"project"`
	AliasesAdded   []string             `json:"aliasesAdded"`
	AliasesRemoved []string             `json:"aliasesRemoved"`
	ReposAdded     []projects.Repo      `json:"reposAdded"`
	ReposRemoved   []string             `json:"reposRemoved"`
	ArchivedFlip   *bool                `json:"archivedFlip,omitempty"`
}

func handleProjectUpdate(pool *pgxpool.Pool, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		callerID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		// ---- Resolve project ----
		projectArg, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, projectArg, callerID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		// ---- Permission gate: the project owner or the space owner (US-6) ----
		if _, oerr := projects.EnsureProjectOwner(ctx, pool, projectID, callerID); oerr != nil {
			if errors.Is(oerr, projects.ErrProjectNotVisible) {
				return projectErrorResult(&ProjectNotFoundError{Query: projectArg})
			}
			if errors.Is(oerr, projects.ErrProjectNotOwner) {
				return skmcp.ToolError(ErrCodeForbidden,
					"only the project owner, the space owner or the instance admin can update a project",
					map[string]any{"project": projectArg}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_project_update",
				"reason", "owner check failed",
				"userId", callerID.String(), "projectId", projectID.String(), "error", oerr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "permission check failed", nil), nil
		}

		// ---- Build patch args. Distinguish "absent" from "empty string". ----
		argsMap := req.GetArguments()
		patch := projects.PatchProjectArgs{}

		// sk-674.18 E-31: a project never changes its space — refused before
		// anything is changed.
		// null names no space: only a value is the move.
		if v, ok := argsMap["space"]; ok && v != nil {
			return skmcp.ToolError(ErrCodeSpaceChangeForbidden,
				"a project cannot be moved to another space", nil), nil
		}
		if _, ok := argsMap["visibility"]; ok {
			v := strings.TrimSpace(req.GetString("visibility", ""))
			if v != spaces.VisibilitySpace && v != spaces.VisibilityMembers {
				return skmcp.ToolError(ErrCodeValidationError, "visibility must be space or members",
					map[string]any{"field": "visibility"}), nil
			}
			patch.Visibility = &v
		}

		if _, ok := argsMap["name"]; ok {
			rawName := req.GetString("name", "")
			canonical, vErr := projects.ValidateProjectName(rawName)
			if vErr != nil {
				return projectsValidationToolError(vErr), nil
			}
			patch.Name = &canonical
		}
		if _, ok := argsMap["description"]; ok {
			v := req.GetString("description", "")
			if vErr := projects.ValidateProse("description", v); vErr != nil {
				return projectsValidationToolError(vErr), nil
			}
			patch.Description = &v
		}
		if _, ok := argsMap["gitlabGroup"]; ok {
			v := strings.TrimSpace(req.GetString("gitlabGroup", ""))
			patch.GitlabGroup = &v
		}
		if v, ok := argsMap["archived"].(bool); ok {
			b := v
			patch.Archived = &b
		}

		// ---- aliasesAdd / aliasesRemove ----
		if rawAliases, ok := argsMap["aliasesAdd"].([]any); ok {
			patch.AliasesAdd = make([]string, 0, len(rawAliases))
			for i, raw := range rawAliases {
				str, ok := raw.(string)
				if !ok {
					return skmcp.ToolError(ErrCodeValidationError,
						"aliasesAdd entries must be strings",
						map[string]any{"index": i}), nil
				}
				canon, vErr := projects.ValidateAlias(str)
				if vErr != nil {
					return projectsValidationToolError(vErr), nil
				}
				patch.AliasesAdd = append(patch.AliasesAdd, canon)
			}
		}
		if rawAliases, ok := argsMap["aliasesRemove"].([]any); ok {
			patch.AliasesRemove = make([]string, 0, len(rawAliases))
			for i, raw := range rawAliases {
				str, ok := raw.(string)
				if !ok {
					return skmcp.ToolError(ErrCodeValidationError,
						"aliasesRemove entries must be strings",
						map[string]any{"index": i}), nil
				}
				canon, vErr := projects.ValidateAlias(str)
				if vErr != nil {
					return projectsValidationToolError(vErr), nil
				}
				patch.AliasesRemove = append(patch.AliasesRemove, canon)
			}
		}

		// ---- gitReposAdd ----
		if rawRepos, ok := argsMap["gitReposAdd"].([]any); ok {
			patch.GitReposAdd = make([]projects.AddRepoArgs, 0, len(rawRepos))
			for i, raw := range rawRepos {
				obj, ok := raw.(map[string]any)
				if !ok {
					return skmcp.ToolError(ErrCodeValidationError,
						"gitReposAdd entries must be objects",
						map[string]any{"index": i}), nil
				}
				repoName, _ := obj["name"].(string)
				repoURL, _ := obj["url"].(string)
				repoBranch, _ := obj["defaultBranch"].(string)
				if strings.TrimSpace(repoName) == "" {
					return skmcp.ToolError(ErrCodeValidationError,
						"gitReposAdd[i].name is required",
						map[string]any{"index": i}), nil
				}
				if strings.TrimSpace(repoURL) == "" {
					return skmcp.ToolError(ErrCodeValidationError,
						"gitReposAdd[i].url is required",
						map[string]any{"index": i}), nil
				}
				patch.GitReposAdd = append(patch.GitReposAdd, projects.AddRepoArgs{
					Name:          strings.TrimSpace(repoName),
					URL:           strings.TrimSpace(repoURL),
					DefaultBranch: strings.TrimSpace(repoBranch),
				})
			}
		}

		// ---- gitReposRemove ----
		if rawIDs, ok := argsMap["gitReposRemove"].([]any); ok {
			patch.GitReposRemove = make([]uuid.UUID, 0, len(rawIDs))
			for i, raw := range rawIDs {
				str, ok := raw.(string)
				if !ok {
					return skmcp.ToolError(ErrCodeValidationError,
						"gitReposRemove entries must be UUID strings",
						map[string]any{"index": i}), nil
				}
				repoID, perr := uuid.Parse(strings.TrimSpace(str))
				if perr != nil {
					return skmcp.ToolError(ErrCodeValidationError,
						"gitReposRemove entry is not a valid UUID",
						map[string]any{"index": i, "value": str}), nil
				}
				patch.GitReposRemove = append(patch.GitReposRemove, repoID)
			}
		}

		// ---- Reject empty patch ----
		if patch.Name == nil &&
			patch.Description == nil &&
			patch.GitlabGroup == nil &&
			patch.Archived == nil &&
			len(patch.AliasesAdd) == 0 &&
			len(patch.AliasesRemove) == 0 &&
			len(patch.GitReposAdd) == 0 &&
			len(patch.GitReposRemove) == 0 &&
			patch.Visibility == nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"at least one of name, description, gitlabGroup, archived, visibility, aliasesAdd, aliasesRemove, gitReposAdd, gitReposRemove must be present",
				nil), nil
		}

		// ---- Apply patch ----
		actor := ProjectsAgentActor(ctx, pool, callerID)
		result, err := projectsStore.PatchProject(ctx, projectID, patch, callerID, role, actor)
		if err != nil {
			return mapProjectUpdateError(ctx, err, callerID, projectID), nil
		}

		// Build response.
		aliasesAdded := make([]string, 0, len(result.AliasesAdded))
		for _, a := range result.AliasesAdded {
			aliasesAdded = append(aliasesAdded, a.Alias)
		}
		reposRemoved := make([]string, 0, len(result.ReposRemoved))
		for _, rid := range result.ReposRemoved {
			reposRemoved = append(reposRemoved, rid.String())
		}
		resp := projectUpdateResponse{
			Project:        result.Project,
			AliasesAdded:   aliasesAdded,
			AliasesRemoved: result.AliasesRemoved,
			ReposAdded:     result.ReposAdded,
			ReposRemoved:   reposRemoved,
			ArchivedFlip:   result.ArchivedFlip,
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_update",
			"userId", callerID.String(),
			"projectId", projectID.String(),
			"scalarChanges", len(result.ScalarChanges),
			"aliasesAdded", len(result.AliasesAdded),
			"aliasesRemoved", len(result.AliasesRemoved),
			"reposAdded", len(result.ReposAdded),
			"reposRemoved", len(result.ReposRemoved),
			"archivedFlip", result.ArchivedFlip != nil,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(resp)
	}
}

// mapProjectUpdateError translates errors from PatchProject into MCP codes.
// Mirrors mapProjectCreateError's constraint-name matching for unique
// violations and adds typed handlers for the alias/repo collision and
// not-found cases that PatchProject surfaces.
func mapProjectUpdateError(ctx context.Context, err error, callerID, projectID uuid.UUID) *mcpgo.CallToolResult {
	if vErr, ok := projects.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, projects.ErrNotFound) {
		return skmcp.ToolError(ErrCodeProjectNotFound,
			"project does not exist or is not visible",
			map[string]any{"projectId": projectID.String()})
	}
	if errors.Is(err, spaces.ErrConcurrentChange) {
		return skmcp.ToolError(ErrCodeConcurrentChange,
			"the project was changed concurrently; nothing was changed, retry the request", nil)
	}

	var aliasTaken *projects.ErrAliasTaken
	if errors.As(err, &aliasTaken) {
		return skmcp.ToolError(ErrCodeIDTaken,
			"one of the aliases is already taken",
			map[string]any{"alias": aliasTaken.Alias})
	}
	if alias, ok := projects.IsAliasMissing(err); ok {
		return skmcp.ToolError(ErrCodeValidationError,
			"alias not present on this project",
			map[string]any{"alias": alias})
	}
	if repoID, ok := projects.IsRepoMissing(err); ok {
		return skmcp.ToolError(ErrCodeValidationError,
			"repo not found in project",
			map[string]any{"repoId": repoID.String()})
	}
	var repoInUse *projects.ErrRepoInUse
	if errors.As(err, &repoInUse) {
		return skmcp.ToolError(ErrCodeValidationError,
			"repo is still referenced by component(s); detach paths first",
			map[string]any{"components": repoInUse.Components})
	}

	var conflict *projects.ErrConflict
	if errors.As(err, &conflict) {
		switch {
		case strings.Contains(conflict.Constraint, "name_lower"):
			// sk-674.18 T025, D-4: names are unique per space — as REST.
			return skmcp.ToolError(ErrCodeProjectNameTaken,
				"a project with this name already exists in the space", nil)
		case strings.Contains(conflict.Constraint, "project_aliases"):
			return skmcp.ToolError(ErrCodeIDTaken,
				"one of the aliases is already taken",
				map[string]any{"constraint": conflict.Constraint})
		default:
			return skmcp.ToolError(ErrCodeIDTaken,
				"unique constraint violation",
				map[string]any{"constraint": conflict.Constraint})
		}
	}

	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_project_update",
		"reason", "patch failed",
		"userId", callerID.String(),
		"projectId", projectID.String(),
		"error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to update project", nil)
}

package tools

import (
	"context"
	"errors"
	"fmt"
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

// RegisterProjectCreate wires the skein_project_create tool. Wraps
// projects.Store.CreateProject and bootstraps the calling user as the
// initial owner. Any authenticated user may create a project — there is
// no role gate (see vision §3 / handler/projects.go Create).
//
// Aliases and gitRepos are optional; when supplied they're inserted in
// the same transaction as the project row. A collision in any one of
// them rolls back the whole project create — we never observe "project
// exists but its aliases are missing".
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false
//   - openWorldHint: false
func RegisterProjectCreate(s *server.MCPServer, pool *pgxpool.Pool, projectsStore *projects.Store, defaultVisibility string) {
	tool := mcpgo.NewTool(
		"skein_project_create",
		mcpgo.WithDescription(
			"Create a new project. The calling user becomes the initial "+
				"owner. `idPrefix` is the per-project token used to namespace "+
				"issue ids (e.g. \"demo\" gives ids like \"demo-42\"); when "+
				"omitted it's auto-derived from the name. `defaultIdMode` "+
				"selects the id-generation strategy for issues "+
				"(counter|hash|custom; default counter). Optional `aliases` "+
				"and `gitRepos` are inserted atomically with the project — "+
				"a collision (alias already taken, name/id_prefix already "+
				"taken) rolls back the whole create. The project goes into a "+
				"space: `space` names it (a short id from skein_whoami); "+
				"when omitted, the one space you can create projects in is used; if "+
				"there are several, the call is refused with `space_required` listing "+
				"them. A space you can see only as the instance admin is refused "+
				"with `not_space_member`: add yourself as a member first.",
		),
		mcpgo.WithString("name",
			mcpgo.Required(),
			mcpgo.Description("Display name (1..200 chars after trim)."),
		),
		mcpgo.WithString("idPrefix",
			mcpgo.Description("Per-project id prefix (lowercase, [a-z0-9][a-z0-9_-]{0,49}). Auto-derived from name when omitted."),
		),
		mcpgo.WithString("defaultIdMode",
			mcpgo.Description("Id-generation mode for issues. Default: counter."),
			mcpgo.Enum("counter", "hash", "custom"),
			mcpgo.DefaultString("counter"),
		),
		mcpgo.WithString("description",
			mcpgo.Description("Free-form project description (markdown)."),
		),
		mcpgo.WithString("space",
			mcpgo.Description("Short id of the space (skein_whoami lists your spaces). Omit when you can create projects in exactly one space."),
		),
		mcpgo.WithString("visibility",
			mcpgo.Description("Who sees the project: \"space\" — every member of the space; \"members\" — only its members and the space owner. Omitted: the installation default; always members in a personal space."),
			mcpgo.Enum("space", "members"),
		),
		mcpgo.WithArray("aliases",
			mcpgo.Description("Project aliases (each 1..64 unicode word/hyphen/underscore). Each is unique across projects — a collision rolls back the create."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("gitRepos",
			mcpgo.Description("Git repositories the project owns. Each entry has {name, url, defaultBranch}. Their ids are server-generated."),
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
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleProjectCreate(pool, projectsStore, defaultVisibility))
}

// projectCreateResponse is the wire shape — `project` carries the full
// ProjectDTO so the agent can immediately use the new id / idPrefix to
// issue follow-up calls (e.g. skein_create) without a round-trip.
type projectCreateResponse struct {
	Project *projects.ProjectDTO `json:"project"`
}

func handleProjectCreate(pool *pgxpool.Pool, projectsStore *projects.Store, defaultVisibility string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		// ---- Required: name ----
		rawName, err := req.RequireString("name")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "name is required", nil), nil
		}
		name, vErr := projects.ValidateProjectName(rawName)
		if vErr != nil {
			return projectsValidationToolError(vErr), nil
		}

		// ---- Optional: idPrefix — omitted: the first free suggestion of
		// the name (sk-674.18 T025, plan 4.4); given: the prefix rule. ----
		idPrefix := strings.TrimSpace(req.GetString("idPrefix", ""))
		if idPrefix == "" {
			sugg, err := projectsStore.SuggestPrefixes(ctx, name)
			if err != nil {
				return mapProjectCreateError(ctx, projectsStore, err, userID, name, idPrefix), nil
			}
			if len(sugg) == 0 {
				return mapProjectCreateError(ctx, projectsStore, projects.ErrPrefixTaken, userID, name, idPrefix), nil
			}
			idPrefix = sugg[0]
		} else if idPrefix, vErr = projectsStore.CheckPrefixRules(idPrefix); vErr != nil {
			return projectsValidationToolError(vErr), nil
		}

		// ---- Optional: defaultIdMode (default counter) ----
		mode := strings.TrimSpace(req.GetString("defaultIdMode", "counter"))
		if mode == "" {
			mode = "counter"
		}
		if _, vErr := projects.ValidateIDMode(mode); vErr != nil {
			return projectsValidationToolError(vErr), nil
		}

		description := req.GetString("description", "")
		if vErr := projects.ValidateProse("description", description); vErr != nil {
			return projectsValidationToolError(vErr), nil
		}

		// ---- Optional: aliases ----
		// Validate every alias up front so a collision on the third entry
		// doesn't leave us with the first two persisted.
		rawAliases := req.GetStringSlice("aliases", nil)
		aliases := make([]string, 0, len(rawAliases))
		for _, raw := range rawAliases {
			canon, vErr := projects.ValidateAlias(raw)
			if vErr != nil {
				return projectsValidationToolError(vErr), nil
			}
			aliases = append(aliases, canon)
		}

		// ---- Optional: gitRepos ----
		// Each repo enters CreateProjectArgs.GitRepos. Validate up front:
		// non-empty name + URL is the same rule the REST AddRepo handler
		// applies; we reject empty values here to keep the contracts in
		// sync.
		rawRepos, _ := req.GetArguments()["gitRepos"].([]any)
		repos := make([]projects.AddRepoArgs, 0, len(rawRepos))
		for i, raw := range rawRepos {
			obj, ok := raw.(map[string]any)
			if !ok {
				return skmcp.ToolError(ErrCodeValidationError,
					"gitRepos entries must be objects",
					map[string]any{"index": i}), nil
			}
			repoName, _ := obj["name"].(string)
			repoURL, _ := obj["url"].(string)
			repoBranch, _ := obj["defaultBranch"].(string)
			if strings.TrimSpace(repoName) == "" {
				return skmcp.ToolError(ErrCodeValidationError,
					"gitRepos[i].name is required",
					map[string]any{"index": i}), nil
			}
			if strings.TrimSpace(repoURL) == "" {
				return skmcp.ToolError(ErrCodeValidationError,
					"gitRepos[i].url is required",
					map[string]any{"index": i}), nil
			}
			repos = append(repos, projects.AddRepoArgs{
				Name:          strings.TrimSpace(repoName),
				URL:           strings.TrimSpace(repoURL),
				DefaultBranch: strings.TrimSpace(repoBranch),
			})
		}

		// ---- Space and visibility (sk-674.18 T024, US-12) ----
		visibility := strings.TrimSpace(req.GetString("visibility", ""))
		if visibility != "" && visibility != spaces.VisibilitySpace && visibility != spaces.VisibilityMembers {
			return skmcp.ToolError(ErrCodeValidationError, "visibility must be space or members",
				map[string]any{"field": "visibility"}), nil
		}
		space, err := spaces.ChooseForProject(ctx, pool, userID, strings.TrimSpace(req.GetString("space", "")))
		if err != nil {
			if res, ok := spacesToolError(err); ok {
				return res, nil
			}
			return mapProjectCreateError(ctx, projectsStore, err, userID, name, idPrefix), nil
		}

		// ---- Create ----
		// Thread the agent actor (Role='agent', SessionID populated from
		// auth ctx) all the way into CreateProjectArgs.Actor so the
		// project.created event is written with actor_type='agent' and a
		// non-NULL actor_session_id. Without this, the store falls back
		// to HumanActor and the audit row mis-attributes every MCP-driven
		// project create as a human action. CreatorName remains the
		// resolved username for backward compat with anything that reads
		// args.CreatorName (none currently, but the field is kept aligned
		// with the actor's Username for future-proofing).
		actor := ProjectsAgentActor(ctx, pool, userID)
		dto, err := projectsStore.CreateProject(ctx, projects.CreateProjectArgs{
			Name:             name,
			IDPrefix:         idPrefix,
			Description:      description,
			DefaultIDMode:    mode,
			GitlabGroup:      nil,
			CreatorID:        userID,
			CreatorRole:      role,
			CreatorName:      actor.Username,
			Actor:            actor,
			Aliases:          aliases,
			AliasAddedByKind: "agent",
			GitRepos:         repos,
			SpaceID:          space.ID,
			Visibility:       spaces.ProjectVisibility(space.Kind, visibility, defaultVisibility),
		})
		if errors.Is(err, projects.ErrNotSpaceMember) {
			// The creator left the chosen space before the creating
			// transaction took its lock: the body REST answers, the space
			// named.
			return skmcp.ToolError(ErrCodeNotSpaceMember,
				"add yourself as a member of the space to create projects there",
				map[string]any{"spaceId": space.ShortID}), nil
		}
		if err != nil {
			return mapProjectCreateError(ctx, projectsStore, err, userID, name, idPrefix), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_project_create",
			"userId", userID.String(), "projectId", dto.ID.String(),
			"idPrefix", dto.IDPrefix, "aliasCount", len(aliases),
			"repoCount", len(repos),
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(projectCreateResponse{Project: dto})
	}
}

// mapProjectCreateError translates the typed errors returned by
// projects.Store.CreateProject into MCP error codes. Constraint name
// matching mirrors handler.mapConflict so the agent and REST clients see
// the same code on collisions.
func mapProjectCreateError(ctx context.Context, store *projects.Store, err error, userID uuid.UUID, name, idPrefix string) *mcpgo.CallToolResult {
	if vErr, ok := projects.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var quota *projects.ErrProjectQuota
	if errors.As(err, &quota) {
		// sk-674.18 T026, SC-012: the code and numbers REST answers.
		return skmcp.ToolError(ErrCodeProjectQuota,
			fmt.Sprintf("the space has reached its project limit: %d of %d", quota.ProjectCount, quota.ProjectQuota),
			map[string]any{"spaceId": quota.SpaceID, "projectCount": quota.ProjectCount, "projectQuota": quota.ProjectQuota})
	}
	if errors.Is(err, projects.ErrPrefixTaken) {
		// sk-674.18 T025, US-15, E-17: neutral — one code, 2–3 free
		// alternatives, never the holder, its space or the constraint.
		alts, aerr := store.PrefixAlternatives(ctx, name)
		if aerr != nil || alts == nil {
			alts = []string{}
		}
		return skmcp.ToolError(ErrCodePrefixTaken, "this prefix is taken",
			map[string]any{"suggestions": alts})
	}
	if errors.Is(err, projects.ErrExternalCannotCreateProject) {
		// sk-163 S3 — external caller would become owner of the new project.
		// sk-674.18: the code of the spaces registry, the same as REST.
		return skmcp.ToolError(ErrCodeExternalCannotCreateProject,
			"external users cannot create projects", nil)
	}
	if errors.Is(err, spaces.ErrSpaceNotFound) {
		return skmcp.ToolError(ErrCodeSpaceNotFound, "space not found", nil)
	}
	if errors.Is(err, projects.ErrNotSpaceMember) {
		return skmcp.ToolError(ErrCodeNotSpaceMember,
			"add yourself as a member of the space to create projects there", nil)
	}
	if errors.Is(err, spaces.ErrConcurrentChange) {
		return skmcp.ToolError(ErrCodeConcurrentChange,
			"the space was changed concurrently; nothing was changed, retry the request", nil)
	}
	var conflict *projects.ErrConflict
	if errors.As(err, &conflict) {
		switch {
		case strings.Contains(conflict.Constraint, "name_lower"):
			// sk-674.18 T025, D-4: names are unique per space — the same
			// code as REST.
			return skmcp.ToolError(ErrCodeProjectNameTaken,
				"a project with this name already exists in the space",
				map[string]any{"name": name})
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
		"tool", "skein_project_create",
		"reason", "create failed",
		"userId", userID.String(),
		"name", name, "idPrefix", idPrefix, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to create project", nil)
}

// projectsValidationToolError adapts projects.ValidationError into the
// MCP tool error envelope. Mirrors validationToolError (issues package)
// but operates on projects.ValidationError.
func projectsValidationToolError(err error) *mcpgo.CallToolResult {
	if ve, ok := projects.IsValidationError(err); ok && ve.Code == projects.CodePrefixInvalid {
		// sk-674.18 T025: the code of the spaces registry, as in REST.
		return skmcp.ToolError(ErrCodePrefixInvalid, ve.Message, nil)
	}
	if ve, ok := projects.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, ve.Message,
			map[string]any{"code": ve.Code})
	}
	return skmcp.ToolError(ErrCodeValidationError, err.Error(), nil)
}

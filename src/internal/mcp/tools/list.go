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

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterList wires the skein_list tool. Paginated list of issues for
// one project, with the same filter set as GET /projects/{id}/issues
// minus the BFF-specific bits.
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
//   - anthropic/maxResultSizeChars: 200000 — NOT lowered by sk-282 T026,
//     against the expectation the plan's registry recorded, and the reason
//     is a measurement rather than an argument.
//
//     The row got much smaller (SC-002: ~615 → ~300 characters of service
//     part), but the row is not what bounds the page: the TITLE is, and a
//     title is legal up to 1000 characters and is deliberately never
//     truncated. Serialised on a full legal page — 100 rows, 1000-character
//     titles, ten labels each — the answer is 158 742 bytes project-scoped
//     and 163 842 cross-project (measured 2026-08-20, throwaway harness on
//     the integration database). That is 82% of this declaration already,
//     and the label count per row has no ceiling at all, so the true bound
//     is open-ended above it.
//
//     Lowering the declaration would therefore not "fit it to the new row";
//     it would push ordinary large pages over the client threshold, where
//     the result is persisted to disk and replaced by a file reference
//     (result_size.go). The saving this run actually delivers on this tool
//     is SC-003 — the DEFAULT page fell from up to 500 rows to 25.
func RegisterList(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store, projectsStore *projects.Store, embedder issues.QueryEmbedder) {
	tool := mcpgo.NewTool(
		"skein_list",
		mcpgo.WithDescription(
			"Paginated list of issues. BY DEFAULT a page is "+strconv.Itoa(ListLimitDefault)+
				" rows (ceiling "+strconv.Itoa(ListLimitMax)+"), and each row is the "+
				"named lean set — "+IssueRowDefaultFieldsSentence(false)+
				" — NOT the "+
				"issue card: no description, design, acceptance or notes. "+
				"`projectId` joins that set in cross-project mode only. Named "+
				"means named: keys outside the set are ABSENT, `componentIds`, "+
				"`createdBy`, `createdAt`, `acceptedAt`, `pinned` and "+
				"`epicProgress` among them — ask for those explicitly with "+
				"`fields` (an unknown name is REFUSED and the error names it, "+
				"so a typo cannot silently widen or empty the row); read one "+
				"issue whole with skein_show.\n\n"+
				"Two modes:\n"+
				"  - Project-scoped (default): pass `project` — paginated list "+
				"of issues in that one project, with status / priority / "+
				"assignee / labels / parent / source / text / pinned filters.\n"+
				"  - Cross-project: pass `crossProject: true` (omit `project`) — "+
				"paginated list of every issue visible to the caller across "+
				"every project they can see. Supports an FTS `q` over title + "+
				"description + acceptance + design + notes (with optional "+
				"semantic rerank when the embedder is available), plus the "+
				"status / type / origin / assignee / createdBy / acceptedAt "+
				"filters from the project-scoped path. Returns `total` (exact "+
				"count) on every page so the caller knows the visible horizon.\n"+
				"The `cursor` field from the response is opaque; echo it back "+
				"unchanged on the next call.",
		),
		mcpgo.WithString("project",
			mcpgo.Description("Project handle: alias, name, or UUID. Required "+
				"unless `crossProject` is true."),
		),
		mcpgo.WithBoolean("crossProject",
			mcpgo.Description("When true, list issues across every project the "+
				"caller can see (visibility-filtered to their project_members "+
				"horizon; admins see all but other users' personal spaces). `project` MUST be omitted in this "+
				"mode. Powers the \"All projects\" view in the web UI."),
		),
		mcpgo.WithString("space",
			mcpgo.Description("Space short id (cross-project mode only): only "+
				"issues of that space's projects (the space of the issue's "+
				"project, not of its id prefix). An unknown or unseen space "+
				"yields nothing. The row's space is the selectable field "+
				"`spaceId`."),
		),
		mcpgo.WithString("q",
			mcpgo.Description("Full-text search query (cross-project mode only). "+
				"Matches against title + description + acceptance_criteria + "+
				"design + notes via tsvector (ru + en). Exact issue id matches "+
				"(e.g. \"demo-2.5\") are boosted to the top."),
		),
		mcpgo.WithString("type",
			mcpgo.Description("Single type filter (cross-project mode only). "+
				"One of bug | feature | task | epic | chore | decision | idea | note."),
			mcpgo.Enum("bug", "feature", "task", "epic", "chore", "decision", "idea", "note"),
		),
		mcpgo.WithArray("statuses",
			mcpgo.Description("Filter by status; any of open|in_progress|deferred|closed|standing. "+
				"`standing` selects perpetual obligations (excluded from the default "+
				"active-work counts/lenses). "+
				"`blocked` is no longer a writable status — use the `isBlocked` filter "+
				"to narrow to issues derived as blocked from the dep graph. "+
				"Project-scoped mode accepts multiple values (OR). Cross-project mode "+
				"accepts at most one entry — e.g. statuses:[\"open\"] only; pass a single "+
				"status or omit for all."),
			mcpgo.WithStringEnumItems([]string{"open", "in_progress", "deferred", "closed", "standing"}),
		),
		mcpgo.WithString("isBlocked",
			mcpgo.Description("Filter by derived blocked-ness. "+
				"\"true\" = only issues with at least one open outgoing blocks/waits-for edge; "+
				"\"false\" = only issues with no open blockers. Omit for no filter."),
			mcpgo.Enum("true", "false"),
		),
		mcpgo.WithNumber("priority",
			mcpgo.Description("Exact priority match (0..4)."),
			mcpgo.Min(0),
			mcpgo.Max(4),
		),
		// types[] filter deferred to R2.2b — needs ListIssuesArgs.Type plumbing.
		mcpgo.WithString("assignee",
			mcpgo.Description("Assignee filter. Use the literal \"me\" to match issues "+
				"assigned to the calling user (resolved via users.id, case-insensitive "+
				"username join — same path as the Inbox view). Any other value is "+
				"treated as a free-form username equality match."),
		),
		mcpgo.WithString("createdBy",
			mcpgo.Description("Created-by filter. \"me\" selects issues the caller "+
				"created (Outbox / \"tasks I created\" view). \"not-me\" selects "+
				"issues the caller did NOT create — pairs with assignee=me to "+
				"surface tasks delegated TO the caller by someone else (the "+
				"\"On me\" lens)."),
			mcpgo.Enum("me", "not-me"),
		),
		mcpgo.WithString("acceptedAt",
			mcpgo.Description("Acceptance filter. \"null\" restricts to issues "+
				"whose assignee has not yet acknowledged the task (Inbox prefix). "+
				"\"not-null\" restricts to issues that have been accepted (the "+
				"\"On me\" / accepted-and-working prefix)."),
			mcpgo.Enum("null", "not-null"),
		),
		mcpgo.WithString("origin",
			mcpgo.Description("Origin group filter (broader than `source`). "+
				"\"human-driven\" matches source IN (human, agent_directed); "+
				"\"agent-autonomous\" matches source = agent_autonomous; "+
				"\"all\" or omitted = no filter. Use this to hide the agent "+
				"firehose in default board mode."),
			mcpgo.Enum("human-driven", "agent-autonomous", "all"),
		),
		mcpgo.WithString("parentId",
			mcpgo.Description("Exact parent id (full hierarchical form, e.g. \"demo-42\")."),
		),
		mcpgo.WithBoolean("pinned",
			mcpgo.Description("Filter on the pinned flag."),
		),
		mcpgo.WithString("source",
			mcpgo.Description("Filter by origin: human|agent_directed|agent_autonomous. "+
				"\"agent_directed\" = agent created on user instruction; "+
				"\"agent_autonomous\" = agent self-spawned mid-task."),
			mcpgo.Enum("human", "agent_directed", "agent_autonomous"),
		),
		mcpgo.WithString("text",
			mcpgo.Description("Title ILIKE filter (substring match). For semantic / FTS search use skein_search."),
		),
		mcpgo.WithArray("labels",
			mcpgo.Description("Filter by labels (any-of semantics — issues with at least one of these labels)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Page size, 1..100. Default: 25."),
			mcpgo.Min(1),
			mcpgo.Max(ListLimitMax),
		),
		mcpgo.WithArray("fields",
			mcpgo.Description("Explicit row field selection for narrow queries. Omit it to "+
				"get the default named set (identifier, title, status, type, priority, "+
				"assignee, labels, blocked flag, parent id, version, updated stamp, child "+
				"counters — plus the project id in cross-project mode). Naming fields returns "+
				"ONLY those plus the identifier, which always comes back; an empty array "+
				"therefore yields the identifier alone. An unknown name is rejected and named "+
				"— it is not ignored, because a silently missing field is indistinguishable "+
				"from an empty one."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Opaque cursor returned by a previous call's `nextCursor` field."),
		),
		mcpgo.WithString("sortBy",
			mcpgo.Description("Sort axis: \"updated\" (default) or \"created\"."),
			mcpgo.Enum("updated", "created"),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 200000,
	})

	addTool(s, tool, handleList(pool, issuesStore, projectsStore, embedder))
}

func handleList(pool *pgxpool.Pool, issuesStore *issues.Store, projectsStore *projects.Store, embedder issues.QueryEmbedder) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		// Cross-project mode short-circuit — drives the new shell's "All
		// projects" Tasks column. Project handle is forbidden in this
		// mode (visibility-filtered already); accept `q`, `type` and the
		// shared status/origin/assignee/createdBy/acceptedAt filters.
		if req.GetBool("crossProject", false) {
			if strings.TrimSpace(req.GetString("project", "")) != "" {
				return skmcp.ToolError(ErrCodeValidationError,
					"project must be omitted when crossProject=true", nil), nil
			}
			return handleListCrossProject(ctx, req, start, pool, issuesStore, embedder, userID, role)
		}
		if strings.TrimSpace(req.GetString("space", "")) != "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"space applies only when crossProject=true (a project already names its space)", nil), nil
		}

		project, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required (or pass crossProject=true)", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, project, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		args := issues.ListIssuesArgs{ProjectID: projectID, Caller: &userID}

		if statuses := req.GetStringSlice("statuses", nil); len(statuses) > 0 {
			canon := make([]string, 0, len(statuses))
			for _, s := range statuses {
				v, vErr := issues.ValidateStatus(strings.TrimSpace(s))
				if vErr != nil {
					// sk-31: route through validationToolError so the
					// `blocked_is_derived` code surfaces at the top level
					// (per the contract in create.go's helper) instead of
					// being swallowed inside a generic validation_error.
					return validationToolError(vErr), nil
				}
				canon = append(canon, v)
			}
			args.Statuses = canon
		}

		if v, ok := req.GetArguments()["priority"]; ok && v != nil {
			p := req.GetInt("priority", -1)
			if p == -1 {
				return skmcp.ToolError(ErrCodeValidationError,
					"priority must be an integer in [0, 4]", nil), nil
			}
			if vErr := issues.ValidatePriority(p); vErr != nil {
				return skmcp.ToolError(ErrCodeValidationError, vErr.Error(), nil), nil
			}
			// sk-273.1 — the store field went multi-select (priority = ANY).
			// This tool's wire contract stays single-valued: one integer in,
			// a one-element set out. Widening the MCP schema is a separate
			// decision, not a side effect of the store change.
			args.Priorities = []int{p}
		}

		if a := strings.TrimSpace(req.GetString("assignee", "")); a != "" {
			if a == "me" {
				id := userID
				args.AssigneeUserID = &id
			} else {
				args.Assignee = &a
			}
		}
		// Phase 2 ui-redesign — B5/sk-30: createdBy=me filter for Outbox,
		// createdBy=not-me for the "On me" lens. Raw UUIDs rejected — see
		// the REST handler for the visibility rationale.
		if cb := strings.TrimSpace(req.GetString("createdBy", "")); cb != "" {
			id := userID
			switch cb {
			case "me":
				args.CreatedBy = &id
			case "not-me":
				args.CreatedByExclude = &id
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"createdBy must be \"me\" or \"not-me\"", nil), nil
			}
		}
		// Phase 2 ui-redesign — B5/sk-30: acceptedAt=null for the Inbox
		// prefix, acceptedAt=not-null for the "On me" prefix.
		if aa := strings.TrimSpace(req.GetString("acceptedAt", "")); aa != "" {
			t := true
			switch aa {
			case "null":
				args.AcceptedAtNull = &t
			case "not-null":
				args.AcceptedAtNotNull = &t
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"acceptedAt accepts only the literal values \"null\" or \"not-null\"", nil), nil
			}
		}
		// Phase 2 ui-redesign — B5: origin filter group.
		if og := strings.TrimSpace(req.GetString("origin", "")); og != "" {
			switch og {
			case issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll:
				// sk-273.1 — same as priority above: the store takes a set,
				// this tool still offers one group. OriginAll in the set is
				// translated to "no filter" by OriginGroupsToSources, which
				// is what the old pointer form meant too.
				args.OriginGroups = []string{og}
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"origin must be one of human-driven, agent-autonomous, all", nil), nil
			}
		}
		if pid := strings.TrimSpace(req.GetString("parentId", "")); pid != "" {
			if !looksLikeHierarchicalID(pid) {
				return skmcp.ToolError(ErrCodeValidationError,
					"parentId must be a full hierarchical id like \"demo-42\"",
					map[string]any{"parentId": pid}), nil
			}
			args.ParentID = &pid
		}
		if v, ok := req.GetArguments()["pinned"]; ok && v != nil {
			b := req.GetBool("pinned", false)
			args.Pinned = &b
		}
		if src := strings.TrimSpace(req.GetString("source", "")); src != "" {
			canon, vErr := issues.ValidateSource(src)
			if vErr != nil {
				return skmcp.ToolError(ErrCodeValidationError, vErr.Error(), nil), nil
			}
			args.Source = &canon
		}
		if text := strings.TrimSpace(req.GetString("text", "")); text != "" {
			args.Text = &text
		}
		if labels := req.GetStringSlice("labels", nil); len(labels) > 0 {
			canon := make([]string, 0, len(labels))
			for _, l := range labels {
				v, vErr := issues.ValidateLabel(l)
				if vErr != nil {
					return skmcp.ToolError(ErrCodeValidationError,
						"labels[i]: "+vErr.Error(), nil), nil
				}
				canon = append(canon, v)
			}
			args.Labels = canon
		}

		// sk-282 T014: the page size is decided HERE, not in the store —
		// the store is shared with REST, whose page size INV-2 freezes.
		args.Limit = resolveMCPLimit(req.GetInt("limit", 0), ListLimitDefault, ListLimitMax)
		if cursor := strings.TrimSpace(req.GetString("cursor", "")); cursor != "" {
			t, id, decErr := issues.DecodeCursor(cursor)
			if decErr != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"invalid cursor: "+decErr.Error(), nil), nil
			}
			args.CursorTime = &t
			args.CursorID = &id
		}
		if sortBy := strings.TrimSpace(req.GetString("sortBy", "")); sortBy != "" {
			switch sortBy {
			case "updated", "created":
				args.SortBy = sortBy
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"sortBy must be \"updated\" or \"created\"", nil), nil
			}
		}
		// sk-31: derived blocked-ness filter (string-typed enum to match
		// the rest of the tool's param shape).
		if v := strings.TrimSpace(req.GetString("isBlocked", "")); v != "" {
			switch v {
			case "true":
				t := true
				args.IsBlocked = &t
			case "false":
				f := false
				args.IsBlocked = &f
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"isBlocked must be \"true\" or \"false\"", nil), nil
			}
		}

		res, err := issuesStore.ListIssues(ctx, args)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_list",
				"reason", "list failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list issues", nil), nil
		}

		fields, ferr := parseRowFields(req.GetArguments())
		if ferr != nil {
			return skmcp.ToolError(ErrCodeValidationError, ferr.Error(),
				map[string]any{"field": "fields"}), nil
		}
		payload, perr := projectListPage(ctx, pool, res, fields, false, authz.FromContext(ctx))
		if perr != nil {
			var unknown *UnknownIssueRowFieldError
			if errors.As(perr, &unknown) {
				return skmcp.ToolError(ErrCodeValidationError, unknown.Error(),
					map[string]any{"field": "fields", "unknownField": unknown.Field}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_list",
				"reason", "row projection failed",
				"userId", userID.String(), "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_list",
			"userId", userID.String(), "projectId", projectID.String(),
			"count", len(payload.Items), "latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(payload)
	}
}

// handleListCrossProject runs the cross-project /api/v1/issues query
// from the MCP side. Visibility is resolved here (admin-aware) and
// passed to issues.Store.ListAllIssues; the FTS+semantic rerank layer
// uses the supplied embedder (nil = FTS-only).
func handleListCrossProject(
	ctx context.Context,
	req mcpgo.CallToolRequest,
	start time.Time,
	pool *pgxpool.Pool,
	issuesStore *issues.Store,
	embedder issues.QueryEmbedder,
	userID uuid.UUID,
	role string,
) (*mcpgo.CallToolResult, error) {
	scope := authz.FromContext(ctx)
	args := issues.ListAllIssuesArgs{
		Caller:   userID,
		Scope:    scope,
		Embedder: embedder,
		// sk-674.18 T032: the page filter — one space's projects.
		Space: strings.TrimSpace(req.GetString("space", "")),
	}

	if v := strings.TrimSpace(req.GetString("q", "")); v != "" {
		args.Q = v
	}
	if statuses := req.GetStringSlice("statuses", nil); len(statuses) > 1 {
		return skmcp.ToolError(ErrCodeValidationError,
			"cross-project mode supports a single status — pass at most one entry in `statuses`", nil), nil
	} else if len(statuses) == 1 {
		canon, vErr := issues.ValidateStatus(strings.TrimSpace(statuses[0]))
		if vErr != nil {
			// sk-31: see project-scoped branch above — route through
			// validationToolError so blocked_is_derived hits the top
			// level instead of being wrapped in validation_error.
			return validationToolError(vErr), nil
		}
		args.Status = canon
	}
	if v := strings.TrimSpace(req.GetString("type", "")); v != "" {
		canon, vErr := issues.ValidateType(v)
		if vErr != nil {
			return skmcp.ToolError(ErrCodeValidationError, vErr.Error(), nil), nil
		}
		args.Type = canon
	}
	if og := strings.TrimSpace(req.GetString("origin", "")); og != "" {
		switch og {
		case issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll:
			v := og
			args.OriginGroup = &v
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"origin must be one of human-driven, agent-autonomous, all", nil), nil
		}
	}
	if a := strings.TrimSpace(req.GetString("assignee", "")); a != "" {
		if a == "me" {
			id := userID
			args.AssigneeUserID = &id
		} else {
			args.Assignee = &a
		}
	}
	if cb := strings.TrimSpace(req.GetString("createdBy", "")); cb != "" {
		id := userID
		switch cb {
		case "me":
			args.CreatedBy = &id
		case "not-me":
			args.CreatedByExclude = &id
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"createdBy must be \"me\" or \"not-me\"", nil), nil
		}
	}
	if aa := strings.TrimSpace(req.GetString("acceptedAt", "")); aa != "" {
		t := true
		switch aa {
		case "null":
			args.AcceptedAtNull = &t
		case "not-null":
			args.AcceptedAtNotNull = &t
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"acceptedAt accepts only the literal values \"null\" or \"not-null\"", nil), nil
		}
	}
	args.Limit = resolveMCPLimit(req.GetInt("limit", 0), ListLimitDefault, ListLimitMax)
	if cursor := strings.TrimSpace(req.GetString("cursor", "")); cursor != "" {
		args.Cursor = &cursor
	}
	if sortBy := strings.TrimSpace(req.GetString("sortBy", "")); sortBy != "" {
		switch sortBy {
		case "updated", "created":
			args.SortBy = sortBy
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"sortBy must be \"updated\" or \"created\"", nil), nil
		}
	}
	// sk-31: derived blocked-ness filter (cross-project mode).
	if v := strings.TrimSpace(req.GetString("isBlocked", "")); v != "" {
		switch v {
		case "true":
			t := true
			args.IsBlocked = &t
		case "false":
			f := false
			args.IsBlocked = &f
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"isBlocked must be \"true\" or \"false\"", nil), nil
		}
	}

	res, err := issuesStore.ListAllIssues(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", "skein_list",
			"reason", "list all issues failed",
			"userId", userID.String(), "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "failed to list issues", nil), nil
	}

	fields, ferr := parseRowFields(req.GetArguments())
	if ferr != nil {
		return skmcp.ToolError(ErrCodeValidationError, ferr.Error(),
			map[string]any{"field": "fields"}), nil
	}
	payload, perr := projectListPage(ctx, pool, res, fields, true, scope)
	if perr != nil {
		var unknown *UnknownIssueRowFieldError
		if errors.As(perr, &unknown) {
			return skmcp.ToolError(ErrCodeValidationError, unknown.Error(),
				map[string]any{"field": "fields", "unknownField": unknown.Field}), nil
		}
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", "skein_list",
			"reason", "row projection failed",
			"userId", userID.String(), "error", perr.Error())
		return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
	}

	slog.InfoContext(ctx, "mcp tool ok",
		"tool", "skein_list",
		"mode", "cross_project",
		"userId", userID.String(),
		"count", len(payload.Items), "total", res.Total,
		"latencyMs", time.Since(start).Milliseconds())
	return skmcp.ToolJSON(payload)
}

// projectErrorResult maps the typed errors from ResolveProjectHandle into
// the right tool-error payload. Shared across all tools that take a
// project arg.
func projectErrorResult(err error) (*mcpgo.CallToolResult, error) {
	var val *ValidationProjectError
	if errors.As(err, &val) {
		return skmcp.ToolError(ErrCodeValidationError,
			val.Message,
			map[string]any{"field": val.Field}), nil
	}
	var ambig *AmbiguousProjectError
	if errors.As(err, &ambig) {
		return skmcp.ToolError(ErrCodeAmbiguousProject,
			"project handle matched multiple projects — pick one and re-call",
			map[string]any{
				"q":          ambig.Query,
				"candidates": ambig.Candidates,
			}), nil
	}
	var notFound *ProjectNotFoundError
	if errors.As(err, &notFound) {
		return skmcp.ToolError(ErrCodeProjectNotFound,
			"project does not exist or is not visible",
			map[string]any{"q": notFound.Query}), nil
	}
	// Anything else is genuinely internal — never the agent's fault.
	return skmcp.ToolError(ErrCodeInternalError,
		"failed to resolve project handle", nil), nil
}

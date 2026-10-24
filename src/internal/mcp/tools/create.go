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

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterCreate wires the skein_create tool. Wraps issues.CreateIssue
// behind a duplicate-detection gate (dependencies.CheckDuplicates):
//
//   - If the title fuzzy-matches one or more open issues in the project
//     and `force` is not set and `duplicateOf` is not set, the tool
//     short-circuits with `created:false, duplicates:[...], decisionRequired:true`.
//     The agent reads that and decides: stop and ask the user, retry with
//     force=true, or call again with duplicateOf=<existing-id>.
//   - If `force` is true, or no duplicates were found, or `duplicateOf` is
//     set, the issue is created. When `duplicateOf` is set the tool also
//     auto-creates a `duplicates` dependency edge from the new issue to
//     the existing one, in the same transaction as the issue (sk-607): the
//     edge and the issue land together or not at all.
//
// Source is determined by the caller-supplied `kind` parameter — this
// tool is only reachable through the MCP path so the caller is always
// an agent. `kind="directed"` means the human user told the agent to
// create this issue (default agent path). `kind="autonomous"` means
// the agent decided to create this issue mid-task (side-quest /
// discovered subtask). The schema requires `kind` for agent callers
// (fail-closed); the resulting issue's source column lands as
// `agent_directed` or `agent_autonomous` accordingly. The optional
// `sourceContext` field is a free-form annotation about why the agent
// created the issue (e.g. "while implementing sk-42").
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false  — duplicate gate makes retries observable
//   - openWorldHint: false
//
// _meta:
//
//   - anthropic/maxResultSizeChars: 100000 — examined by sk-282 T026 and
//     deliberately NOT lowered, although the registry expected it might be.
//
//     The DEFAULT answer is tiny: an acknowledgement is ~140 characters
//     (SC-007 measures it), and the decision-required branch adds at most
//     five duplicate hints. Neither is what the declaration must cover.
//     `verbose:true` returns the untouched IssueDetail, and an issue's
//     prose has no ceiling — the same open-ended bound skein_show declares
//     100000 against, for the same object. Fitting the number to the
//     acknowledgement would push every verbose create over the client
//     threshold, where the answer leaves the conversation as a file
//     reference (result_size.go).
//
//     So the run's saving on this tool is real but it is not expressible as
//     a smaller ceiling: the ceiling covers the escape hatch, and the
//     escape hatch did not change.
func RegisterCreate(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store, depsStore *dependencies.Store, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_create",
		mcpgo.WithDescription(withAckAnswerNote(
			"⚠ DEPENDENCIES: use the `dependencies` array to wire typed "+
				"edges (blocks / waits-for / related / discovered-from / "+
				"supersedes / duplicates / parent-child) to existing "+
				"issues atomically with the create. Do NOT write \"depends "+
				"on X\" / \"blocks Y\" / \"requires X\" in description, "+
				"acceptance or notes — prose refs are invisible to the "+
				"dependency graph, `skein_ready` gating, the UI "+
				"Dependencies section and close-time warnings. For a "+
				"hierarchical child prefer the `parentId` parameter on "+
				"this same call; for edges on an EXISTING issue use "+
				"`skein_dep_add`.\n\n"+
				"Create a new issue. Two-layer duplicate detection "+
				"(pg_trgm title similarity + semantic vector rerank) runs "+
				"natively — do NOT preflight with skein_search or "+
				"skein_prime to 'check for duplicates'. On a match returns "+
				"`{created:false, duplicates:[...], decisionRequired:true}`: "+
				"stop and ask the user, or retry with `force:true`, or call "+
				"again with `duplicateOf:\"demo-42\"` (links a `duplicates` "+
				"edge). Layers, thresholds and the three-way rubric: "+
				"skein_get_instructions (\"Duplicate detection\").\n\n"+
				"`kind` (REQUIRED for agent callers) sets the issue's "+
				"`source` column — see the parameter. The optional "+
				"`sourceContext` annotates the trigger further. `parentId` "+
				"switches id-generation to hierarchical (parent.N) "+
				"regardless of project default. `id` is honoured only when "+
				"`idMode=custom`.\n\n"+
				"Inline `dependencies[]` (max 50) are created in the "+
				"SAME SERIALIZABLE tx as the issue insert — any failed "+
				"edge (cycle, target invisible, target missing, "+
				"duplicate edge) rolls back the entire create and "+
				"returns a validation_error / dependency_* code naming "+
				"the failing index. Cross-project edges are allowed; "+
				"the caller must already see both projects. The new "+
				"issue is on the FROM side of each edge.\n\n"+
				assigneeDefaultNote+"\n\n"+
				fieldBleedNote,
		)),
		mcpgo.WithString("project",
			mcpgo.Required(),
			mcpgo.Description("Project handle: alias, name, or UUID."),
		),
		mcpgo.WithString("title",
			mcpgo.Required(),
			mcpgo.Description("Short imperative title (1..1000 chars)."),
		),
		mcpgo.WithString("type",
			mcpgo.Required(),
			mcpgo.Description("Issue type."),
			mcpgo.Enum("bug", "feature", "task", "epic", "chore", "decision", "idea", "note"),
		),
		mcpgo.WithNumber("priority",
			mcpgo.Description("Priority 0..4 (0=highest). Default: 3."),
			mcpgo.Min(0),
			mcpgo.Max(4),
			mcpgo.DefaultNumber(3),
		),
		mcpgo.WithString("description",
			mcpgo.Description("Long-form description (markdown)."),
		),
		mcpgo.WithString("design",
			mcpgo.Description("Design notes / approach."),
		),
		mcpgo.WithString("acceptanceCriteria",
			mcpgo.Description("Acceptance criteria (markdown)."),
		),
		mcpgo.WithString("notes",
			mcpgo.Description("Free-form notes."),
		),
		mcpgo.WithString("parentId",
			mcpgo.Description("Full hierarchical id of the parent issue (e.g. \"demo-42\"). Switches id-generation to hierarchical."),
		),
		mcpgo.WithArray("componentIds",
			mcpgo.Description("Component UUIDs to attach. Must belong to the same project."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("labels",
			mcpgo.Description("Free-form labels to attach."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithString("assignee",
			mcpgo.Description(assigneeDefaultParamDescription),
		),
		mcpgo.WithBoolean("unassigned",
			mcpgo.Description(unassignedParamDescription),
		),
		mcpgo.WithString("sourceContext",
			mcpgo.Description("Free-form annotation describing why the agent created this issue (e.g. \"while implementing sk-42\")."),
		),
		mcpgo.WithBoolean("standing",
			mcpgo.Description("Create the issue as a standing task instead of the "+
				"default `open`. "+standingConceptNote+" Standing under "+
				"standing is rejected with `nested_standing_forbidden`. To "+
				"promote an existing task instead, use "+
				"skein_update(status=standing). "+standingGuideRef+
				" Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithBoolean("reviewRequired",
			mcpgo.Description("Whether the issue's author will want to review the "+
				"result once it is closed. Private bookkeeping of the AUTHOR: it is "+
				"never returned to anyone else and never shows up in any feed. "+
				"Omit it and your personal default applies (built-in default: true) "+
				"— the same default every other creation path uses. Pass false to "+
				"opt this one issue out."),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass the duplicate gate. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithString("duplicateOf",
			mcpgo.Description("Full hierarchical id of an existing issue this should be flagged as a duplicate of. Skips the duplicate gate AND auto-creates a `duplicates` dependency edge from the new issue to the supplied one, in the same transaction — if the edge cannot be written, nothing is created."),
		),
		mcpgo.WithString("idMode",
			mcpgo.Description("Override the project's default id-generation mode."),
			mcpgo.Enum("counter", "hash", "custom"),
		),
		mcpgo.WithString("id",
			mcpgo.Description("Explicit id (custom mode only). Must be \"<prefix>-<suffix>\" matching the project's id_prefix."),
		),
		// `kind` is REQUIRED for agent callers — enforced in the handler
		// rather than at the schema level so the human-via-future-BFF
		// path can pass through without a hard schema break. Web UI
		// callers (cookie-auth) hit the REST endpoint directly, where
		// source is server-forced to "human" regardless of input.
		mcpgo.WithString("kind",
			mcpgo.Description("Issue origin classification (REQUIRED for agent "+
				"callers) — sets the stored `source` column: \"directed\" → "+
				"`source=\"agent_directed\"`, the human user told the agent to "+
				"create this issue; \"autonomous\" → `source=\"agent_autonomous\"`, "+
				"the agent decided to create it mid-task (side-quest, discovered "+
				"subtask). The web UI ignores this parameter and forces source=human; "+
				"MCP callers must specify it."),
			mcpgo.Enum("directed", "autonomous"),
		),
		mcpgo.WithArray("dependencies",
			mcpgo.Description("Typed dependency edges created atomically with this "+
				"issue. Each entry is {toId, type, gate?}; the new issue is "+
				"on the FROM side of every edge, and direction is canonical "+
				"per type. Max "+inlineDepMaxStr+" per call, same type/gate "+
				"semantics as skein_dep_add — any failed edge rolls back the "+
				"entire create. For hierarchy prefer the top-level `parentId` "+
				"(picks the next `parent.N` id); to make an EXISTING issue a "+
				"child of this new one, create it first, then skein_reparent "+
				"the child. Type table: skein_get_instructions "+
				"(\"Dependency edge types\")."),
			mcpgo.MaxItems(inlineDepMaxItems),
			mcpgo.Items(map[string]any{
				"type":     "object",
				"required": []string{"toId", "type"},
				"properties": map[string]any{
					"toId": map[string]any{
						"type":        "string",
						"description": "Full hierarchical id of the TO-side issue (e.g. \"billing-9\"). May be in a different project.",
					},
					"type": map[string]any{
						"type": "string",
						"enum": []string{
							dependencies.DepBlocks,
							dependencies.DepRelated,
							dependencies.DepDiscoveredFrom,
							dependencies.DepSupersedes,
							dependencies.DepDuplicates,
							dependencies.DepWaitsFor,
							dependencies.DepParentChild,
						},
						// Short on purpose: the enum lists the legal values and
						// the array-level description above already points at
						// the guide's type table. Repeating depTypeParamDescription
						// here would add ~330 chars for no new information.
						"description": "Dependency type. parent-child is the canonical hierarchy edge.",
					},
					"gate": map[string]any{
						"type": "string",
						"enum": []string{
							dependencies.GateAllChildren,
							dependencies.GateAnyChildren,
						},
						"description": depGateParamDescription,
					},
				},
			}),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription+cardUnavailableNote),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 100000,
	})

	addTool(s, tool, handleCreate(pool, issuesStore, depsStore, projectsStore))
}

// inlineDepMaxItems caps how many edges a single skein_create call may
// inline via the `dependencies` array. Tighter than batchMaxItems (100)
// for skein_dep_add_many — inline-deps live inside a single SERIALIZABLE
// issue-create tx, so each extra edge widens the tx footprint and the
// blast radius of a rollback. 50 covers the realistic ceiling (epics
// with a sprawling blocker fan-in) without inviting tx-bloat patterns.
const inlineDepMaxItems = 50

// inlineDepMaxStr is the string form of inlineDepMaxItems, kept as a
// const so it can be interpolated into the WithDescription string above
// without an fmt.Sprintf call at every package init.
const inlineDepMaxStr = "50"

// createResponse is the wire shape returned by skein_create. Three
// outcomes are encoded in one shape so the agent has a single contract:
//
//	created=true,  issue=<acknowledgement>, duplicates=[],     decisionRequired=false
//	created=false, issue=null,              duplicates=[...],  decisionRequired=true
//	created=true,  issue=<acknowledgement>, duplicates=[],     decisionRequired=false (force or duplicateOf)
//
// `duplicates` is always an array (possibly empty) so the JSON consumer
// doesn't have to special-case absence vs. emptiness.
//
// `degraded` reports the duplicate check the same way skein_search
// reports its own retrieval (sk-228): true means the semantic layer
// could not be consulted, so the verdict — including an EMPTY
// `duplicates` — rests on pg_trgm alone, or on nothing at all when the
// title was not legible to pg_trgm either. The reason is logged rather
// than returned — SC-007 caps this payload and the boolean is what a
// caller branches on. It is false when the gate was skipped entirely
// (force / duplicateOf): nothing was checked, so there is no degradation
// to report. Without this field `created:true, duplicates:[]` reads as
// "checked, clean" whether or not anything actually compared the title —
// which is precisely the confusion that let a fabricated `similarity: 1`
// go unnoticed.
//
// `warnings` carries non-blocking informational notes for the agent
// that don't map cleanly onto the typed fields above. Today the only
// producer is the parent-component inheritance path (when the caller
// omits componentIds on a child create, the store copies the parent's
// set and a human-readable warning lands here so the agent surfaces it
// in chat instead of silently inheriting). Always non-nil — empty
// array on every other outcome so the JSON consumer doesn't have to
// special-case absence.
//
// `issue` is typed `any` since sk-233. Since sk-282 T022 it carries the
// ACKNOWLEDGEMENT (id, ok, version and the post-state stamps that apply)
// by default, and the untouched IssueDetail when the caller passes
// verbose=true. It stays `null` on the decision-required outcome, same
// as before — nothing was created, so there is no post-state to confirm.
//
// The version inside it is not optional decoration: INV-7 requires the
// caller to be able to issue its next conditional update off this
// response alone, and a create is the most common start of that cycle.
type createResponse struct {
	Created          bool                              `json:"created"`
	Issue            any                               `json:"issue"`
	Duplicates       []dependencies.DuplicateCandidate `json:"duplicates"`
	DecisionRequired bool                              `json:"decisionRequired"`
	Degraded         bool                              `json:"degraded"`
	Warnings         []string                          `json:"warnings"`
	// CardUnavailable — sk-607: present (true) only on a verbose call whose
	// issue was created but whose card could not be read back; `issue` is
	// then the acknowledgement. See cardUnavailableKey.
	CardUnavailable bool `json:"cardUnavailable,omitempty"`
}

func handleCreate(pool *pgxpool.Pool, issuesStore *issues.Store, depsStore *dependencies.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		// ---- Resolve project ----
		projectArg, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, projectArg, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}

		// ---- Required inputs ----
		rawTitle, err := req.RequireString("title")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "title is required", nil), nil
		}
		title, vErr := issues.ValidateTitle(rawTitle)
		if vErr != nil {
			return validationToolError(vErr), nil
		}

		rawType, err := req.RequireString("type")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "type is required", nil), nil
		}
		itype, vErr := issues.ValidateType(rawType)
		if vErr != nil {
			return validationToolError(vErr), nil
		}

		// Priority default 3 (per spec — vision-level "low default" rather
		// than the REST default of 2). Clamp + validate per the [0,4] rule.
		priority := req.GetInt("priority", 3)
		if vErr := issues.ValidatePriority(priority); vErr != nil {
			return validationToolError(vErr), nil
		}

		// ---- Optional inputs ----
		description := req.GetString("description", "")
		design := req.GetString("design", "")
		acceptance := req.GetString("acceptanceCriteria", "")
		notes := req.GetString("notes", "")

		// Guard against tool-call wrapper literals leaking into free-text
		// fields (sk-28). Title is checked inside ValidateTitle above.
		for _, pair := range [...]struct{ field, val string }{
			{"description", description},
			{"design", design},
			{"acceptanceCriteria", acceptance},
			{"notes", notes},
		} {
			if vErr := issues.ValidateProse(pair.field, pair.val); vErr != nil {
				return validationToolError(vErr), nil
			}
		}

		var parentIDArg *string
		if pid := strings.TrimSpace(req.GetString("parentId", "")); pid != "" {
			if !looksLikeHierarchicalID(pid) {
				return skmcp.ToolError(ErrCodeValidationError,
					"parentId must be a full hierarchical id like \"demo-42\"",
					map[string]any{"parentId": pid}), nil
			}
			parentIDArg = &pid
		}

		// Component IDs — caller passes UUID strings; we parse them up
		// front so a malformed entry returns validation_error rather than
		// failing inside the store.
		rawComponentIDs := req.GetStringSlice("componentIds", nil)
		componentIDs := make([]uuid.UUID, 0, len(rawComponentIDs))
		for _, raw := range rawComponentIDs {
			id, perr := uuid.Parse(strings.TrimSpace(raw))
			if perr != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"componentIds[i] is not a UUID",
					map[string]any{"componentId": raw}), nil
			}
			componentIDs = append(componentIDs, id)
		}
		// componentIdsExplicit signals the parent-inheritance gate at
		// the store level: when the caller supplied the `componentIds`
		// key (even as an empty array) we honour their value as-is and
		// skip inheritance; when the key is absent we let the store
		// inherit from the parent (if one is set, same-project, non-
		// empty). The mcp-go arg map only contains keys the caller
		// actually sent, so a presence check is the right signal.
		_, componentIDsExplicit := req.GetArguments()["componentIds"]

		rawLabels := req.GetStringSlice("labels", nil)
		labels := make([]string, 0, len(rawLabels))
		for _, l := range rawLabels {
			canon, vErr := issues.ValidateLabel(l)
			if vErr != nil {
				return validationToolError(vErr), nil
			}
			labels = append(labels, canon)
		}

		// sk-605: a non-string assignee is refused here, before the
		// duplicate gate and any write.
		assigneeArg, assigneeErr := assigneeFromArgs(req.GetArguments(), "", nil)
		if assigneeErr != nil {
			return assigneeErr, nil
		}

		// sk-565 (P9, US-4): `unassigned` is parsed from the raw arguments
		// right after `assignee` — before inline edges, duplicateOf and the
		// duplicate gate — so its refusals leave nothing behind and win over
		// decisionRequired (INV-9).
		assignByDefault, modeErr := assignCreatorByDefault(req.GetArguments(), assigneeArg, "", nil)
		if modeErr != nil {
			return modeErr, nil
		}

		// sk-674.18 T066: creating an issue is ActionWrite of US-6; naming
		// an assignee is ActionAssign too. Before inline edges, duplicateOf
		// and the duplicate gate — a refused caller learns nothing more.
		createActions := []authz.Action{authz.ActionWrite}
		if assigneeArg != nil {
			createActions = append(createActions, authz.ActionAssign)
		}
		if res := projectActionGate(ctx, pool, "skein_create", projectArg, projectID, userID, createActions...); res != nil {
			return res, nil
		}

		// ---- Inline dependencies (typed-edge wiring atomic with create) ----
		// Parsed up-front so a malformed entry short-circuits BEFORE the
		// duplicate-check round-trip and the issue insert. The store
		// re-validates type/gate inside the tx; we do shape + cap
		// validation here so the error message can name the failing index
		// before any DB work has happened.
		inlineDeps, depParseErr := parseInlineDeps(req.GetArguments())
		if depParseErr != nil {
			return depParseErr, nil
		}

		var sourceContextArg *string
		if sc := strings.TrimSpace(req.GetString("sourceContext", "")); sc != "" {
			if vErr := issues.ValidateProse("sourceContext", sc); vErr != nil {
				return validationToolError(vErr), nil
			}
			sourceContextArg = &sc
		}

		// idMode override + custom id. The default mode comes from the
		// project; we let the caller override via idMode. id is only used
		// when the resolved mode is "custom".
		project, err := projectsStore.GetProject(ctx, projectID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_create",
				"reason", "load project failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load project metadata", nil), nil
		}
		defaultIDMode := project.DefaultIDMode
		if mode := strings.TrimSpace(req.GetString("idMode", "")); mode != "" {
			switch mode {
			case "counter", "hash", "custom":
				defaultIDMode = mode
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"idMode must be one of: counter, hash, custom",
					map[string]any{"idMode": mode}), nil
			}
		}
		customID := strings.TrimSpace(req.GetString("id", ""))

		force := req.GetBool("force", false)

		// sk-118 (F2i): create-as-standing flag. When true the store inserts
		// status='standing' (no started_at, no auto-accept, no lock) and
		// enforces the nested-standing ban against the parentId. Default false
		// → the legacy open create path, byte-identical.
		standing := req.GetBool("standing", false)

		// sk-303.14 / T009: explicit pass of the "review required" flag.
		// The agent path is the only one that has it (Q3/A3); other paths
		// stay silent and get the author's personal default.
		//
		// Read from the RAW arguments, not via GetBool with a default:
		// "did not pass" and "passed false" are different intents, and a
		// default would glue them together, silently cancelling the
		// personal default on every call that does not mention the flag.
		reviewRequiredArg, reviewErr := createReviewRequiredArg(req.GetArguments())
		if reviewErr != nil {
			return reviewErr, nil
		}

		var duplicateOfID string
		if d := strings.TrimSpace(req.GetString("duplicateOf", "")); d != "" {
			if !looksLikeHierarchicalID(d) {
				return skmcp.ToolError(ErrCodeValidationError,
					"duplicateOf must be a full hierarchical id like \"demo-42\"",
					map[string]any{"duplicateOf": d}), nil
			}
			duplicateOfID = d
		}

		// ---- Pre-check duplicateOf existence + visibility ----
		// The `duplicates` edge is written as an inline dependency inside
		// CreateIssue's transaction (sk-607), so an edge that fails rolls
		// the whole create back. This pre-check still answers the common
		// cases before any transaction opens, with duplicateOf's own
		// codes. A visibility miss renders identically to not-found per
		// the project visibility pattern (never leak forbidden vs
		// not-found across project boundaries). After this check, an edge
		// failure inside the transaction is a genuine race (target deleted
		// or hidden between check and create); mapDuplicateOfEdgeError
		// renders it, and nothing was created.
		if duplicateOfID != "" {
			target, terr := issuesStore.GetIssue(ctx, duplicateOfID)
			if terr != nil {
				if errors.Is(terr, issues.ErrNotFound) {
					return skmcp.ToolError(ErrCodeValidationError,
						"duplicateOf target issue does not exist",
						map[string]any{"duplicateOf": duplicateOfID}), nil
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_create",
					"reason", "duplicateOf precheck load failed",
					"userId", userID.String(), "duplicateOf", duplicateOfID, "error", terr.Error())
				return skmcp.ToolError(ErrCodeInternalError,
					"failed to verify duplicateOf target", nil), nil
			}
			if _, vErr := projects.EnsureProjectVisible(ctx, pool, target.ProjectID, userID); vErr != nil {
				if errors.Is(vErr, projects.ErrProjectNotVisible) {
					return skmcp.ToolError(ErrCodeValidationError,
						"duplicateOf target issue does not exist",
						map[string]any{"duplicateOf": duplicateOfID}), nil
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_create",
					"reason", "duplicateOf precheck visibility failed",
					"userId", userID.String(), "duplicateOf", duplicateOfID, "error", vErr.Error())
				return skmcp.ToolError(ErrCodeInternalError,
					"failed to verify duplicateOf target", nil), nil
			}
		}

		// ---- Duplicate gate ----
		// Skip the gate when the caller has already declared the issue
		// as a duplicate of a known id, or has explicitly forced through.
		var duplicates []dependencies.DuplicateCandidate
		// Degradation of the duplicate check has to reach BOTH exits of
		// this handler (the decision-required short-circuit below and the
		// successful create at the bottom), so it is held here rather than
		// inside the gate block.
		duplicateCheckDegraded := false
		if duplicateOfID == "" && !force {
			report, err := depsStore.CheckDuplicates(ctx, projectID, title, description)
			if err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_create",
					"reason", "duplicate check failed",
					"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "duplicate check failed", nil), nil
			}
			cands := report.Candidates
			if report.Degraded {
				// Carried on BOTH outcomes below — a degraded check that
				// found nothing is the more dangerous of the two, because
				// the create succeeds and nothing else in the response
				// hints that the comparison was weakened.
				//
				// The prose reason stays OUT of the response: SC-007 pins
				// this payload under 250 characters, and on a deployment
				// with no embedder the reason would ride along on every
				// single create, forever, for no decision the boolean
				// doesn't already drive. It goes to the log (and to the
				// REST endpoint, which has no such budget); the meaning of
				// the flag is documented in skein_get_instructions.
				duplicateCheckDegraded = true
				slog.InfoContext(ctx, "duplicate check degraded",
					"tool", "skein_create",
					"projectId", projectID.String(),
					"reason", report.DegradedReason)
			}
			if len(cands) > 0 {
				slog.InfoContext(ctx, "mcp tool ok",
					"tool", "skein_create",
					"userId", userID.String(), "projectId", projectID.String(),
					"outcome", "decision_required",
					"duplicateCount", len(cands),
					"duplicateCheckDegraded", duplicateCheckDegraded,
					"latencyMs", time.Since(start).Milliseconds())
				return skmcp.ToolJSON(createResponse{
					Created:          false,
					Issue:            nil,
					Duplicates:       cands,
					DecisionRequired: true,
					Degraded:         duplicateCheckDegraded,
					Warnings:         []string{},
				})
			}
			duplicates = cands // empty slice
		}
		if duplicates == nil {
			duplicates = []dependencies.DuplicateCandidate{}
		}

		// ---- Determine effective source from caller mode + kind ----
		// Phase 1 ui-redesign: skein_create classifies the new issue's
		// origin. Caller mode is detected via api-key presence in ctx
		// (set by the MCP api-key middleware). Agent callers (api-key)
		// MUST pass `kind` and the value maps to one of agent_directed
		// / agent_autonomous. Cookie-auth callers (future BFF
		// passthrough) ignore kind and force source=human (Q-2 = a).
		// Missing or unknown kind → validation_error (Q-5 = fail-closed).
		kindArg := strings.TrimSpace(req.GetString("kind", ""))
		callerMode, ok := callerOriginMode(ctx)
		if !ok {
			// Fail-closed: neither api-key nor cookie auth populated
			// ctx. RequireUser above already validated UserID is set,
			// so this branch should be unreachable in production —
			// but if a future middleware refactor regresses, returning
			// internal_error makes the regression loud instead of
			// silently mis-tagging the issue's source.
			slog.ErrorContext(ctx, "callerOriginMode could not classify caller — neither api-key nor cookie signal present",
				"userId", userID)
			return skmcp.ToolError(ErrCodeInternalError,
				"ambiguous caller mode; cannot determine issue source",
				nil), nil
		}
		var effectiveSource string
		switch callerMode {
		case "human":
			effectiveSource = issues.SourceHuman
		case "agent":
			switch kindArg {
			case "directed":
				effectiveSource = issues.SourceAgentDirected
			case "autonomous":
				effectiveSource = issues.SourceAgentAutonomous
			case "":
				return skmcp.ToolError(ErrCodeValidationError,
					"kind is required for agent callers; pass \"directed\" "+
						"(user-requested) or \"autonomous\" (side-quest)", nil), nil
			default:
				return skmcp.ToolError(ErrCodeValidationError,
					"kind must be one of: directed, autonomous",
					map[string]any{"kind": kindArg}), nil
			}
		default:
			// callerOriginMode contract: ok=true ⇒ mode ∈ {"agent", "human"}.
			// Defensive against future enum widening that forgets to
			// extend this switch.
			slog.ErrorContext(ctx, "callerOriginMode returned unexpected mode",
				"mode", callerMode, "userId", userID)
			return skmcp.ToolError(ErrCodeInternalError,
				"unknown caller mode; cannot determine issue source",
				map[string]any{"mode": callerMode}), nil
		}

		// ---- Create the issue ----
		actor := IssuesAgentActor(ctx, pool, userID)
		input := issues.CreateIssueInput{
			ID:                   customID,
			ParentID:             parentIDArg,
			Title:                title,
			Description:          description,
			Type:                 itype,
			Priority:             priority,
			Source:               effectiveSource,
			SourceContext:        sourceContextArg,
			Assignee:             assigneeArg,
			Design:               design,
			AcceptanceCriteria:   acceptance,
			Notes:                notes,
			ComponentIDs:         componentIDs,
			ComponentIDsExplicit: componentIDsExplicit,
			Labels:               labels,
			Dependencies:         inlineDeps,
			Standing:             standing,
			ReviewRequired:       reviewRequiredArg,
			// sk-565 (P1): the creator's «assign to me» setting decides
			// unless the caller passed unassigned:true. The store applies
			// it (D2) — this layer never substitutes an assignee itself.
			AssignCreatorByDefault: assignByDefault,
		}
		// sk-607: the `duplicates` edge of duplicateOf is written in the
		// create's own transaction, as one more inline edge — not by a
		// second write after the commit, which a context ending in between
		// turned into internal_error for an issue that existed. The
		// pre-check above still runs first; a failure of this edge now
		// rolls the create back (dupEdgeIndex maps it to the duplicateOf
		// codes). An edge the caller already listed inline is not added
		// twice.
		dupEdgeIndex := -1
		if duplicateOfID != "" && !hasInlineEdge(inlineDeps, duplicateOfID, dependencies.DepDuplicates) {
			inlineDeps = append(inlineDeps, issues.DependencyEdgeInput{
				ToID: duplicateOfID,
				Type: dependencies.DepDuplicates,
			})
			dupEdgeIndex = len(inlineDeps) - 1
		}
		input.Dependencies = inlineDeps
		// DependenciesCaller is the resolved (uuid, role) the store uses
		// for visibility checks. Required when inlineDeps is non-empty
		// (each edge runs cross-project visibility) AND, since sk-35,
		// when ParentID is set (cross-project parents need the same
		// check). Set it once when either trigger fires.
		if len(inlineDeps) > 0 || (parentIDArg != nil && *parentIDArg != "") {
			input.DependenciesCaller = &issues.DependenciesCaller{
				UserID: userID,
				Role:   role,
			}
		}
		dto, err := issuesStore.CreateIssue(ctx, projectID, project.IDPrefix, defaultIDMode,
			input, userID, actor)
		verbose := req.GetBool("verbose", false)
		cardUnavailable := false
		if err != nil {
			rb, committed := asReadBackFailed(err)
			if !committed {
				if res := mapDuplicateOfEdgeError(ctx, err, dupEdgeIndex, duplicateOfID, userID); res != nil {
					return res, nil
				}
				return mapCreateError(ctx, err, userID, projectID), nil
			}
			// sk-607: the issue exists; only reading its card back failed.
			// Answering an error here is how a retry creates it twice. A
			// verbose caller gets the acknowledgement too, marked
			// cardUnavailable.
			logReadBackFallback(ctx, "skein_create", userID.String(), rb)
			dto = ackSourceFromCommitted(rb.State)
			dto.InheritedComponentIDs = rb.InheritedComponentIDs
			cardUnavailable = verbose
			verbose = false
		}

		// ---- Compose non-blocking warnings ----
		// Today the only producer is the parent-component inheritance
		// path. dto.InheritedComponentIDs is populated by the store
		// exclusively when the caller omitted componentIds AND a
		// same-project parent had components attached. We resolve the
		// uuid set to human-readable names so the agent surfaces a
		// useful chat message ("inherited foo, bar from parent —
		// override via skein_update if wrong") rather than a list of
		// opaque UUIDs.
		warnings := []string{}
		if len(dto.InheritedComponentIDs) > 0 {
			// After the commit: detached from the caller's context like the
			// read-back (sk-607), so a caller that ended now does not lose
			// the inheritance note for an issue that exists.
			nctx, cancelNames := issues.ReadBackContext(ctx)
			names, nerr := loadComponentNames(nctx, pool, dto.InheritedComponentIDs)
			cancelNames()
			if nerr != nil {
				// Non-fatal — the issue has been created, the agent
				// just won't see the inheritance note. Log so an SRE
				// can chase a persistent failure mode.
				slog.WarnContext(ctx, "mcp tool warning",
					"tool", "skein_create",
					"reason", "load inherited component names failed",
					"userId", userID.String(), "issueId", dto.ID,
					"error", nerr.Error())
			} else {
				warnings = append(warnings,
					"componentIds were inherited from the parent: "+
						strings.Join(names, ", ")+
						". If the inherited components are wrong for "+
						"this subtask, call skein_update to override.")
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_create",
			"userId", userID.String(), "projectId", projectID.String(),
			"issueId", dto.ID, "outcome", "created",
			"inheritedComponents", len(dto.InheritedComponentIDs),
			"latencyMs", time.Since(start).Milliseconds())

		// sk-282 T022 / D-6, FP-2: skein_create is one of the four sites
		// that never went through the common write-response point, and
		// this line is the whole bypass — the ONLY thing needed to close
		// it is that `issue` carries the acknowledgement rather than the
		// card. The ENVELOPE is untouched on purpose: `created`,
		// `duplicates`, `decisionRequired` and `warnings` are what US-5
		// calls "what the next step hangs on", and flattening them
		// into the acknowledgement would be a wire change nobody asked
		// for and would break the documented three-way rubric.
		issuePayloadOut, perr := ackPayload(dto, verbose)
		if perr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_create", "reason", "issue detail projection failed",
				"userId", userID.String(), "issueId", dto.ID, "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}
		return skmcp.ToolJSON(createResponse{
			Created:          true,
			Issue:            issuePayloadOut,
			Duplicates:       duplicates,
			DecisionRequired: false,
			Degraded:         duplicateCheckDegraded,
			Warnings:         warnings,
			CardUnavailable:  cardUnavailable,
		})
	}
}

// hasInlineEdge reports whether the caller's inline dependencies already
// name the edge (toID, type).
func hasInlineEdge(deps []issues.DependencyEdgeInput, toID, depType string) bool {
	for _, d := range deps {
		if d.ToID == toID && d.Type == depType {
			return true
		}
	}
	return false
}

// mapDuplicateOfEdgeError renders a failure of the duplicateOf edge, which
// rides in the create's transaction as inline edge dupEdgeIndex (sk-607),
// with the code duplicateOf always answered for a target that is gone —
// validation_error — and, since sk-674.18 T033 (INV-3), the same answer
// for one the caller cannot see. Nothing was created — the edge failure rolled the issue back —
// so, unlike the old post-commit write, the details carry no issueId. nil
// when err is not that edge's failure.
func mapDuplicateOfEdgeError(ctx context.Context, err error, dupEdgeIndex int, duplicateOfID string, userID uuid.UUID) *mcpgo.CallToolResult {
	var depFail *issues.ErrInlineDepFailed
	if dupEdgeIndex < 0 || !errors.As(err, &depFail) || depFail.Index != dupEdgeIndex {
		return nil
	}
	details := map[string]any{"duplicateOf": duplicateOfID}
	switch {
	case errors.Is(depFail.Err, dependencies.ErrTargetNotFound), errors.Is(depFail.Err, dependencies.ErrFromNotFound),
		// INV-3 (sk-674.18 T033): an invisible target answers as a nonexistent one.
		errors.Is(depFail.Err, dependencies.ErrDependencyTargetNotVisible):
		return skmcp.ToolError(ErrCodeValidationError, "duplicateOf target issue does not exist", details)
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_create",
		"reason", "duplicates edge failed; the create was rolled back",
		"userId", userID.String(), "duplicateOf", duplicateOfID, "error", depFail.Err.Error())
	return skmcp.ToolError(ErrCodeInternalError,
		"failed to add the duplicates edge; nothing was created", details)
}

// loadComponentNames batch-resolves a set of component UUIDs to their
// display names, preserving the input order. Used by skein_create to
// build the parent-component inheritance warning string. Names that
// can't be resolved (deleted between create and warning compose — a
// genuine race) fall back to the uuid string so the agent still sees
// something. One round-trip regardless of slice length.
func loadComponentNames(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id, name FROM components WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resolved := make(map[uuid.UUID]string, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		resolved[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := resolved[id]; ok {
			out = append(out, name)
		} else {
			out = append(out, id.String())
		}
	}
	return out, nil
}

// mapCreateError translates issues.Store / dependencies typed errors into
// stable tool-error codes. Mirrors the shape of mapIssueErr in the REST
// handler but emits MCP error codes instead of HTTP statuses.
func mapCreateError(ctx context.Context, err error, userID, projectID uuid.UUID) *mcpgo.CallToolResult {
	// sk-350.3: CreateIssue exhausting createMaxAttempts against a genuine
	// SERIALIZABLE abort now degrades to issues.ErrSerializationRetry (see
	// Store.CreateIssue) instead of leaking the raw pgconn.PgError through
	// to the ErrCodeInternalError default below — the same fix sk-303.17
	// already applied to skein_comment / skein_comment_update /
	// skein_comment_delete / skein_epic_create. Checked first, and NOT
	// mapped to ErrCodeVersionConflict: the issue was never created, so
	// there is no version for a re-read to quote.
	if errors.Is(err, issues.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"the create lost a serialization race and made no change — retry the call",
			map[string]any{"projectId": projectID.String()})
	}
	// sk-607: the caller's context ended inside the create's single
	// transaction — a stop, not a failure of the server.
	if code, message, ok := contextStop(ctx, ctx, err); ok {
		return skmcp.ToolError(code, message, map[string]any{"projectId": projectID.String()})
	}
	// Inline-dep failure: surface the failing index + classified dep
	// error code so the agent can patch the offending dependencies[i]
	// without re-binding the whole call.
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		return assigneeNotAllowedResult(map[string]any{"projectId": projectID.String()})
	}
	var depFail *issues.ErrInlineDepFailed
	if errors.As(err, &depFail) {
		return mapInlineDepError(ctx, depFail, userID, projectID)
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		// sk-118 (F2i / R7): the create-path nested-standing ban
		// (skein_create(parentId=<standing>, standing:true)) carries the
		// actionable `nested_standing_forbidden` code. Promote it to the
		// top-level envelope `error` — matching the inline-dep promotion in
		// mapInlineDepError and skein_dep_add — so agents branch on the
		// specific cause instead of a generic validation_error.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return skmcp.ToolError(ErrCodeNestedStandingForbidden, vErr.Message, nil)
		}
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrIDTaken) {
		return skmcp.ToolError(ErrCodeIDTaken,
			"an issue with this id already exists", nil)
	}
	var perr *issues.ErrParentNotEligible
	if errors.As(err, &perr) {
		switch perr.Code {
		case "parent_not_found":
			return skmcp.ToolError(ErrCodeParentNotFound, perr.Message, nil)
		case "parent_closed":
			return skmcp.ToolError(ErrCodeParentClosed, perr.Message, nil)
		case "parent_wrong_project":
			// sk-35: cross-project parents are allowed; this code path
			// is unreachable for the main store. Kept for back-compat
			// with any other caller of fetchParent that we might add.
			return skmcp.ToolError(ErrCodeParentWrongProject, perr.Message, nil)
		}
		return skmcp.ToolError(ErrCodeValidationError, perr.Message, nil)
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		return skmcp.ToolError(ErrCodeComponentsWrongProject,
			"some components do not belong to this project",
			map[string]any{"componentIds": cerr.ComponentIDs})
	}
	if errors.Is(err, issues.ErrHashCollisionExhausted) {
		return skmcp.ToolError(ErrCodeInternalError,
			"could not allocate a unique hash id; try again", nil)
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_create",
		"reason", "create failed",
		"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to create issue", nil)
}

// mapInlineDepError translates ErrInlineDepFailed into the stable MCP
// error envelope. Mirrors mapDepAddError but always carries
// `dependencyIndex`, `toId`, and `type` in details so the agent can
// retry the create with the offending entry removed / fixed without
// guessing which one failed.
func mapInlineDepError(ctx context.Context, depFail *issues.ErrInlineDepFailed, userID, projectID uuid.UUID) *mcpgo.CallToolResult {
	inner := depFail.Err
	base := map[string]any{
		"dependencyIndex": depFail.Index,
		"toId":            depFail.ToID,
		"type":            depFail.Type,
	}
	if vErr, ok := dependencies.IsValidationError(inner); ok {
		base["code"] = vErr.Code
		// sk-118 (F2e/F2f): the standing-edge guards carry actionable,
		// branch-on-able codes — promote them to the top-level envelope `error`
		// (matching skein_dep_add / skein_dep_add_many) instead of burying them
		// under a generic validation_error. The inline-dep details
		// (dependencyIndex/toId/type/code) ride along in `base`.
		switch vErr.Code {
		case ErrCodeNestedStandingForbidden:
			return skmcp.ToolError(ErrCodeNestedStandingForbidden,
				"dependencies["+strconv.Itoa(depFail.Index)+"]: "+vErr.Message, base)
		case ErrCodeBlockerTargetStanding:
			return skmcp.ToolError(ErrCodeBlockerTargetStanding,
				"dependencies["+strconv.Itoa(depFail.Index)+"]: "+vErr.Message, base)
		}
		return skmcp.ToolError(ErrCodeValidationError,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: "+vErr.Message, base)
	}
	if errors.Is(inner, dependencies.ErrFromNotFound) {
		// Shouldn't happen on inline-deps (we just inserted FROM in the
		// same tx) — but surface clearly if it does.
		slog.ErrorContext(ctx, "inline dep create: from-issue missing in same tx",
			"userId", userID.String(), "projectId", projectID.String(),
			"index", depFail.Index, "toId", depFail.ToID)
		return skmcp.ToolError(ErrCodeInternalError,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: from-issue not visible inside its own tx",
			base)
	}
	// INV-3 (sk-674.18 T033): an invisible target answers as a nonexistent one.
	if errors.Is(inner, dependencies.ErrTargetNotFound) || errors.Is(inner, dependencies.ErrDependencyTargetNotVisible) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: target issue does not exist or is not visible",
			base)
	}
	if errors.Is(inner, dependencies.ErrDependencyExists) {
		return skmcp.ToolError(ErrCodeDependencyAlreadyExists,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: this dependency already exists",
			base)
	}
	if errors.Is(inner, dependencies.ErrParentAlreadySet) {
		// Defect 2 / sk-43: an inline parent-child edge cannot ADD a second
		// parent — point the caller at skein_reparent for the move.
		return skmcp.ToolError(ErrCodeParentAlreadySet,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: issue already has a parent — use skein_reparent to move it",
			base)
	}
	if errors.Is(inner, dependencies.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: dependency write lost a serialization race — safe to retry",
			base)
	}
	if errors.Is(inner, dependencies.ErrDependencyTargetNotReady) {
		return skmcp.ToolError(ErrCodeDependencyTargetNotVisibleYet,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: dependency target not visible yet — retry shortly",
			base)
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(inner, &cycle) {
		base["path"] = cycle.Path
		return skmcp.ToolError(ErrCodeDependencyCycle,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: would create a cycle",
			base)
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_create",
		"reason", "inline dep create failed",
		"userId", userID.String(), "projectId", projectID.String(),
		"index", depFail.Index, "toId", depFail.ToID, "type", depFail.Type,
		"error", inner.Error())
	return skmcp.ToolError(ErrCodeInternalError,
		"dependencies["+strconv.Itoa(depFail.Index)+"]: failed to create dependency", base)
}

// parseInlineDeps reads + validates the `dependencies` array argument
// from the MCP tool request. Returns the parsed slice (possibly empty)
// or a populated *mcpgo.CallToolResult error envelope ready to be
// returned by the handler.
//
// Validation is intentionally tight here so the failure points at the
// offending dependencies[i] field, not at the eventual store-level
// rejection. Cap, item shape, hierarchical-id shape, and dep-type/gate
// combos are all checked before the duplicate gate runs — a malformed
// inline-dep should never burn the embedding cost of duplicate
// detection.
func parseInlineDeps(args map[string]any) ([]issues.DependencyEdgeInput, *mcpgo.CallToolResult) {
	raw, ok := args["dependencies"]
	if !ok || raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"dependencies must be an array of {toId, type, gate?} objects",
			nil)
	}
	if len(rawSlice) == 0 {
		return nil, nil
	}
	if len(rawSlice) > inlineDepMaxItems {
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"dependencies exceeds the per-call cap",
			map[string]any{"max": inlineDepMaxItems, "got": len(rawSlice)})
	}
	out := make([]issues.DependencyEdgeInput, 0, len(rawSlice))
	for i, entry := range rawSlice {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"dependencies["+strconv.Itoa(i)+"] must be an object",
				map[string]any{"dependencyIndex": i})
		}
		rawToID, _ := obj["toId"].(string)
		toID := strings.TrimSpace(rawToID)
		if !looksLikeHierarchicalID(toID) {
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"dependencies["+strconv.Itoa(i)+"].toId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"dependencyIndex": i, "toId": rawToID})
		}
		rawType, _ := obj["type"].(string)
		depType, vErr := dependencies.ValidateType(rawType)
		if vErr != nil {
			ve, _ := dependencies.IsValidationError(vErr)
			details := map[string]any{"dependencyIndex": i, "toId": toID}
			if ve != nil {
				details["code"] = ve.Code
			}
			msg := "dependencies[" + strconv.Itoa(i) + "]: " + vErr.Error()
			return nil, skmcp.ToolError(ErrCodeValidationError, msg, details)
		}
		var gateArg *string
		if g, ok := obj["gate"].(string); ok {
			if trimmed := strings.TrimSpace(g); trimmed != "" {
				gateArg = &trimmed
			}
		}
		if _, vErr := dependencies.ValidateGate(depType, gateArg); vErr != nil {
			ve, _ := dependencies.IsValidationError(vErr)
			details := map[string]any{"dependencyIndex": i, "toId": toID, "type": depType}
			if ve != nil {
				details["code"] = ve.Code
			}
			msg := "dependencies[" + strconv.Itoa(i) + "]: " + vErr.Error()
			return nil, skmcp.ToolError(ErrCodeValidationError, msg, details)
		}
		out = append(out, issues.DependencyEdgeInput{
			ToID: toID,
			Type: depType,
			Gate: gateArg,
		})
	}
	return out, nil
}

// validationToolError is a small adapter from issues.ValidationError to
// the MCP tool error envelope. Centralised so every write tool emits the
// same `{error:"validation_error", message:"...", code:"..."}` shape.
//
// sk-31 exception: the `blocked_is_derived` validation code is promoted
// to a top-level error code on the wire (instead of being nested under
// `code:` inside a generic validation_error envelope) so callers can
// branch on it deterministically. The legacy ValidateStatus path is the
// only producer of this code.
func validationToolError(err error) *mcpgo.CallToolResult {
	if ve, ok := issues.IsValidationError(err); ok {
		if ve.Code == "blocked_is_derived" {
			return skmcp.ToolError(ve.Code, ve.Message, nil)
		}
		return skmcp.ToolError(ErrCodeValidationError, ve.Message,
			map[string]any{"code": ve.Code})
	}
	return skmcp.ToolError(ErrCodeValidationError, err.Error(), nil)
}

// callerOriginMode classifies the caller as "agent" or "human" based on
// the auth signals that landed them in this handler. The function is
// fail-closed: returns ok=false when neither signal is present, so the
// caller surfaces an explicit internal_error instead of silently
// defaulting to "human" and mis-tagging the issue's source.
//
// Recognised configurations:
//   - api-key middleware ran → non-nil APIKeyID in ctx → mode="agent"
//   - cookie middleware ran  → UserID set, APIKeyID absent → mode="human"
//   - neither signal present (a future transport regression, a
//     misconfigured router, a hand-rolled ctx in tests) → ok=false
//
// Phase 1 ui-redesign: skein_create requires this distinction so it can
// (a) force source=human for cookie callers regardless of `kind`, and
// (b) require `kind` from agents for the directed/autonomous split.
// callerRole() returns the *user's* role (admin/user) which doesn't
// answer the orthogonal question of "is this caller an agent?"; this
// helper does.
//
// The pre-fix behaviour ("default to human on missing APIKeyID") would
// quietly tag agent calls as source=human if the api-key middleware
// regressed and stopped populating ctx — a silent corruption of the
// audit trail that's hard to detect post-hoc. Fail-closed makes any
// such regression surface immediately as a 500/internal_error.
func callerOriginMode(ctx context.Context) (string, bool) {
	if id, ok := auth.APIKeyIDFrom(ctx); ok && id != uuid.Nil {
		return "agent", true
	}
	if _, ok := auth.UserIDFrom(ctx); ok {
		return "human", true
	}
	return "", false
}

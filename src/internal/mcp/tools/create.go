package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
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
//     The agent reads that and decides: retry with force=true, or call
//     again with duplicateOf=<existing-id>.
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
				"decide yourself — retry with `force:true` if it is distinct, "+
				"or call again with `duplicateOf:\"demo-42\"` (links a "+
				"`duplicates` edge). `degraded` semantics: "+
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
const inlineDepMaxItems = issueops.InlineDepMaxItems

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
	d := issueops.Deps{Pool: pool, Issues: issuesStore, Deps: depsStore, Projects: projectsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Create(ctx, d, userID, issueops.Args(req.GetArguments()), issueops.OriginMCP)
		if f != nil {
			return toolFailure(f), nil
		}
		projectID := res.ProjectID
		if res.Decision {
			return skmcp.ToolJSON(createResponse{
				Created:          false,
				Issue:            nil,
				Duplicates:       res.Duplicates,
				DecisionRequired: true,
				Degraded:         res.Degraded,
				Warnings:         []string{},
			})
		}
		duplicates, duplicateCheckDegraded := res.Duplicates, res.Degraded
		dto := res.Issue
		verbose := req.GetBool("verbose", false)
		cardUnavailable := false
		if rb := res.ReadBack; rb != nil {
			// sk-607: the issue committed but its card could not be read
			// back — acknowledge it from the committed state.
			logReadBackFallback(ctx, "skein_create", userID.String(), rb)
			dto = ackSourceFromCommitted(rb.State)
			dto.InheritedComponentIDs = rb.InheritedComponentIDs
			cardUnavailable = verbose
			verbose = false
		}

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

func hasInlineEdge(deps []issues.DependencyEdgeInput, toID, depType string) bool {
	return issueops.HasInlineEdge(deps, toID, depType)
}

func mapDuplicateOfEdgeError(ctx context.Context, err error, dupEdgeIndex int, duplicateOfID string, userID uuid.UUID) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapDuplicateOfEdgeError(ctx, err, dupEdgeIndex, duplicateOfID, userID))
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

func mapCreateError(ctx context.Context, err error, userID, projectID uuid.UUID) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapCreateError(ctx, err, userID, projectID))
}

func mapInlineDepError(ctx context.Context, depFail *issues.ErrInlineDepFailed, userID, projectID uuid.UUID) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapInlineDepError(ctx, depFail, userID, projectID))
}

func parseInlineDeps(args map[string]any) ([]issues.DependencyEdgeInput, *mcpgo.CallToolResult) {
	v0, f := issueops.ParseInlineDeps(args)
	return v0, toolFailure(f)
}

func validationToolError(err error) *mcpgo.CallToolResult {
	return toolFailure(issueops.ValidationFailure(err))
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

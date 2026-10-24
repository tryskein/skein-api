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

// epicChildTemplateMaxItems caps how many children a single
// skein_epic_create call may inline. Tighter than inlineDepMaxItems
// (50) by design — the whole batch lives inside one SERIALIZABLE tx,
// so each extra child widens the tx footprint and the blast radius of
// a rollback. 50 already covers the realistic ceiling for an epic
// decomposition; agents needing more should call skein_epic_create for
// the epic + 50 first-batch children, then skein_create per remaining
// child with parentId set.
const epicChildTemplateMaxItems = 50

// epicCreateReferenceCardChars is the size of ONE issue card this tool can
// return under `verbose=true`, in bytes; epicCreateMaxResultChars is what
// the tool declares as its result ceiling.
//
// # Both numbers come from a measurement, not from an estimate
//
// Measured 2026-08-20 on this tracker's own production instance: 35 real
// IssueDetails read through skein_show with comments / deps / events off —
// the same object this tool puts in `epic` and in every `children[]` slot
// when the caller opts into the full response. The implementation tasks of
// the sk-282 G-A4 wave measured 6 208 … 9 978 bytes (mean 7 307); the
// plan-level tasks of the same epic 1 433 … 3 568 (mean 2 244). 10 000 is
// the heaviest of those rounded up to the next thousand.
//
// That card times the batch this tool can answer with — the epic plus
// epicChildTemplateMaxItems children, plus the `{epic, children:[…]}`
// envelope and the 50 separators between the cards — comes to 510 072
// bytes, measured by serialising it in
// internal/mcp/tools/epic_create_cap_test.go.
//
// # The declared number is the platform ceiling, and that is not a rounding
//
// 510 072 is ABOVE what a tool is allowed to declare:
// MCPMaxDeclarableResultChars is a hard 500 000 on the client side, so the
// heaviest legal verbose response of this tool cannot be covered by any
// annotation at all. The declaration is therefore the ceiling itself —
// the largest true statement available — and the residual ~10 KB is a
// platform limit, not a decision taken here. A response that overruns the
// threshold is not corrupted: the client persists it to disk and leaves a
// file reference in its place (see result_size.go), so the cost of
// under-declaring is a legitimate answer that leaves the conversation.
//
// # Why it is not 100 000, and not 250 000 either
//
// 100 000 was set by T024 «to match the new form», reasoning from the
// acknowledgement (~4 KB for fifty children) and then adding room for
// verbose without measuring verbose. It covers about ten real cards out of
// the fifty-one this tool can return — and the same tool declaring 100 000
// for ONE card is skein_show. Since the T024 change did not alter the
// verbose response by a single byte, a lower ceiling could only have come
// from a re-measurement, and none was taken.
//
// 250 000, the value before T024, was sized the same way one round earlier
// («fifty-one full cards with long prose») and is also under the measured
// batch: it covers 51 cards only while each stays below 4 901 bytes, which
// the cards of this very wave do not.
//
// # What no number can do
//
// Nothing bounds a card from above. issues.ValidateTitle caps a title at
// 1 000 characters and ValidateLabel a label at 100; description, design,
// acceptanceCriteria, notes and sourceContext have NO length limit anywhere
// in the system, and neither has the count of labels, components or commit
// refs. So even without the platform ceiling no finite declaration would be
// a PROOF against the persist-to-disk path — it could only cover the
// heaviest response the surface has been observed to produce. Two
// consequences worth keeping straight: the reference card moves only on a
// fresh measurement, never on a round-number adjustment; and the
// declaration cannot move at all while the measured batch stays above
// MCPMaxDeclarableResultChars, because it is already at the maximum a tool
// is allowed to state.
const (
	epicCreateReferenceCardChars = 10000
	epicCreateMaxResultChars     = MCPMaxDeclarableResultChars
)

// epicCreateResponse is the wire shape returned by skein_epic_create on
// success: the freshly-loaded epic IssueDetail + one per child in input
// order. On failure the tool returns an MCP error envelope (no `epic`
// or `children` field present); a partial response is never produced —
// the whole batch rolls back atomically.
//
// Both fields are typed `any` since sk-233. Since sk-282 T024 they carry
// the ACKNOWLEDGEMENT — the epic's own, and one row per child with its
// identifier and version (INV-7) — by default, and untouched
// IssueDetails when the caller passes verbose=true. This tool was the
// heaviest echo in the API: an epic plus up to 50 children, each
// previously carrying its full description / design / acceptance / notes
// back into agent context, where every byte is re-sent on every
// subsequent turn.
type epicCreateResponse struct {
	Epic     any   `json:"epic"`
	Children []any `json:"children"`
	// CardUnavailable — sk-607: present (true) only on a verbose call whose
	// tree was created but whose cards could not be read back; `epic` and
	// `children` are then acknowledgements. See cardUnavailableKey.
	CardUnavailable bool `json:"cardUnavailable,omitempty"`
}

// RegisterEpicCreate wires the skein_epic_create tool. Atomic
// decomposition affordance for the sk-32 first-class-epics slice —
// agents call this when they want to spin up an epic AND a known set
// of child issues in one shot, without the round-trip storm of
// skein_create-per-issue and without the risk of a half-built epic
// (epic created, child 4 of 7 failed → orphaned shell).
//
// Atomicity contract:
//   - One SERIALIZABLE tx wraps the epic insert and every child insert.
//   - Any child failure (validation, FK, race) rolls back the whole
//     batch — including the epic itself — and surfaces a typed error
//     envelope identifying the failing index.
//   - Children skip the layer-1 / layer-2 duplicate gate entirely.
//     Siblings under one epic often have similar titles ("write tests
//     for X" + "write tests for Y") and triggering decision_required
//     mid-decomposition would defeat the purpose of the batch.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false  — retry creates a new epic + new children
//   - openWorldHint: false
//
// _meta:
//
//   - anthropic/maxResultSizeChars: epicCreateMaxResultChars — sized on
//     the VERBOSE mode, because INV-5 asks what the tool CAN emit, and a
//     ceiling fitted to the acknowledgement would truncate the opt-in
//     full response with no truncation marker (the trap the registry
//     names for skein_show). The number is derived from a measurement of
//     real cards rather than from the shape of the default answer — see
//     the constant for what was measured, and epic_create_cap_test.go for
//     the guard that re-derives it by serialising the response.
//
//     US-12 ("the declared result-size ceiling is brought in line with
//     the new form — otherwise it remains a monument to the previous
//     volume") is satisfied in the direction the invariant actually
//     points: the DEFAULT answer got smaller, the verbose one did not
//     move by a byte, and the ceiling belongs to the verbose one.
func RegisterEpicCreate(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_epic_create",
		mcpgo.WithDescription(withAckAnswerNote(
			"Atomically create an epic and 0..50 child issues under it in a "+
				"single SERIALIZABLE transaction. Use skein_epic_create when "+
				"decomposing a feature, story, or chunk of work into 3+ "+
				"sub-tasks that should be tracked together. Children are "+
				"created in input order; each child's parentId is "+
				"automatically set to the newly-allocated epic id, so child "+
				"ids land as epicId.1, epicId.2, … regardless of the "+
				"project's default id-generation mode.\n\n"+
				"For an EMPTY epic (no children to plan up front), use the "+
				"plain skein_create with type=\"epic\" instead — this tool's "+
				"value is the atomic decomposition, not the epic itself.\n\n"+
				"Atomicity: any child failure (validation, FK, race) rolls "+
				"back the ENTIRE batch — the epic vanishes too. There is "+
				"never a half-built decomposition on disk. A batch that "+
				"lost a database serialization race after every retry "+
				"(nothing written) returns `serialization_retry` — retry "+
				"the call verbatim. On success the "+
				"response is an ACKNOWLEDGEMENT, not a set of issue cards: "+
				"`epic` and one `children` entry per child in input order, "+
				"each carrying the identifier, the applied state and the "+
				"version — enough to continue an expectedVersion cycle "+
				"without re-reading — and no issue prose at all. Pass "+
				"`verbose` for the full IssueDetail of the epic and of every "+
				"child, or read one of them later with skein_show.\n\n"+
				"Children skip the layer-1 / layer-2 duplicate gate that "+
				"skein_create runs by default — siblings under one epic "+
				"often have legitimately similar titles (\"write tests for "+
				"X\", \"write tests for Y\") and decision_required mid-"+
				"decomposition would defeat the batch. The epic itself is "+
				"NOT duplicate-checked either; callers wanting a "+
				"duplicate-aware epic should fall back to skein_create + "+
				"per-child skein_create calls.\n\n"+
				"`kind` is REQUIRED for agent callers (same semantics as "+
				"skein_create): \"directed\" = user-requested, "+
				"\"autonomous\" = agent side-quest. The value maps to "+
				"source=\"agent_directed\" / source=\"agent_autonomous\" "+
				"and propagates to every child (so the audit trail is "+
				"uniform across the decomposition). Web UI callers force "+
				"source=human and may omit `kind`.\n\n"+
				assigneeDefaultNote+" The assignee of the epic and of each child is decided "+
				"separately, by its own `assignee` / `unassigned`; the epic's values "+
				"do not propagate to children, and the epic created by this call counts as "+
				"their parent for the someone-else's-parent rule.",
		)),
		mcpgo.WithString("project",
			mcpgo.Required(),
			mcpgo.Description("Project handle: alias, name, or UUID."),
		),
		mcpgo.WithString("title",
			mcpgo.Required(),
			mcpgo.Description("Epic title (1..1000 chars). Short, imperative — same conventions as skein_create."),
		),
		mcpgo.WithString("description",
			mcpgo.Description("Long-form epic description (markdown)."),
		),
		mcpgo.WithString("design",
			mcpgo.Description("Design notes / approach for the epic."),
		),
		mcpgo.WithString("acceptanceCriteria",
			mcpgo.Description("Acceptance criteria for the epic (markdown)."),
		),
		mcpgo.WithString("notes",
			mcpgo.Description("Free-form notes for the epic."),
		),
		mcpgo.WithNumber("priority",
			mcpgo.Description("Epic priority 0..4 (0=highest). Default: 3."),
			mcpgo.Min(0),
			mcpgo.Max(4),
			mcpgo.DefaultNumber(3),
		),
		mcpgo.WithString("assignee",
			mcpgo.Description("For the epic. "+assigneeDefaultParamDescription),
		),
		mcpgo.WithBoolean("unassigned",
			mcpgo.Description("For the epic; does not propagate to children. "+unassignedParamDescription),
		),
		mcpgo.WithString("parentId",
			mcpgo.Description("Full hierarchical id of the issue the EPIC itself hangs under "+
				"(e.g. \"demo-42\"). Omit it and the epic lands at the project root, as before. "+
				"Supplying it switches the epic's own id-generation to hierarchical, so the "+
				"node arrives as `<parentId>.N` instead of a project-counter id — that is the "+
				"same shape skein_create(parentId=…) produces, and it is visible to "+
				"skein_list(parentId=…) immediately, with no follow-up skein_reparent. "+
				"Children are unaffected: their parent is always the new epic. "+
				"A parent that is closed, missing, or in a project you cannot see is "+
				"rejected and NOTHING is created."),
		),
		mcpgo.WithArray("componentIds",
			mcpgo.Description("Component UUIDs to attach to the epic. Must belong to the same project as the epic. Each child inherits this set by default via the existing parent-component inheritance path — supply childTemplates[i].componentIds to override per-child."),
			mcpgo.Items(map[string]any{"type": "string"}),
		),
		mcpgo.WithArray("labels",
			mcpgo.Description("Free-form labels to attach to the epic. Per-child labels go in "+
				"childTemplates[i].labels — they are NOT inherited from the epic."),
			mcpgo.Items(map[string]any{"type": "string"}),
		),
		mcpgo.WithString("sourceContext",
			mcpgo.Description("Free-form annotation describing why the agent created this epic (e.g. \"decomposing sk-32\")."),
		),
		mcpgo.WithString("kind",
			mcpgo.Description("Issue origin classification (REQUIRED for agent callers): "+
				"\"directed\" = the human user told the agent to create this "+
				"epic; \"autonomous\" = the agent decided to create this epic "+
				"mid-task (side-quest, discovered subtask). Web UI ignores "+
				"this parameter and forces source=human; MCP callers must "+
				"specify it. The value propagates to every child."),
			mcpgo.Enum("directed", "autonomous"),
		),
		mcpgo.WithBoolean("reviewRequired",
			mcpgo.Description("Whether the EPIC's author will want to review the result once "+
				"it is closed. Private bookkeeping of the AUTHOR: never returned to anyone "+
				"else, never shown in any feed. Omit it and your personal default applies "+
				"(built-in default: true). Pass false to opt this one epic out — the usual "+
				"case for a structural node that nobody reads a result from. It does NOT "+
				"propagate to children: each child keeps the author's default, because the "+
				"work whose result gets read lives in the children, not in the node."),
		),
		mcpgo.WithArray("childTemplates",
			mcpgo.Description("Child issues to create atomically with the epic. "+
				"Each entry: {title, type?, description?, design?, "+
				"acceptanceCriteria?, notes?, priority?, assignee?, "+
				"unassigned?, componentIds?, labels?}. "+
				"type defaults to \"task\"; priority defaults to 3. "+
				"max "+epicChildTemplateMaxStr+" items per call. Children skip the "+
				"duplicate-detection gate (siblings can legitimately share "+
				"near-identical titles). Each child's parentId is "+
				"automatically set to the new epic's id — do NOT supply it."),
			mcpgo.MaxItems(epicChildTemplateMaxItems),
			mcpgo.Items(map[string]any{
				"type":     "object",
				"required": []string{"title"},
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "Child title (1..1000 chars).",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"bug", "feature", "task", "chore", "decision", "idea", "note"},
						"description": "Child issue type. Default: task. Epic is intentionally excluded — nested epics aren't a supported decomposition shape; create the nested epic separately via skein_epic_create with parentId set on the inner epic's children if needed.",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "Long-form description (markdown).",
					},
					"design": map[string]any{
						"type":        "string",
						"description": "Design notes.",
					},
					"acceptanceCriteria": map[string]any{
						"type":        "string",
						"description": "Acceptance criteria.",
					},
					"notes": map[string]any{
						"type":        "string",
						"description": "Free-form notes.",
					},
					"priority": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"maximum":     4,
						"description": "Priority 0..4 (0=highest). Default: 3.",
					},
					"assignee": map[string]any{
						"type":        "string",
						"description": "Assignee handle. Omitted, null or blank — the creator's «assign to me» setting decides, as for skein_create. A non-string value is refused with validation_error (childIndex set), nothing created.",
					},
					"unassigned": map[string]any{
						"type":        "boolean",
						"description": "true — this child stays unassigned whatever the setting says; cannot be combined with a non-empty assignee. Not inherited from the epic.",
					},
					"componentIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional per-child component override (UUIDs). When omitted (and the epic has componentIds set), the child inherits the epic's component set via parent-component inheritance.",
					},
					"labels": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Free-form labels for this child. Not inherited from the epic — a child with no labels key gets none.",
					},
				},
			}),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription+" Applies to the epic and to every "+
				"child row alike."+cardUnavailableNote),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		// See epicCreateMaxResultChars for what the number is measured
		// from, and why it is sized on `verbose` rather than on the
		// acknowledgement.
		"anthropic/maxResultSizeChars": epicCreateMaxResultChars,
	})

	addTool(s, tool, handleEpicCreate(pool, issuesStore, projectsStore))
}

const epicChildTemplateMaxStr = "50"

func handleEpicCreate(pool *pgxpool.Pool, issuesStore *issues.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
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

		// ---- Epic title + validation ----
		rawTitle, err := req.RequireString("title")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "title is required", nil), nil
		}
		title, vErr := issues.ValidateTitle(rawTitle)
		if vErr != nil {
			return validationToolError(vErr), nil
		}

		// Epic priority — same default (3) as skein_create.
		priority := req.GetInt("priority", 3)
		if vErr := issues.ValidatePriority(priority); vErr != nil {
			return validationToolError(vErr), nil
		}

		description := req.GetString("description", "")
		design := req.GetString("design", "")
		acceptance := req.GetString("acceptanceCriteria", "")
		notes := req.GetString("notes", "")

		// Guard the epic's own free-text fields (sk-28). Children are
		// validated in epicChildTemplatesFromArgs below.
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

		// sk-605: a non-string assignee is refused before any write.
		assigneeArg, assigneeErr := assigneeFromArgs(req.GetArguments(), "", nil)
		if assigneeErr != nil {
			return assigneeErr, nil
		}
		// sk-565 (P9, US-4/US-5): the epic's own `unassigned`. It does not
		// propagate — each child has its own (parseChildTemplates).
		epicAssignByDefault, modeErr := assignCreatorByDefault(req.GetArguments(), assigneeArg, "", nil)
		if modeErr != nil {
			return modeErr, nil
		}
		var sourceContextArg *string
		if sc := strings.TrimSpace(req.GetString("sourceContext", "")); sc != "" {
			if vErr := issues.ValidateProse("sourceContext", sc); vErr != nil {
				return validationToolError(vErr), nil
			}
			sourceContextArg = &sc
		}

		// Top-level componentIds — caller-supplied UUIDs that attach to
		// the epic at create time. Child templates inherit the epic's
		// component set by default via the existing parent-component
		// inheritance path (issues.Store step 1b); per-child overrides
		// live in childTemplates[i].componentIds (parsed in
		// parseChildTemplates below). `Explicit=true` when the key was
		// present in the wire payload with a non-null value (a present
		// `[]` counts as "explicit no components, suppress inheritance";
		// a present `null` is treated as omitted because morally the
		// caller said nothing).
		args := req.GetArguments()
		epicComponentIDs, parseErr := parseComponentIDs(args, "componentIds", nil)
		if parseErr != nil {
			return parseErr, nil
		}
		epicComponentIDsExplicit := false
		if v, exists := args["componentIds"]; exists && v != nil {
			epicComponentIDsExplicit = true
		}

		// parentId — optional, and its absence is the legacy behaviour:
		// the epic lands at the project root. Shape is validated here so
		// a malformed handle fails as validation_error rather than as a
		// parent lookup miss inside the store.
		var parentIDArg *string
		if pid := strings.TrimSpace(req.GetString("parentId", "")); pid != "" {
			if !looksLikeHierarchicalID(pid) {
				return skmcp.ToolError(ErrCodeValidationError,
					"parentId must be a full hierarchical id like \"demo-42\"",
					map[string]any{"parentId": pid}), nil
			}
			parentIDArg = &pid
		}

		// Epic labels, canonicalised through the same validator
		// skein_create uses; per-child labels are parsed in
		// parseChildTemplates.
		//
		// Read from the raw arguments rather than through
		// req.GetStringSlice: that helper keeps the elements that assert
		// to string and DROPS the rest, so `labels: ["triage", 7]` would
		// create the epic with one label and no complaint. A label the
		// caller asked for and did not get must not vanish quietly, and
		// the per-child path right below rejects the same input — one
		// tool answering two ways about one field is worse than the
		// divergence from skein_create, which has the same silent drop.
		var epicLabels []string
		if rawLabels, exists := args["labels"]; exists && rawLabels != nil {
			rawSlice, okSlice := rawLabels.([]any)
			if !okSlice {
				return skmcp.ToolError(ErrCodeValidationError,
					"labels must be an array of strings", nil), nil
			}
			epicLabels = make([]string, 0, len(rawSlice))
			for j, raw := range rawSlice {
				str, okStr := raw.(string)
				if !okStr {
					return skmcp.ToolError(ErrCodeValidationError,
						"labels["+strconv.Itoa(j)+"] must be a string",
						map[string]any{"labelIndex": j}), nil
				}
				canon, vErr := issues.ValidateLabel(str)
				if vErr != nil {
					return validationToolError(vErr), nil
				}
				epicLabels = append(epicLabels, canon)
			}
		}

		// reviewRequired — read from the RAW arguments, not via GetBool
		// with a default: "did not pass" and "passed false" are different
		// intents, and a default would silently cancel the author's
		// personal default on every call that does not mention the flag.
		reviewRequiredArg, reviewErr := createReviewRequiredArg(args)
		if reviewErr != nil {
			return reviewErr, nil
		}

		// ---- Determine effective source from caller mode + kind ----
		// Mirrors skein_create — agents must supply `kind`; cookie-auth
		// (Web UI) callers force source=human and may omit kind.
		kindArg := strings.TrimSpace(req.GetString("kind", ""))
		callerMode, ok := callerOriginMode(ctx)
		if !ok {
			slog.ErrorContext(ctx, "callerOriginMode could not classify caller",
				"tool", "skein_epic_create", "userId", userID.String())
			return skmcp.ToolError(ErrCodeInternalError,
				"ambiguous caller mode; cannot determine issue source", nil), nil
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
			slog.ErrorContext(ctx, "callerOriginMode returned unexpected mode",
				"tool", "skein_epic_create", "mode", callerMode, "userId", userID.String())
			return skmcp.ToolError(ErrCodeInternalError,
				"unknown caller mode; cannot determine issue source",
				map[string]any{"mode": callerMode}), nil
		}

		// ---- Parse childTemplates ----
		childTemplates, parseResp := parseChildTemplates(req.GetArguments())
		if parseResp != nil {
			return parseResp, nil
		}

		// sk-674.18 T066: creating the epic and its children is ActionWrite
		// of US-6; naming an assignee on the epic or on any child is
		// ActionAssign too. Every input is parsed by now; nothing is written
		// before this gate.
		epicActions := []authz.Action{authz.ActionWrite}
		namesAssignee := assigneeArg != nil
		for _, ct := range childTemplates {
			namesAssignee = namesAssignee || ct.Assignee != nil
		}
		if namesAssignee {
			epicActions = append(epicActions, authz.ActionAssign)
		}
		if res := projectActionGate(ctx, pool, "skein_epic_create", projectArg, projectID, userID, epicActions...); res != nil {
			return res, nil
		}

		// ---- Build epic input ----
		epicInput := issues.CreateIssueInput{
			ParentID:             parentIDArg,
			Title:                title,
			Description:          description,
			Type:                 issues.TypeEpic,
			Priority:             priority,
			Source:               effectiveSource,
			SourceContext:        sourceContextArg,
			Assignee:             assigneeArg,
			Design:               design,
			AcceptanceCriteria:   acceptance,
			Notes:                notes,
			ComponentIDs:         epicComponentIDs,
			ComponentIDsExplicit: epicComponentIDsExplicit,
			Labels:               epicLabels,
			ReviewRequired:       reviewRequiredArg,
			// sk-565 (P1): see skein_create — the store applies the setting.
			AssignCreatorByDefault: epicAssignByDefault,
		}
		// DependenciesCaller is the resolved (uuid, role) the store uses
		// for the parent's cross-project visibility check — the same
		// trigger skein_create has since sk-35. Children need none: their
		// parent is the epic just inserted in this very transaction, in
		// this very project.
		if parentIDArg != nil && *parentIDArg != "" {
			epicInput.DependenciesCaller = &issues.DependenciesCaller{
				UserID: userID,
				Role:   role,
			}
		}

		project, err := projectsStore.GetProject(ctx, projectID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_epic_create",
				"reason", "load project failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load project metadata", nil), nil
		}
		actor := IssuesAgentActor(ctx, pool, userID)

		// ---- Atomic create ----
		cardUnavailable := false
		epicDto, childDtos, err := issuesStore.CreateEpicWithChildren(ctx,
			projectID, project.IDPrefix, project.DefaultIDMode,
			epicInput, childTemplates, effectiveSource, userID, actor)
		if err != nil {
			rb, committed := asReadBackFailed(err)
			if !committed {
				return mapEpicCreateError(ctx, err, userID, projectID), nil
			}
			// sk-607: the epic and every child exist; only reading their
			// cards back failed. Answer the acknowledgements — under
			// verbose too, marked cardUnavailable — never an error that
			// tells a retrying agent nothing was created.
			cardUnavailable = req.GetBool("verbose", false)
			logReadBackFallback(ctx, "skein_epic_create", userID.String(), rb)
			epicDto = ackSourceFromCommitted(rb.State)
			childDtos = make([]*issues.IssueDetail, 0, len(rb.Children))
			for _, c := range rb.Children {
				childDtos = append(childDtos, ackSourceFromCommitted(c))
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_epic_create",
			"userId", userID.String(), "projectId", projectID.String(),
			"epicId", epicDto.ID, "childCount", len(childDtos),
			"latencyMs", time.Since(start).Milliseconds())

		// sk-282 T024 / D-6, FP-9: the fourth and last site that never
		// reached the common write-response point. Both halves now go
		// through the acknowledgement — the epic through ackPayload, the
		// children through ackBatchFromIssues — so the heaviest single
		// action response in the API stops returning fifty-one issue
		// cards.
		//
		// The `{epic, children}` ENVELOPE is unchanged. It is this tool's
		// own contract (the epic is a distinguished element, not row zero
		// of a batch), and US-12 asks for "an acknowledgement of the epic
		// and one row per child", which is exactly what this is.
		//
		// ATOMICITY IS UNTOUCHED: the store still runs one SERIALIZABLE
		// transaction, any child failure rolls back the epic too and
		// surfaces a typed error envelope. Nothing here can produce a
		// partial response — a failed batch never reaches this code.
		verbose := req.GetBool("verbose", false) && !cardUnavailable
		epicOut, perr := ackPayload(epicDto, verbose)
		if perr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_epic_create", "reason", "issue detail projection failed",
				"userId", userID.String(), "issueId", epicDto.ID, "error", perr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}

		childrenOut := make([]any, 0, len(childDtos))
		if verbose {
			for _, child := range childDtos {
				childrenOut = append(childrenOut, child)
			}
		} else {
			// ackBatchFromIssues rather than a loop over ackFromIssue: it
			// carries the nil-slot guard (a child with no post-state is a
			// missing load, not a row) and the all-or-nothing rollup, and
			// using it keeps the claim batch and this tool on ONE
			// definition of «row per created issue».
			//
			// Its `summary` is deliberately not carried onto the wire:
			// `children` already has one row per created child, and on an
			// all-or-nothing tool `failed` is 0 in every response that
			// can exist. A field that is constant across every possible
			// response carries no information and costs bytes on all of
			// them — the exact trade this whole change is about.
			batch, berr := ackBatchFromIssues(childDtos)
			if berr != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_epic_create", "reason", "issue detail projection failed",
					"userId", userID.String(), "epicId", epicDto.ID, "error", berr.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
			}
			for _, row := range batch.Results {
				childrenOut = append(childrenOut, row)
			}
		}

		return skmcp.ToolJSON(epicCreateResponse{
			Epic:            epicOut,
			Children:        childrenOut,
			CardUnavailable: cardUnavailable,
		})
	}
}

// parseChildTemplates reads + validates the `childTemplates` array
// from the MCP tool request. Returns a slice of CreateIssueInput
// (possibly empty when the caller wants just the epic) or a populated
// *mcpgo.CallToolResult error envelope ready for return.
//
// Validation is tight at the call boundary so the failure points at
// the offending childTemplates[i] field — the store-side errors don't
// know the original input index.
func parseChildTemplates(args map[string]any) ([]issues.CreateIssueInput, *mcpgo.CallToolResult) {
	raw, ok := args["childTemplates"]
	if !ok || raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"childTemplates must be an array of {title, type?, ...} objects",
			nil)
	}
	if len(rawSlice) == 0 {
		return nil, nil
	}
	if len(rawSlice) > epicChildTemplateMaxItems {
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"childTemplates exceeds the per-call cap",
			map[string]any{"max": epicChildTemplateMaxItems, "got": len(rawSlice)})
	}
	out := make([]issues.CreateIssueInput, 0, len(rawSlice))
	for i, entry := range rawSlice {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"] must be an object",
				map[string]any{"childIndex": i})
		}
		rawTitle, _ := obj["title"].(string)
		title, vErr := issues.ValidateTitle(rawTitle)
		if vErr != nil {
			ve, _ := issues.IsValidationError(vErr)
			details := map[string]any{"childIndex": i}
			if ve != nil {
				details["code"] = ve.Code
			}
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].title: "+vErr.Error(), details)
		}

		// Type defaults to task. Whitelist excludes 'epic' — nested
		// epics aren't a supported decomposition; agents needing
		// nested epics call skein_epic_create per epic and skein_create
		// for cross-epic deps.
		rawType, _ := obj["type"].(string)
		childType := strings.TrimSpace(rawType)
		if childType == "" {
			childType = issues.TypeTask
		}
		validType, vErr := issues.ValidateType(childType)
		if vErr != nil {
			ve, _ := issues.IsValidationError(vErr)
			details := map[string]any{"childIndex": i}
			if ve != nil {
				details["code"] = ve.Code
			}
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].type: "+vErr.Error(), details)
		}
		if validType == issues.TypeEpic {
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].type cannot be \"epic\" — nested epics are not supported via skein_epic_create",
				map[string]any{"childIndex": i})
		}

		// Priority — default 3 (match skein_create + epic default).
		var priority int
		if pf, ok := obj["priority"].(float64); ok {
			priority = int(pf)
		} else if pi, ok := obj["priority"].(int); ok {
			priority = pi
		} else {
			priority = 3
		}
		if vErr := issues.ValidatePriority(priority); vErr != nil {
			ve, _ := issues.IsValidationError(vErr)
			details := map[string]any{"childIndex": i}
			if ve != nil {
				details["code"] = ve.Code
			}
			return nil, skmcp.ToolError(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].priority: "+vErr.Error(), details)
		}

		description, _ := obj["description"].(string)
		design, _ := obj["design"].(string)
		acceptance, _ := obj["acceptanceCriteria"].(string)
		notes, _ := obj["notes"].(string)

		// Guard each child's free-text fields (sk-28). One bad child
		// fails the whole batch — atomic with the parent epic.
		for _, pair := range [...]struct{ field, val string }{
			{"description", description},
			{"design", design},
			{"acceptanceCriteria", acceptance},
			{"notes", notes},
		} {
			if vErr := issues.ValidateProse(
				"childTemplates["+strconv.Itoa(i)+"]."+pair.field, pair.val,
			); vErr != nil {
				return nil, validationToolError(vErr)
			}
		}

		// sk-605: a non-string assignee is refused, naming the child.
		assigneeArg, assigneeErr := assigneeFromArgs(obj,
			"childTemplates["+strconv.Itoa(i)+"].", map[string]any{"childIndex": i})
		if assigneeErr != nil {
			return nil, assigneeErr
		}
		// sk-565 (P9, US-5): the child's own `unassigned`, never the epic's.
		childAssignByDefault, modeErr := assignCreatorByDefault(obj, assigneeArg,
			"childTemplates["+strconv.Itoa(i)+"].", map[string]any{"childIndex": i})
		if modeErr != nil {
			return nil, modeErr
		}

		// Per-child componentIds override. When absent (key missing on
		// the child object, or present-with-null), ComponentIDsExplicit
		// stays false and the store-level inheritance step fires — the
		// child inherits the epic's components (recorded just before
		// this insert in the same SERIALIZABLE tx). When present with a
		// non-null value (including an empty array), the explicit value
		// wins and no inheritance fires. A bare `null` is treated as
		// "caller said nothing" — same as omitting the key.
		idxCopy := i
		childComponentIDs, parseErr := parseComponentIDs(obj, "componentIds", &idxCopy)
		if parseErr != nil {
			return nil, parseErr
		}
		childComponentIDsExplicit := false
		if v, exists := obj["componentIds"]; exists && v != nil {
			childComponentIDsExplicit = true
		}

		// Per-child labels. Absent key means no labels — the epic's own
		// labels are NOT inherited, unlike componentIds: a component says
		// where the work lives (shared by construction across a
		// decomposition), a label is an assertion about one issue.
		var childLabels []string
		if rawChildLabels, exists := obj["labels"]; exists && rawChildLabels != nil {
			rawSliceLabels, okLabels := rawChildLabels.([]any)
			if !okLabels {
				return nil, skmcp.ToolError(ErrCodeValidationError,
					"childTemplates["+strconv.Itoa(i)+"].labels must be an array of strings",
					map[string]any{"childIndex": i})
			}
			childLabels = make([]string, 0, len(rawSliceLabels))
			for j, rawLabel := range rawSliceLabels {
				str, okStr := rawLabel.(string)
				if !okStr {
					return nil, skmcp.ToolError(ErrCodeValidationError,
						"childTemplates["+strconv.Itoa(i)+"].labels["+strconv.Itoa(j)+"] must be a string",
						map[string]any{"childIndex": i, "labelIndex": j})
				}
				canon, vErr := issues.ValidateLabel(str)
				if vErr != nil {
					ve, _ := issues.IsValidationError(vErr)
					details := map[string]any{"childIndex": i}
					if ve != nil {
						details["code"] = ve.Code
					}
					return nil, skmcp.ToolError(ErrCodeValidationError,
						"childTemplates["+strconv.Itoa(i)+"].labels: "+vErr.Error(), details)
				}
				childLabels = append(childLabels, canon)
			}
		}

		out = append(out, issues.CreateIssueInput{
			Title:                title,
			Description:          description,
			Type:                 validType,
			Priority:             priority,
			Design:               design,
			AcceptanceCriteria:   acceptance,
			Notes:                notes,
			Assignee:             assigneeArg,
			ComponentIDs:         childComponentIDs,
			ComponentIDsExplicit: childComponentIDsExplicit,
			Labels:               childLabels,
			// sk-565 (P1).
			AssignCreatorByDefault: childAssignByDefault,
		})
	}
	return out, nil
}

// parseComponentIDs reads a `componentIds` array of UUID strings from
// the supplied args map under `key`. Returns nil + nil when the key is
// absent (i.e. not supplied). Empty arrays return an empty non-nil
// slice so the caller can treat "explicit empty" as a real signal.
// indexHint, when non-nil, is folded into validation error details
// (used by the per-child path to surface `childIndex` in the typed
// error).
func parseComponentIDs(args map[string]any, key string, indexHint *int) ([]uuid.UUID, *mcpgo.CallToolResult) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		details := map[string]any{}
		if indexHint != nil {
			details["childIndex"] = *indexHint
		}
		return nil, skmcp.ToolError(ErrCodeValidationError,
			key+" must be an array of UUID strings", details)
	}
	if len(rawSlice) == 0 {
		return []uuid.UUID{}, nil
	}
	out := make([]uuid.UUID, 0, len(rawSlice))
	for i, entry := range rawSlice {
		s, ok := entry.(string)
		if !ok {
			details := map[string]any{"position": i}
			if indexHint != nil {
				details["childIndex"] = *indexHint
			}
			return nil, skmcp.ToolError(ErrCodeValidationError,
				key+"["+strconv.Itoa(i)+"] must be a UUID string", details)
		}
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			details := map[string]any{"position": i}
			if indexHint != nil {
				details["childIndex"] = *indexHint
			}
			return nil, skmcp.ToolError(ErrCodeValidationError,
				key+"["+strconv.Itoa(i)+"] is not a valid UUID", details)
		}
		out = append(out, id)
	}
	return out, nil
}

// mapEpicCreateError translates the layered errors from
// CreateEpicWithChildren into the stable MCP error envelope. When the
// failure was on a child template, we surface the index so the agent
// can fix that one entry without re-binding the whole call.
//
// sk-303.17 coderev: CreateEpicWithChildren lost EVERY
// epicCreateMaxAttempts retry against a genuine SERIALIZABLE abort used
// to fall straight through every branch below into the
// ErrCodeInternalError default — the exact sk-305 defect shape this
// diff's retry was meant to close, just pushed to the tail of the
// budget. issues.Store now degrades that exhaustion to
// issues.ErrSerializationRetry (neither the epic nor any child was ever
// committed, so there is no version to blame it on); map it to the SAME
// `serialization_retry` code Claim already uses for the identical
// situation. Checked FIRST, before the *ErrEpicChildFailed unwrap, since
// an exhausted retry arrives as the bare sentinel (the store already
// unwraps through ErrEpicChildFailed to detect it) — there is no
// meaningful childIndex to report for a whole-transaction abort.
func mapEpicCreateError(ctx context.Context, err error, userID, projectID interface{ String() string }) *mcpgo.CallToolResult {
	if errors.Is(err, issues.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"epic create lost a serialization race and made no change — retry the call",
			map[string]any{"projectId": projectID.String()})
	}
	// sk-607: the caller's context ended inside the one transaction that
	// writes the epic and its children — a stop, not a failure. Checked
	// before the child unwrap: a child insert cut by the context is the
	// same stop, and its index says nothing about what to fix.
	if code, message, ok := contextStop(ctx, ctx, err); ok {
		return skmcp.ToolError(code, message, map[string]any{"projectId": projectID.String()})
	}
	if errors.Is(err, issues.ErrAssigneeNotAllowed) && !errors.As(err, new(*issues.ErrEpicChildFailed)) {
		return assigneeNotAllowedResult(map[string]any{"projectId": projectID.String()})
	}
	var childFail *issues.ErrEpicChildFailed
	if errors.As(err, &childFail) {
		inner := childFail.Err
		base := map[string]any{"childIndex": childFail.Index}
		if errors.Is(inner, issues.ErrAssigneeNotAllowed) {
			return assigneeNotAllowedResult(base)
		}
		if vErr, ok := issues.IsValidationError(inner); ok {
			base["code"] = vErr.Code
			return skmcp.ToolError(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(childFail.Index)+"]: "+vErr.Message, base)
		}
		var cerr *issues.ErrComponentsWrongProject
		if errors.As(inner, &cerr) {
			base["componentIds"] = cerr.ComponentIDs
			return skmcp.ToolError(ErrCodeComponentsWrongProject,
				"childTemplates["+strconv.Itoa(childFail.Index)+"]: some components do not belong to this project", base)
		}
		if errors.Is(inner, issues.ErrIDTaken) {
			return skmcp.ToolError(ErrCodeIDTaken,
				"childTemplates["+strconv.Itoa(childFail.Index)+"]: an issue with this id already exists", base)
		}
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", "skein_epic_create",
			"reason", "child create failed",
			"userId", userID.String(), "projectId", projectID.String(),
			"childIndex", childFail.Index, "error", inner.Error())
		return skmcp.ToolError(ErrCodeInternalError,
			"childTemplates["+strconv.Itoa(childFail.Index)+"]: failed to create child", base)
	}
	// Epic itself failed — map the same way create.go does.
	if vErr, ok := issues.IsValidationError(err); ok {
		// Same promotion create.go does: the nested-standing ban carries
		// an actionable code, so it must not be flattened into a generic
		// validation_error.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return skmcp.ToolError(ErrCodeNestedStandingForbidden, vErr.Message, nil)
		}
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	// Parent ineligible — reachable only since the epic accepts parentId.
	// Codes and their surfaces are create.go's, deliberately: two tools
	// that reject the same parent for the same reason must answer with
	// the same code.
	var perr *issues.ErrParentNotEligible
	if errors.As(err, &perr) {
		switch perr.Code {
		case "parent_not_found":
			return skmcp.ToolError(ErrCodeParentNotFound, perr.Message, nil)
		case "parent_closed":
			return skmcp.ToolError(ErrCodeParentClosed, perr.Message, nil)
		case "parent_wrong_project":
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
	if errors.Is(err, issues.ErrIDTaken) {
		return skmcp.ToolError(ErrCodeIDTaken,
			"an issue with this id already exists", nil)
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_epic_create",
		"reason", "epic create failed",
		"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to create epic", nil)
}

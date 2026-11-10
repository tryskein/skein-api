package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
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
const epicChildTemplateMaxItems = issueops.EpicChildTemplateMaxItems

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
	d := issueops.Deps{Pool: pool, Issues: issuesStore, Projects: projectsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.EpicCreate(ctx, d, userID, issueops.Args(req.GetArguments()), issueops.OriginMCP)
		if f != nil {
			return toolFailure(f), nil
		}
		projectID := res.ProjectID
		epicDto, childDtos := res.Epic, res.Children
		cardUnavailable := false
		if rb := res.ReadBack; rb != nil {
			// sk-607: the tree committed but could not be read back —
			// acknowledge it from the committed states.
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

func parseChildTemplates(args map[string]any) ([]issues.CreateIssueInput, *mcpgo.CallToolResult) {
	v0, f := issueops.ParseChildTemplates(args)
	return v0, toolFailure(f)
}

func parseComponentIDs(args map[string]any, key string, indexHint *int) ([]uuid.UUID, *mcpgo.CallToolResult) {
	v0, f := issueops.ParseComponentIDs(args, key, indexHint)
	return v0, toolFailure(f)
}

func mapEpicCreateError(ctx context.Context, err error, userID, projectID interface{ String() string }) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapEpicCreateError(ctx, err, userID, projectID))
}

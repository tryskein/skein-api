package tools

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterUpdate wires the skein_update tool. Patches issue fields with
// optimistic concurrency, plus routes status / components / labels
// through the dedicated store methods. The whole change is ONE
// SERIALIZABLE transaction (issues.Store.UpdateIssue, T015, US-14): a
// refusal at any step writes nothing and leaves the version where it was.
//
// R2.3: this tool is lock-aware. The caller must hold the lock on the
// issue (locked_by = caller's session) or pass force=true (force_unlock).
// status="closed" is routed to the dedicated Close store method which
// also clears the lock and emits issue.closed + issue.unclaimed events.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false
//   - openWorldHint: false
func RegisterUpdate(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_update",
		mcpgo.WithDescription(withAckAnswerNote(
			"Patch an existing issue. Supports field updates (title / "+
				"description / type / priority / assignee / due dates / "+
				"design / acceptance / notes / external ref / source "+
				"context / estimated minutes), status transitions, "+
				"component-set replace, and label add/remove — all in "+
				"one tool call. Required: `id` (full hierarchical) and "+
				"`expectedVersion` for optimistic concurrency. On version "+
				"mismatch returns "+
				"`{error:\"version_conflict\", currentVersion:<int>}`. "+
				"Locks are honoured: caller must hold the lock or pass "+
				"`force=true` (force_unlock: project owner, space owner, instance admin). This tool does NOT touch the "+
				"lock — to release the lock use `skein_unclaim(id, "+
				"nextStatus, reason)` (atomic over lock+status) or "+
				"`skein_close` (terminal close). The change is atomic: "+
				"fields, status, components and labels are written "+
				"together or not at all — any error means nothing was "+
				"written and the version did not move.\n\n"+
				"A transaction abort with an unchanged declared version returns serialization_retry: nothing was written, resend the call unchanged.\n\n"+
				"closeOutcome is required when transitioning to closed; repeating a close without it preserves the previous outcome.\n\n"+
				patchTextContractNote+"\n\n"+
				fieldBleedNote,
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithNumber("expectedVersion",
			mcpgo.Required(),
			mcpgo.Description("The version the agent expects to see on the issue. Every update — components and labels alone included — only proceeds when the live version matches; otherwise returns version_conflict with the live version. force=true skips the check."),
			mcpgo.Min(1),
		),
		mcpgo.WithString("title",
			mcpgo.Description("New title (1..1000 chars)."),
		),
		mcpgo.WithString("description",
			mcpgo.Description("Replace description body."),
		),
		mcpgo.WithString("type",
			mcpgo.Description("Change issue type."),
			mcpgo.Enum("bug", "feature", "task", "epic", "chore", "decision", "idea", "note"),
		),
		mcpgo.WithNumber("priority",
			mcpgo.Description("Priority 0..4."),
			mcpgo.Min(0),
			mcpgo.Max(4),
		),
		mcpgo.WithString("design",
			mcpgo.Description("Replace design notes."),
		),
		mcpgo.WithString("acceptanceCriteria",
			mcpgo.Description("Replace acceptance criteria."),
		),
		mcpgo.WithString("notes",
			mcpgo.Description("Replace notes."),
		),
		mcpgo.WithString("sourceContext",
			mcpgo.Description("Replace source-context annotation."),
		),
		mcpgo.WithString("assignee",
			mcpgo.Description("Replace assignee handle. Pass an empty string (or null) to clear. A user UUID that does not name a person who may work in the project is refused with assignee_not_allowed and nothing is written; a username or email is stored as text. A non-string value (number, array, object, boolean) is refused with validation_error, code assignee_not_string, and nothing is written."),
		),
		mcpgo.WithString("dueAt",
			mcpgo.Description("RFC3339 timestamp; replaces due date."),
		),
		mcpgo.WithString("deferUntil",
			mcpgo.Description("RFC3339 timestamp; replaces defer-until date."),
		),
		mcpgo.WithString("externalRef",
			mcpgo.Description("Replace external-system reference."),
		),
		mcpgo.WithNumber("estimatedMinutes",
			mcpgo.Description("Replace estimated-minutes (>=0)."),
			mcpgo.Min(0),
		),
		mcpgo.WithString("closeOutcome",
			mcpgo.Description(closeOutcomeParamDescription),
			mcpgo.Enum(issues.ValidCloseOutcomes()...),
		),
		mcpgo.WithString("closeTarget",
			mcpgo.Description(closeTargetParamDescription),
		),
		mcpgo.WithString("status",
			mcpgo.Description("Status transition. Routed through UpdateStatus. "+
				"`standing` promotes the issue: "+standingConceptNote+
				" The sole demote leg is back to 'open'. Rejected standing "+
				"legs — `cannot_promote_locked_to_standing`, "+
				"`cannot_work_standing_directly`, `cannot_defer_standing`, "+
				"`nested_standing_forbidden` — are structural invariants that "+
				"force does NOT bypass. "+standingGuideRef+" "+
				"`blocked` is no longer accepted — blocked-ness is derived "+
				"from the dep graph (use skein_dep_add with type='blocks' "+
				"or 'waits-for' on the issue you want labelled blocked)."),
			mcpgo.Enum("open", "in_progress", "deferred", "closed", "standing"),
		),
		mcpgo.WithString("reason",
			mcpgo.Description("Free-form reason annotation; only meaningful when `status` is set (lands in events.reason and, for closed, issues.closed_reason)."),
		),
		mcpgo.WithArray("componentIds",
			mcpgo.Description("Replace the issue's component set. UUID strings; must belong to the issue's project."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("labelsAdd",
			mcpgo.Description("Labels to add (idempotent — already-present labels are no-ops)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("labelsRemove",
			mcpgo.Description("Labels to remove (must currently be set on the issue)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithBoolean("reviewRequired",
			mcpgo.Description("Whether this issue needs the AUTHOR's review once it is "+
				"closed. Only the issue's author may set it — anyone else, admins "+
				"included, gets `review_forbidden`. It does NOT travel the ordinary "+
				"update path: send it ALONE. Combined with any other changing "+
				"argument (a field, `status`, `reason`, `componentIds`, `labelsAdd` / "+
				"`labelsRemove`) the whole call is refused with `validation_error` "+
				"and nothing is written — the flag is never applied half-way. "+
				"Flipping it moves neither `version` nor the issue's updated-at "+
				"stamp, so the acknowledgement's version is the one you already "+
				"held; `expectedVersion` is still required and still checked, as "+
				"the assertion that you are flipping the state you read. Clearing "+
				"the flag is NOT a review mark — it leaves any existing mark "+
				"untouched. There is deliberately no agent tool for the mark "+
				"itself. The acknowledgement of this branch is always the lean "+
				"one — `verbose=true` does NOT widen it to the card, because the "+
				"card would carry the mechanism's own keys. Read them with "+
				"`skein_show`."),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass lock + version + pinned predicates. Needs the key right `manage` (else `insufficient_scope`) and force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
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

	addTool(s, tool, handleUpdate(pool, issuesStore))
}

func handleUpdate(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Update(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		id := res.IssueID
		if res.ReviewFlag {
			// The review flag travels its own path; verbose does not widen
			// this branch.
			return writeIssueAck(ctx, "skein_update", userID.String(), id, res.Final, false)
		}
		verbose := req.GetBool("verbose", false)
		var extra map[string]any
		final := res.Final
		if rb := res.ReadBack; rb != nil {
			// sk-607: the change committed but the issue could not be read
			// back — acknowledge it from the committed state.
			logReadBackFallback(ctx, "skein_update", userID.String(), rb)
			final = ackSourceFromCommitted(rb.State)
			if verbose {
				extra = map[string]any{cardUnavailableKey: true}
			}
			verbose = false
		}
		return writeIssueCloseAck(ctx, "skein_update", userID.String(), id,
			final, verbose, extra, res.Link)
	}
}

// buildPatchInput translates the request args into issues.PatchIssueInput.
// Returns (input, hasAtLeastOneField, errorResult). When the second
// return is false the caller skips PatchIssue entirely — issuing a
// no-op patch would still bump version+emit an audit row, which is
// surprising for the "I only wanted to change status" path.
//
// Thin adapter around buildPatchInputFromArgs so the single-tool path
// keeps its mcpgo.CallToolRequest signature; the batch tool calls the
// args-map variant directly.
func buildPatchInput(req mcpgo.CallToolRequest, expectedVersion int) (issues.PatchIssueInput, bool, *mcpgo.CallToolResult) {
	return buildPatchInputFromArgs(req.GetArguments(), expectedVersion)
}

const patchTextContractNote = "Text patches (description, design, acceptanceCriteria, notes, externalRef, sourceContext): " +
	"omitted leaves unchanged; null or an empty string clears. Other non-string values are refused " +
	"before any write with validation_error, code text_field_not_string and field naming the argument."

func patchTextValue(field string, raw any) (string, *mcpgo.CallToolResult) {
	v0, f := issueops.PatchTextValue(field, raw)
	return v0, toolFailure(f)
}

func buildPatchInputFromArgs(args map[string]any, expectedVersion int) (issues.PatchIssueInput, bool, *mcpgo.CallToolResult) {
	v0, v1, f := issueops.BuildPatchInputFromArgs(args, expectedVersion)
	return v0, v1, toolFailure(f)
}

func readIntFromAny(v any, fallback int) int {
	return issueops.ReadIntFromAny(v, fallback)
}

func standingTransitionCode(vCode string) (string, bool) {
	return issueops.StandingTransitionCode(vCode)
}

func mapUpdateError(ctx context.Context, err error, userID uuid.UUID, issueID, closeTarget string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapUpdateError(ctx, err, userID, issueID, closeTarget))
}

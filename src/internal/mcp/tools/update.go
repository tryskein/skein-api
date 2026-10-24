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

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterUpdate wires the skein_update tool. Patches issue fields with
// optimistic concurrency, plus routes status / components / labels
// through the dedicated store methods. The tool fans out to multiple
// store calls in sequence; each store call is its own SERIALIZABLE
// transaction (the underlying stores do not expose a multi-op tx
// surface). On a mid-fan-out failure, earlier sub-operations are NOT
// rolled back — the response carries the first error and the issue
// reflects whatever sub-ops succeeded. The tool description warns the
// agent about this so retries on partial failure are deliberate.
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
				"`skein_close` (terminal close). Multi-op note: when the "+
				"call sets a mix of "+
				"fields, status, components, and labels, each sub-operation "+
				"is committed independently — a failure mid-way leaves a "+
				"partial state and you should re-fetch with skein_show "+
				"before retrying.\n\n"+
				"closeOutcome is required when transitioning to closed; repeating a close without it preserves the previous outcome.\n\n"+
				fieldBleedNote,
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithNumber("expectedVersion",
			mcpgo.Required(),
			mcpgo.Description("The version the agent expects to see on the issue. The update only proceeds when the live version matches; otherwise returns version_conflict with the live version."),
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
			mcpgo.Description("Bypass lock + version + pinned predicates. Needs force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		// ---- Required: id + expectedVersion ----
		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		id := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"demo-42\"",
				map[string]any{"id": rawID}), nil
		}

		// expectedVersion is required by spec; mcp-go's Required() does not
		// enforce the int-typed presence on numeric fields (it accepts the
		// schema definition but a missing arg defaults to 0). We still
		// validate explicitly.
		expectedVersion := req.GetInt("expectedVersion", 0)
		if expectedVersion < 1 {
			return skmcp.ToolError(ErrCodeValidationError,
				"expectedVersion is required and must be >= 1", nil), nil
		}

		force := req.GetBool("force", false)
		// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
		// per issue on its project (forceUnlockGate), not by the role.

		// sk-303.14 T007 (contract C1): the review flag does not travel the
		// ordinary update path. A mixed request is refused by FORM, before
		// any write — on MCP the form code is validation_error, the analogue
		// of REST 400. Parsing is reviewFlagArg.
		reviewFlag, reviewErr := reviewFlagArg(req.GetArguments())
		if reviewErr != nil {
			return reviewErr, nil
		}

		// ---- Parsing and checking the SHAPE — entirely before the store (sk-516) ----
		//
		// The order is aligned with close.go#handleClose. Previously this block stood
		// AFTER the visibility check, and on the very same input — a wrong
		// argument plus a nonexistent (or invisible) issue — the two
		// surfaces answered differently: skein_close about the argument,
		// skein_update about the issue. No requirement chose between
		// them, the divergence arose by itself.
		//
		// The caller paid for it: an agent that got issue_not_found where
		// its own argument is wrong goes looking for the issue instead of
		// fixing the call. The diagnostic value of the code
		// is lost exactly where it is needed.
		//
		// So EVERYTHING that needs no database stands here: componentIds,
		// labels, status, the outcome with its target, `reason` and the field edit. The mutual
		// order inside the block is kept as before — what moved was the place of the block
		// as a whole, not the priorities of the formal refusals among themselves.
		//
		// The statement about the order is update_close_outcome_test.go#
		// TestUpdateMCP_FormalRefusalPrecedesTheStore, the reference —
		// close_outcome_mcp_test.go#TestCloseMCP_FormalRefusalPrecedesTheStore.

		// ---- Pre-validate componentIds (UUID parse) ----
		// This was already done up front; we keep the same pattern for
		// labels below so validation errors never leave partial state.
		var componentIDs []uuid.UUID
		_, hasComponentIDs := req.GetArguments()["componentIds"]
		if hasComponentIDs {
			rawComponentIDs := req.GetStringSlice("componentIds", nil)
			componentIDs = make([]uuid.UUID, 0, len(rawComponentIDs))
			for _, raw := range rawComponentIDs {
				cid, perr := uuid.Parse(strings.TrimSpace(raw))
				if perr != nil {
					return skmcp.ToolError(ErrCodeValidationError,
						"componentIds[i] is not a UUID",
						map[string]any{"componentId": raw}), nil
				}
				componentIDs = append(componentIDs, cid)
			}
		}

		// ---- Pre-validate labelsAdd / labelsRemove ----
		// Validation must happen BEFORE any sub-op runs; otherwise a typo
		// in label N would commit labels 0..N-1 then fail with
		// validation_error, leaving partial state. Aligns with the
		// componentIds pattern above.
		labelsAddRaw := req.GetStringSlice("labelsAdd", nil)
		labelsAdd := make([]string, 0, len(labelsAddRaw))
		for _, l := range labelsAddRaw {
			canon, vErr := issues.ValidateLabel(l)
			if vErr != nil {
				return validationToolError(vErr), nil
			}
			labelsAdd = append(labelsAdd, canon)
		}
		labelsRemoveRaw := req.GetStringSlice("labelsRemove", nil)
		labelsRemove := make([]string, 0, len(labelsRemoveRaw))
		for _, l := range labelsRemoveRaw {
			canon, vErr := issues.ValidateLabel(l)
			if vErr != nil {
				return validationToolError(vErr), nil
			}
			labelsRemove = append(labelsRemove, canon)
		}

		// ---- Pre-validate status (if supplied) ----
		// Same rationale: catch the validation_error up front.
		statusRaw := strings.TrimSpace(req.GetString("status", ""))
		var statusCanon string
		if statusRaw != "" {
			canon, vErr := issues.ValidateStatus(statusRaw)
			if vErr != nil {
				return validationToolError(vErr), nil
			}
			statusCanon = canon
		}

		// sk-489 — the FIRST input builder of this surface. handleUpdate
		// assembles updateOneInput itself and does NOT go through buildUpdateOneInput,
		// so the parsing and checks stand both here and there.
		// The sk-489 requirement on the place holds after the block was moved: the check
		// must stand before the first step of the chain, because step 1 is a field
		// edit and commits on its own, and a refusal after it would mean
		// "priority changed, outcome not". Now it stands even earlier — before
		// the store altogether — and that only strengthens the first requirement.
		outcome, outcomeErr := parseCloseOutcomeArgs(req.GetArguments(), id, "")
		if outcomeErr != nil {
			return skmcp.ToolError(outcomeErr.Code, outcomeErr.Message, outcomeErr.Details), nil
		}
		if rej := refuseOutcomeOnNonClosingStatus(outcome, statusCanon, id); rej != nil {
			return rej, nil
		}

		// `reason` and the field edit are the last formal checks. They too
		// stand before the store: a refusal over a bad title is about the title, not
		// about the issue, which may not even exist.
		reasonArg := req.GetString("reason", "")
		if vErr := issues.ValidateProse("reason", reasonArg); vErr != nil {
			return validationToolError(vErr), nil
		}
		patchInput, hasFieldPatch, vErr := buildPatchInput(req, expectedVersion)
		if vErr != nil {
			return vErr, nil
		}

		// ---- Visibility: load issue once, ensure project visible ----
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_update",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		// sk-674.18 T066: editing is ActionWrite of US-6; changing the
		// assignee is ActionAssign too.
		updateActions := []authz.Action{authz.ActionWrite}
		if _, ok := req.GetArguments()["assignee"]; ok {
			updateActions = append(updateActions, authz.ActionAssign)
		}
		if res := issueActionGate(ctx, pool, "skein_update", "id", id, issue.ProjectID, userID, updateActions...); res != nil {
			return res, nil
		}
		if force {
			if res := forceUnlockGate(ctx, pool, "skein_update", "id", id, issue.ProjectID, userID); res != nil {
				return res, nil
			}
		}

		// ---- R2.3: lock predicate ----
		// Authoritative lock check lives in the store: every sub-op
		// invokes lockGuard inside its own SERIALIZABLE tx and surfaces
		// ErrLockedByOther{LockedBy: <current>} with the live lock-holder
		// at error time. We do NOT pre-check from the GetIssue snapshot
		// — that read is outside any tx and could return a stale
		// lockedBy that disagrees with what the store rejects against.
		// force=true passed forceUnlockGate above (K-28).
		sessionID := auth.SessionIDFrom(ctx)

		actor := IssuesAgentActor(ctx, pool, userID)

		// The flag takes its own path, outside the general update: without a version bump,
		// without a change stamp, without a fan-out (INV-2). The branch stands
		// AFTER the visibility checks and BEFORE the general edit starts: a mixed
		// request no longer gets here (reviewFlagArg rejected it before
		// the store), and a non-mixed one must not perform an edit that
		// does not exist. The shape parsing of the general edit since sk-516 stands upstream
		// and is harmless for a non-mixed call: without changing arguments it
		// collects nothing and complains about nothing.
		if reviewFlag != nil {
			final, err := issuesStore.SetReviewRequired(ctx, issues.ReviewFlagInput{
				ID:              id,
				ExpectedVersion: expectedVersion,
				Required:        *reviewFlag,
				Actor:           actor,
			})
			if err != nil {
				return mapReviewFlagError(ctx, err, userID, id), nil
			}
			slog.InfoContext(ctx, "mcp tool ok",
				"tool", "skein_update",
				"userId", userID.String(), "issueId", id,
				"field", "review_required",
				"version", final.Version,
				"latencyMs", time.Since(start).Milliseconds())
			// The acknowledgement of THIS branch is always the short form,
			// and `verbose:true` does not widen it (fix from wave A/3
			// review).
			//
			// Matrix row 120 requires literally: an agent-path write
			// acknowledgement does NOT carry the review-mechanics keys. The
			// shared verbose projection returns the card as-is, and only the
			// author can perform the operation — so it is the author's card
			// that carries those keys, and the guard's claim would have
			// held only with a "except verbose" caveat. A caveat in a
			// privacy claim is a hole someone later reads as permission.
			//
			// The full data INV-4 promises does not come through this
			// response: the author takes the card with one skein_show and
			// gets all three fields. The restriction is declared in the
			// argument description, not only here.
			return writeIssueAck(ctx, "skein_update", userID.String(), id,
				final, false /*verbose — does not widen this branch*/)
		}

		// Run the chain via the shared helper. Both surfaces share
		// runUpdateChain, so the per-row semantics stay identical; they
		// differ only in what they read back afterwards — the full card
		// here, the version alone in update_many (runUpdateChainVersion),
		// which is all a batch row carries. Everything assembled here has already been
		// parsed and checked above — before the store (sk-516).
		in := updateOneInput{
			ID:              id,
			ExpectedVersion: expectedVersion,
			HasFieldPatch:   false,
			StatusCanon:     statusCanon,
			Reason:          reasonArg,
			HasComponentIDs: hasComponentIDs,
			ComponentIDs:    componentIDs,
			LabelsAdd:       labelsAdd,
			LabelsRemove:    labelsRemove,
			CloseOutcome:    outcome.Outcome,
			CloseTarget:     outcome.Target,
			// The system role — the same visibility key as for a manual link.
			// Passed unconditionally: "only when needed" is one
			// refactoring away from a store failure.
			DependenciesCaller: &issues.DependenciesCaller{UserID: userID, Role: role},
			Force:              force,
			SessionID:          sessionID,
			Actor:              actor,
		}
		in.PatchInput = patchInput
		in.HasFieldPatch = hasFieldPatch

		verbose := req.GetBool("verbose", false)
		var extra map[string]any
		final, link, err := runUpdateForOne(ctx, issuesStore, in)
		if err != nil {
			rb, committed := asReadBackFailed(err)
			if !committed {
				return mapUpdateError(ctx, err, userID, id, outcome.Target), nil
			}
			// sk-607: every step of the chain committed; only the final
			// card read failed. Acknowledge from the last step's state —
			// under verbose too, marked cardUnavailable.
			logReadBackFallback(ctx, "skein_update", userID.String(), rb)
			final = ackSourceFromCommitted(rb.State)
			if verbose {
				extra = map[string]any{cardUnavailableKey: true}
			}
			verbose = false
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_update",
			"userId", userID.String(), "issueId", id,
			"version", final.Version,
			"latencyMs", time.Since(start).Milliseconds())
		// sk-489 (US-9) — the general update path is moved to the builder
		// of the closing surfaces. The single `reviewRequired` branch
		// above is NOT moved to it: it does not perform a close and does not move the
		// outcome, and its confirmation is also deliberately narrowed.
		return writeIssueCloseAck(ctx, "skein_update", userID.String(), id,
			final, verbose, extra, link)
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

// buildPatchInputFromArgs is the args-map variant of buildPatchInput.
// Used by skein_update (via the wrapper above) and skein_update_many
// (which only has the per-item map[string]any in hand). Behaviour is
// identical — the single-tool tests assert this.
func buildPatchInputFromArgs(args map[string]any, expectedVersion int) (issues.PatchIssueInput, bool, *mcpgo.CallToolResult) {
	input := issues.PatchIssueInput{ExpectedVersion: expectedVersion}
	has := false

	// Only set a pointer when the key is present in the JSON. We do NOT
	// fall back to a defaulted-empty value — the empty default would
	// alias "not supplied" with "set to empty" and the patch path would
	// silently clear fields the caller didn't touch.
	if v, ok := args["title"]; ok {
		s, _ := v.(string)
		s = strings.TrimSpace(s)
		canon, vErr := issues.ValidateTitle(s)
		if vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.Title = &canon
		has = true
	}
	if v, ok := args["description"]; ok {
		s, _ := v.(string)
		if vErr := issues.ValidateProse("description", s); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.Description = &s
		has = true
	}
	if v, ok := args["type"]; ok {
		s, _ := v.(string)
		canon, vErr := issues.ValidateType(s)
		if vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.Type = &canon
		has = true
	}
	if v, ok := args["priority"]; ok {
		p := readIntFromAny(v, -1)
		if vErr := issues.ValidatePriority(p); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.Priority = &p
		has = true
	}
	if v, ok := args["assignee"]; ok {
		// sk-621: null and "" both clear the assignee (the contract this
		// parser always had — null used to reach here as ""). Any other
		// non-string value is refused: read as "" it silently dropped the
		// assignee.
		s := ""
		if v != nil {
			str, isString := v.(string)
			if !isString {
				return input, false, assigneeNotStringError(v, "", nil)
			}
			s = str
		}
		input.Assignee = &s
		has = true
	}
	if v, ok := args["design"]; ok {
		s, _ := v.(string)
		if vErr := issues.ValidateProse("design", s); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.Design = &s
		has = true
	}
	if v, ok := args["acceptanceCriteria"]; ok {
		s, _ := v.(string)
		if vErr := issues.ValidateProse("acceptanceCriteria", s); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.AcceptanceCriteria = &s
		has = true
	}
	if v, ok := args["notes"]; ok {
		s, _ := v.(string)
		if vErr := issues.ValidateProse("notes", s); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.Notes = &s
		has = true
	}
	if v, ok := args["estimatedMinutes"]; ok {
		i := readIntFromAny(v, 0)
		input.EstimatedMinutes = &i
		has = true
	}
	if v, ok := args["dueAt"]; ok && v != nil {
		s, _ := v.(string)
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			return input, false, skmcp.ToolError(ErrCodeValidationError,
				"dueAt must be RFC3339 timestamp",
				map[string]any{"dueAt": s})
		}
		input.DueAt = &t
		has = true
	}
	if v, ok := args["deferUntil"]; ok && v != nil {
		s, _ := v.(string)
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			return input, false, skmcp.ToolError(ErrCodeValidationError,
				"deferUntil must be RFC3339 timestamp",
				map[string]any{"deferUntil": s})
		}
		input.DeferUntil = &t
		has = true
	}
	if v, ok := args["externalRef"]; ok {
		s, _ := v.(string)
		if vErr := issues.ValidateProse("externalRef", s); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.ExternalRef = &s
		has = true
	}
	if v, ok := args["sourceContext"]; ok {
		s, _ := v.(string)
		if vErr := issues.ValidateProse("sourceContext", s); vErr != nil {
			return input, false, validationToolError(vErr)
		}
		input.SourceContext = &s
		has = true
	}
	return input, has, nil
}

// readIntFromAny extracts an int from a JSON-decoded interface. mcp-go
// passes numbers through encoding/json, so they arrive as float64. The
// switch covers the alternate forms callers may construct directly in
// tests.
func readIntFromAny(v any, fallback int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return fallback
}

// mapUpdateError translates the typed errors emitted by issues.Store
// across PatchIssue / UpdateStatus / ReplaceComponents / AddLabel /
// RemoveLabel / Close into stable MCP error codes.
// standingTransitionCode maps a ValidationError.Code emitted by the
// sk-118 (F2c) standing transition guards onto its top-level MCP envelope
// code. Returns (code, true) for the three standing guard codes; ("", false)
// for any other validation code (which stays under validation_error). Shared
// by mapUpdateError (single tool) and classifyUpdateError (batch) so both
// surfaces promote the same codes identically.
func standingTransitionCode(vCode string) (string, bool) {
	switch vCode {
	case ErrCodeCannotPromoteLockedToStanding:
		return ErrCodeCannotPromoteLockedToStanding, true
	case ErrCodeCannotWorkStandingDirectly:
		return ErrCodeCannotWorkStandingDirectly, true
	case ErrCodeCannotDeferStanding:
		return ErrCodeCannotDeferStanding, true
	case ErrCodeNestedStandingForbidden:
		return ErrCodeNestedStandingForbidden, true
	default:
		return "", false
	}
}

func mapUpdateError(ctx context.Context, err error, userID uuid.UUID, issueID, closeTarget string) *mcpgo.CallToolResult {
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		return assigneeNotAllowedResult(map[string]any{"id": issueID})
	}
	if cls := classifyCloseTargetError(err, closeTarget); cls != nil {
		return skmcp.ToolError(cls.Code, cls.Message, cls.Details)
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		// sk-118 (F2c): the standing transition guards are *ValidationError
		// values but carry actionable, branch-on-able codes — promote them
		// to the top-level envelope `error` instead of the generic
		// validation_error so callers don't have to dig into detail.code.
		if code, ok := standingTransitionCode(vErr.Code); ok {
			return skmcp.ToolError(code, vErr.Message, map[string]any{"id": issueID})
		}
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var labelErr *errLabelNotPresent
	if errors.As(err, &labelErr) {
		return skmcp.ToolError(ErrCodeValidationError,
			"label is not currently set on this issue",
			map[string]any{"label": labelErr.Label})
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return skmcp.ToolError(ErrCodeVersionConflict,
			"the issue was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	var lockErr *issues.ErrLockedByOther
	if errors.As(err, &lockErr) {
		return skmcp.ToolError(ErrCodeLockedByOther,
			"issue is locked by another session",
			map[string]any{"id": issueID, "lockedBy": lockErr.LockedBy})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": issueID})
	}
	if errors.Is(err, issues.ErrPinnedNoForce) {
		return skmcp.ToolError(ErrCodePinnedNoForce,
			"pinned issues require the dedicated unpin tool to be closed", nil)
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		return skmcp.ToolError(ErrCodeComponentsWrongProject,
			"some components do not belong to this project",
			map[string]any{"componentIds": cerr.ComponentIDs})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_update",
		"reason", "store call failed",
		"userId", userID.String(), "issueId", issueID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "update failed", nil)
}

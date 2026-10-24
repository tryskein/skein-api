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
	"skein-api/internal/projects"
)

// RegisterUpdateMany wires the skein_update_many tool. Each items[i]
// is a fully-independent partial patch — same shape as skein_update,
// minus the top-level id/expectedVersion (those go inside each item).
// The contract is "N independent patches in one round-trip", not
// "same patch applied to N issues" — agents constructing a mass-edit
// build the same value into each item.
//
// Per-row behaviour mirrors skein_update exactly: the multi-step chain
// (PatchIssue → UpdateStatus/Close → ReplaceComponents → AddLabel /
// RemoveLabel) runs sub-tx by sub-tx; mid-chain failure leaves
// THAT row in partial state but the rest of the batch keeps running.
// See plan §C-2 / risks §H.
//
// `force=true` is admin-only and applied uniformly across the batch.
func RegisterUpdateMany(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_update_many",
		mcpgo.WithDescription(
			"Patch many issues in one round-trip. Each items[i] carries "+
				"its own id + expectedVersion + the same partialPatch "+
				"shape that skein_update accepts (title / description / "+
				"type / priority / assignee / componentIds / labelsAdd / "+
				"labelsRemove / status / reason / dueAt / deferUntil / "+
				"design / acceptanceCriteria / notes / externalRef / "+
				"sourceContext / estimatedMinutes). The N patches are "+
				"independent — passing the same value in every "+
				"item gives mass-edit semantics. "+
				"Each row runs the full skein_update chain in its own "+
				"SERIALIZABLE sub-transactions per step; mid-chain "+
				"failure on one row leaves THAT row in partial state "+
				"(earlier sub-ops are NOT rolled back — this matches "+
				"single-tool behaviour) but does not affect the rest of "+
				"the batch. Inspect results[i].ok / results[i].error.code "+
				"per row. A row stopped by batch_deadline_exceeded / "+
				"request_cancelled while in progress may have committed "+
				"earlier steps: re-read it and resend only what is missing, "+
				"with the live version. "+
				"closeOutcome is required when transitioning to closed; repeating a close without it preserves the previous outcome. "+
				"No per-row `closeTarget`: close an OPEN issue as duplicate / "+
				"superseded with skein_close / skein_update. "+
				"Caps at 100 items per call. "+
				"`force=true` requires admin and applies uniformly to "+
				"every row. "+
				"`reviewRequired` is NOT accepted here: the review flag "+
				"travels its own path (it moves neither version nor "+
				"updated_at, so a batch row could not report it). A row "+
				"carrying it is rejected with validation_error and writes "+
				"nothing — use skein_update, one call per issue.",
		),
		mcpgo.WithArray("items",
			mcpgo.Required(),
			mcpgo.Description("Per-row partial patches. Min 1, max 100. Order is preserved in results[]."),
			mcpgo.MinItems(1),
			mcpgo.MaxItems(batchMaxItems),
			// We deliberately do NOT enumerate every patch field in the
			// JSON Schema for items[] here. The set is large (16+
			// fields) and changes whenever skein_update grows new
			// fields; duplicating it would drift. Agents discover the
			// per-item shape by reading skein_update's tool schema and
			// using the same keys at the items[i] level. The handler
			// validates strictly per-row and returns
			// validation_error with the offending field on bad input.
			mcpgo.Items(map[string]any{
				"type":                 "object",
				"additionalProperties": true,
				"required":             []string{"id", "expectedVersion"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Full hierarchical issue id.",
					},
					"expectedVersion": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "The version the agent observed.",
					},
					"closeOutcome": map[string]any{
						"type":        "string",
						"enum":        issues.ValidCloseOutcomes(),
						"description": closeOutcomeParamDescription,
					},
				},
			}),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass lock + version + pinned predicates across the entire batch. Needs force_unlock on each row's project (project owner, space owner, instance admin); a row without it fails with `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleUpdateMany(pool, issuesStore))
}

func handleUpdateMany(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		force := req.GetBool("force", false)
		// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
		// per issue on its project (forceUnlockGate), not by the role.

		rawItems, ok := req.GetArguments()["items"]
		if !ok || rawItems == nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"items is required and must be a non-empty array", nil), nil
		}
		rawSlice, ok := rawItems.([]any)
		if !ok {
			return skmcp.ToolError(ErrCodeValidationError,
				"items must be an array of objects", nil), nil
		}
		if len(rawSlice) == 0 {
			return skmcp.ToolError(ErrCodeValidationError,
				"items must contain at least 1 entry", nil), nil
		}
		if len(rawSlice) > batchMaxItems {
			return skmcp.ToolError(ErrCodeValidationError,
				"items exceeds the per-call cap",
				map[string]any{"max": batchMaxItems, "got": len(rawSlice)}), nil
		}

		sessionID := auth.SessionIDFrom(ctx)
		actor := IssuesAgentActor(ctx, pool, userID)

		// The row work runs under our own deadline so the response is
		// written while the caller is still there to read it — see
		// batchLoopDeadline. Logging keeps the parent context.
		loopCtx, cancelLoop := batchLoopContext(ctx)
		defer cancelLoop()

		results := make([]batchResultRow, len(rawSlice))
		var succeeded, failed int

		// Visibility is a property of the PROJECT, and a batch is
		// overwhelmingly one project's worth of issues, so the same answer
		// was being fetched once per row. Cached for the call only — a
		// membership change mid-batch is not a case worth a round-trip per
		// row, and every row of this batch was authorised in the same
		// instant either way.
		//
		// ONLY definitive answers are cached: visible, and
		// ErrProjectNotVisible. A transient failure — a pool checkout that
		// blipped, a scan that lost its connection — is not an answer about
		// the project, and caching it would replay one row's bad luck as
		// internal_error onto every later row of that project without ever
		// asking again.
		visible := make(map[uuid.UUID]error)
		// levels holds the caller's level per visible project, so each row
		// asks the action table of US-6 for its own actions without another
		// query (sk-674.18 T066).
		levels := make(map[uuid.UUID]projects.AccessLevel)

		for i, raw := range rawSlice {
			// Stopping early: mark this and all remaining rows with the
			// reason, then stop — same caller-visible result without extra
			// iterations. The rows above are already committed and stay so.
			if code, message, stop := batchStopReason(ctx, loopCtx); stop {
				for j := i; j < len(rawSlice); j++ {
					// Carry the id whenever the row parses far enough to
					// have one: "retry the unrun rows" is easier to act on
					// than "zip these by input order", even though the
					// order is guaranteed.
					unrunID := ""
					if obj, okObj := rawSlice[j].(map[string]any); okObj {
						unrunID, _ = obj["id"].(string)
					}
					results[j] = batchResultRow{
						ID: unrunID,
						OK: false,
						Error: &batchErrorDTO{
							Code:    code,
							Message: message,
						},
					}
					failed++
				}
				slog.WarnContext(ctx, "mcp batch stopped early",
					"tool", "skein_update_many", "code", code,
					"appliedRows", succeeded, "unrunRows", len(rawSlice)-i,
					"userId", userID.String())
				break
			}
			obj, ok := raw.(map[string]any)
			if !ok {
				results[i] = batchResultRow{
					OK: false,
					Error: &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: "items[i] must be an object",
					},
				}
				failed++
				continue
			}

			rawID, _ := obj["id"].(string)
			id := strings.TrimSpace(rawID)
			if !looksLikeHierarchicalID(id) {
				results[i] = batchResultRow{
					ID: rawID,
					OK: false,
					Error: &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: "items[i].id must be a full hierarchical id like \"demo-42\"",
						Details: map[string]any{"id": rawID},
					},
				}
				failed++
				continue
			}

			expectedVersion, vErr := readPositiveInt(obj, "expectedVersion")
			if vErr != nil {
				results[i] = batchResultRow{ID: id, OK: false, Error: vErr}
				failed++
				continue
			}

			// Build the run-input from this item's args. Reuses the
			// same parser the single-tool handler does; per-row
			// validation_error is captured as an in-place row failure
			// rather than aborting the batch.
			// The system-role carrier is ALWAYS passed, even though this surface
			// rejects the target: card T009
			// requires "both builders fill it", and the reason is in the
			// doc block of update_run.go#updateOneInput.
			in, parseRes := buildUpdateOneInput(obj, pool, ctx, id, expectedVersion,
				force, sessionID, actor,
				&issues.DependenciesCaller{UserID: userID, Role: role})
			if parseRes != nil {
				results[i] = batchResultRow{
					ID:    id,
					OK:    false,
					Error: errorDTOFromCallResult(parseRes),
				}
				failed++
				continue
			}

			// Visibility check — read the owning project, ensure it is
			// visible. Per-row, AFTER the row shape is parsed — and since sk-516 this
			// is indeed the same order as in the single
			// update.go#handleUpdate: before that change the single one went to the
			// database FIRST, and the claim that the orders match was wrong. The
			// proof of this half is
			// update_close_outcome_test.go#
			// TestUpdateManyMCP_FormalRefusalPrecedesTheStore.
			// Only the project id is needed here, and reading it costs one
			// query where loading the whole issue costs six.
			projectID, err := issuesStore.GetIssueProjectID(loopCtx, id)
			if err != nil {
				if errors.Is(err, issues.ErrNotFound) {
					results[i] = batchResultRow{
						ID: id,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeIssueNotFound,
							Message: "issue does not exist or is not visible",
							Details: map[string]any{"id": id},
						},
					}
				} else if stopErr := updateRowContextStop(ctx, loopCtx, err, id); stopErr != nil {
					results[i] = batchResultRow{ID: id, OK: false, Error: stopErr}
				} else {
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_update_many",
						"reason", "load issue failed",
						"userId", userID.String(), "issueId", id, "error", err.Error())
					results[i] = batchResultRow{
						ID: id,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeInternalError,
							Message: "failed to load issue",
						},
					}
				}
				failed++
				continue
			}
			visErr, checked := visible[projectID]
			if !checked {
				var level projects.AccessLevel
				level, visErr = projects.EnsureProjectVisible(loopCtx, pool, projectID, userID)
				if visErr == nil || errors.Is(visErr, projects.ErrProjectNotVisible) {
					visible[projectID] = visErr
					levels[projectID] = level
				}
			}
			if visErr == nil {
				// Editing is ActionWrite of US-6; a row changing the
				// assignee is ActionAssign too.
				rowActions := []authz.Action{authz.ActionWrite}
				if _, ok := obj["assignee"]; ok {
					rowActions = append(rowActions, authz.ActionAssign)
				}
				if force && projects.RequireAction(levels[projectID], rowActions...) == nil &&
					projects.RequireAction(levels[projectID], authz.ActionForceUnlock) != nil {
					// sk-674.18 T034 (K-28): force on this row's project.
					results[i] = batchResultRow{
						ID: id,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeForceUnlockForbidden,
							Message: msgForceUnlockForbidden,
							Details: map[string]any{"id": id},
						},
					}
					failed++
					continue
				}
				if projects.RequireAction(levels[projectID], rowActions...) != nil {
					results[i] = batchResultRow{
						ID: id,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeForbidden,
							Message: msgActionForbidden,
							Details: map[string]any{"id": id},
						},
					}
					failed++
					continue
				}
			}
			if visErr != nil {
				if errors.Is(visErr, projects.ErrProjectNotVisible) {
					results[i] = batchResultRow{
						ID: id,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeIssueNotFound,
							Message: "issue does not exist or is not visible",
							Details: map[string]any{"id": id},
						},
					}
				} else if stopErr := updateRowContextStop(ctx, loopCtx, visErr, id); stopErr != nil {
					results[i] = batchResultRow{ID: id, OK: false, Error: stopErr}
				} else {
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_update_many",
						"reason", "visibility check failed",
						"userId", userID.String(), "issueId", id, "error", visErr.Error())
					results[i] = batchResultRow{
						ID: id,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeInternalError,
							Message: "visibility check failed",
						},
					}
				}
				failed++
				continue
			}

			version, err := runUpdateChainVersion(loopCtx, issuesStore, in)
			if err != nil {
				dto := updateRowContextStop(ctx, loopCtx, err, id)
				if dto == nil {
					dto = classifyUpdateError(ctx, err, "skein_update_many", userID.String(), id)
				}
				results[i] = batchResultRow{ID: id, OK: false, Error: dto}
				failed++
				continue
			}
			results[i] = batchResultRow{
				ID:      id,
				OK:      true,
				Version: version,
			}
			succeeded++
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_update_many",
			"userId", userID.String(),
			"requested", len(rawSlice),
			"succeeded", succeeded,
			"failed", failed,
			"force", force,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(batchResponse{
			Results: results,
			Summary: batchSummary{
				Requested: len(rawSlice),
				Succeeded: succeeded,
				Failed:    failed,
			},
		})
	}
}

// updateRowContextStop renders a skein_update_many row whose work was cut by
// a context — the batch loop's own deadline (batchLoopDeadline) or the
// caller going away — while the row was in progress, or nil when err is not
// that (readback.go#contextStop decides which code). Before sk-647.2 such a
// row answered internal_error, the code of a row that broke while being
// written, although nothing broke: the server stopped on its own clock.
//
// The message is NOT contextStop's «did not report a commit, resend with the
// same expectedVersion»: the update chain (update_run.go#runUpdateForOne)
// commits step by step, so a cut in the middle can leave the steps before it
// applied. The row therefore says so and asks for a re-read first; the
// guide's rule for these codes — compare the live state, resend only what is
// missing — covers the rest.
func updateRowContextStop(parent, loop context.Context, err error, issueID string) *batchErrorDTO {
	code, _, ok := contextStop(parent, loop, err)
	if !ok {
		return nil
	}
	message := "the request ended while this row was in progress; steps of its update may already " +
		"be applied — re-read the issue before repeating it"
	if code == ErrCodeBatchDeadlineExceeded {
		message = "the batch reached its server-side deadline while this row was in progress; steps of " +
			"its update may already be applied — re-read the issue, then resend what is still missing"
	}
	return &batchErrorDTO{
		Code:    code,
		Message: message,
		Details: map[string]any{"id": issueID},
	}
}

// classifyUpdateError mirrors mapUpdateError but returns a
// batchErrorDTO directly (no CallToolResult round-trip) so the batch
// tool keeps the per-row error envelope identical to the single tool's
// output without re-marshalling.
func classifyUpdateError(ctx context.Context, err error, tool, userID, issueID string) *batchErrorDTO {
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		return &batchErrorDTO{
			Code:    ErrCodeAssigneeNotAllowed,
			Message: "the assignee chosen by id may not work in this project; nothing was changed",
			Details: map[string]any{"id": issueID},
		}
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		// sk-118 (F2c): promote the standing transition guard codes to the
		// top-level batch row `code` (parity with mapUpdateError).
		if code, ok := standingTransitionCode(vErr.Code); ok {
			return &batchErrorDTO{
				Code:    code,
				Message: vErr.Message,
				Details: map[string]any{"id": issueID},
			}
		}
		return &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: vErr.Message,
			Details: map[string]any{"code": vErr.Code},
		}
	}
	var labelErr *errLabelNotPresent
	if errors.As(err, &labelErr) {
		return &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "label is not currently set on this issue",
			Details: map[string]any{"label": labelErr.Label},
		}
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return &batchErrorDTO{
			Code:    ErrCodeVersionConflict,
			Message: "the issue was modified by another writer",
			Details: map[string]any{"currentVersion": verr.CurrentVersion},
		}
	}
	var lockErr *issues.ErrLockedByOther
	if errors.As(err, &lockErr) {
		return &batchErrorDTO{
			Code:    ErrCodeLockedByOther,
			Message: "issue is locked by another session",
			Details: map[string]any{"id": issueID, "lockedBy": lockErr.LockedBy},
		}
	}
	if errors.Is(err, issues.ErrNotFound) {
		return &batchErrorDTO{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"id": issueID},
		}
	}
	if errors.Is(err, issues.ErrPinnedNoForce) {
		return &batchErrorDTO{
			Code:    ErrCodePinnedNoForce,
			Message: "pinned issues require the dedicated unpin tool to be closed",
		}
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		return &batchErrorDTO{
			Code:    ErrCodeComponentsWrongProject,
			Message: "some components do not belong to this project",
			Details: map[string]any{"componentIds": cerr.ComponentIDs},
		}
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", tool,
		"reason", "store call failed",
		"userId", userID, "issueId", issueID, "error", err.Error())
	return &batchErrorDTO{
		Code:    ErrCodeInternalError,
		Message: "update failed",
	}
}

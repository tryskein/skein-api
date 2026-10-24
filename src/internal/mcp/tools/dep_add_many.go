package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterDepAddMany wires the skein_dep_add_many tool. Each items[i]
// is a {fromId, toId, type, gate?} edge — same shape as skein_dep_add.
// Per-row dispatch via dependencies.Store.Create runs in its own
// SERIALIZABLE tx; the cycle check therefore sees the live graph
// post-prior-inserts state inside the same batch — inserting [A→B,
// B→A] of type=blocks in one call leaves A→B persisted and surfaces
// dependency_cycle on B→A (as expected).
//
// There is no expectedVersion (deps are write-once-by-PK; the unique
// constraint is the natural concurrency guard).
//
// `force` is NOT supported — there is no admin-bypass path on dep
// creation in the single-tool either.
func RegisterDepAddMany(s *server.MCPServer, pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_dep_add_many",
		mcpgo.WithDescription(
			"Add many typed dependency edges in one round-trip. Each item "+
				"is independent — direction + type semantics match "+
				"skein_dep_add. Cycle detection runs per-edge against the "+
				"live graph state, so a batch of [A→B, B→A] of type=blocks "+
				"persists A→B and fails B→A with dependency_cycle: order "+
				"in items[] matters. Caps at 100 edges per call; result "+
				"rows preserve input order. Per-row error codes: "+
				"validation_error, issue_not_found (also for a target of "+
				"another space or of a project you cannot see), "+
				"dependency_already_exists, "+
				"dependency_cycle, parent_already_set, serialization_retry "+
				"(retryable), dependency_target_not_visible_yet "+
				"(retryable), batch_deadline_exceeded / request_cancelled "+
				"(the row did not report a commit — resend it; an edge "+
				"that landed answers dependency_already_exists), "+
				"internal_error.\n\n"+
				depHierarchyNote,
		),
		mcpgo.WithArray("items",
			mcpgo.Required(),
			mcpgo.Description("Per-row dependency edges. Min 1, max 100."),
			mcpgo.MinItems(1),
			mcpgo.MaxItems(batchMaxItems),
			mcpgo.Items(map[string]any{
				"type":     "object",
				"required": []string{"fromId", "toId", "type"},
				"properties": map[string]any{
					"fromId": map[string]any{
						"type":        "string",
						"description": "Full hierarchical id of the FROM-side issue.",
					},
					"toId": map[string]any{
						"type":        "string",
						"description": "Full hierarchical id of the TO-side issue.",
					},
					"type": map[string]any{
						"type":        "string",
						"description": depTypeParamDescription,
						"enum": []string{
							dependencies.DepBlocks,
							dependencies.DepRelated,
							dependencies.DepDiscoveredFrom,
							dependencies.DepSupersedes,
							dependencies.DepDuplicates,
							dependencies.DepWaitsFor,
							dependencies.DepParentChild,
						},
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
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleDepAddMany(pool, depsStore, issuesStore))
}

// depAddManyResultRow extends batchResultRow with the resolved
// dependency DTO on success — there is no version on dependency
// rows so the bare batchResultRow's Version field is unused.
type depAddManyResultRow struct {
	FromID string         `json:"fromId"`
	ToID   string         `json:"toId"`
	Type   string         `json:"type,omitempty"`
	OK     bool           `json:"ok"`
	Dep    *depDTO        `json:"dependency,omitempty"`
	Error  *batchErrorDTO `json:"error,omitempty"`
}

// depAddManyResponse mirrors batchResponse but with the row shape
// above — keeps the on-the-wire shape obvious without forcing the
// agent to map (id, ?) onto (fromId, toId) when reading.
type depAddManyResponse struct {
	Results []depAddManyResultRow `json:"results"`
	Summary batchSummary          `json:"summary"`
}

func handleDepAddMany(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

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

		actor := DependenciesAgentActor(ctx, pool, userID)

		// See batchLoopDeadline: the loop stops on our clock so the caller
		// learns which edges landed instead of losing the response.
		loopCtx, cancelLoop := batchLoopContext(ctx)
		defer cancelLoop()
		caller := dependencies.CallerAccess{UserID: userID, Role: role}

		results := make([]depAddManyResultRow, len(rawSlice))
		var succeeded, failed int

		for i, raw := range rawSlice {
			if code, message, stop := batchStopReason(ctx, loopCtx); stop {
				// Fill every remaining slot and stop, rather than walking
				// the rest of the batch against a dead context one row at a
				// time. Same shape as skein_update_many and
				// skein_close_many: the rows above are committed and stay
				// so, and the WARN line is how an incident like this is
				// found afterwards.
				for j := i; j < len(rawSlice); j++ {
					unrunFrom, unrunTo := "", ""
					if obj, okObj := rawSlice[j].(map[string]any); okObj {
						unrunFrom, _ = obj["fromId"].(string)
						unrunTo, _ = obj["toId"].(string)
					}
					results[j] = depAddManyResultRow{
						FromID: unrunFrom,
						ToID:   unrunTo,
						OK:     false,
						Error: &batchErrorDTO{
							Code:    code,
							Message: message,
						},
					}
					failed++
				}
				slog.WarnContext(ctx, "mcp batch stopped early",
					"tool", "skein_dep_add_many", "code", code,
					"appliedRows", succeeded, "unrunRows", len(rawSlice)-i,
					"userId", userID.String())
				break
			}
			obj, ok := raw.(map[string]any)
			if !ok {
				results[i] = depAddManyResultRow{
					OK: false,
					Error: &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: "items[i] must be an object",
					},
				}
				failed++
				continue
			}

			rawFromID, _ := obj["fromId"].(string)
			fromID := strings.TrimSpace(rawFromID)
			if !looksLikeHierarchicalID(fromID) {
				results[i] = depAddManyResultRow{
					FromID: rawFromID,
					OK:     false,
					Error: &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: "items[i].fromId must be a full hierarchical id",
						Details: map[string]any{"fromId": rawFromID},
					},
				}
				failed++
				continue
			}
			rawToID, _ := obj["toId"].(string)
			toID := strings.TrimSpace(rawToID)
			if !looksLikeHierarchicalID(toID) {
				results[i] = depAddManyResultRow{
					FromID: fromID,
					ToID:   rawToID,
					OK:     false,
					Error: &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: "items[i].toId must be a full hierarchical id",
						Details: map[string]any{"toId": rawToID},
					},
				}
				failed++
				continue
			}
			rawType, _ := obj["type"].(string)
			depType, vErr := dependencies.ValidateType(rawType)
			if vErr != nil {
				results[i] = depAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					OK:     false,
					Error:  depErrorDTOFromValidation(vErr),
				}
				failed++
				continue
			}

			var gateArg *string
			if g, ok := obj["gate"].(string); ok {
				if trimmed := strings.TrimSpace(g); trimmed != "" {
					gateArg = &trimmed
				}
			}
			if _, vErr := dependencies.ValidateGate(depType, gateArg); vErr != nil {
				results[i] = depAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error:  depErrorDTOFromValidation(vErr),
				}
				failed++
				continue
			}

			// Pre-check FROM-side visibility — same shape as
			// skein_dep_add. Surfaces issue_not_found rather than the
			// generic "from-issue not found".
			fromIssue, err := issuesStore.GetIssue(loopCtx, fromID)
			if err != nil {
				if errors.Is(err, issues.ErrNotFound) {
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error: &batchErrorDTO{
							Code:    ErrCodeIssueNotFound,
							Message: "issue does not exist or is not visible",
							Details: map[string]any{"fromId": fromID},
						},
					}
				} else if stopErr := depRowContextStop(ctx, loopCtx, err, fromID, toID, depType); stopErr != nil {
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error:  stopErr,
					}
				} else {
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_dep_add_many",
						"reason", "load fromId failed",
						"userId", userID.String(), "fromId", fromID, "error", err.Error())
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error: &batchErrorDTO{
							Code:    ErrCodeInternalError,
							Message: "failed to load fromId",
						},
					}
				}
				failed++
				continue
			}
			// sk-674.18 T066: linking is ActionWrite of US-6 on the FROM side.
			if _, vErr := projects.EnsureProjectAction(loopCtx, pool, fromIssue.ProjectID, userID, authz.ActionWrite); vErr != nil {
				if errors.Is(vErr, projects.ErrProjectNotVisible) {
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error: &batchErrorDTO{
							Code:    ErrCodeIssueNotFound,
							Message: "issue does not exist or is not visible",
							Details: map[string]any{"fromId": fromID},
						},
					}
				} else if errors.Is(vErr, projects.ErrProjectActionForbidden) {
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error: &batchErrorDTO{
							Code:    ErrCodeForbidden,
							Message: msgActionForbidden,
							Details: map[string]any{"fromId": fromID},
						},
					}
				} else if stopErr := depRowContextStop(ctx, loopCtx, vErr, fromID, toID, depType); stopErr != nil {
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error:  stopErr,
					}
				} else {
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_dep_add_many",
						"reason", "fromId visibility check failed",
						"userId", userID.String(), "fromId", fromID, "error", vErr.Error())
					results[i] = depAddManyResultRow{
						FromID: fromID,
						ToID:   toID,
						Type:   depType,
						OK:     false,
						Error: &batchErrorDTO{
							Code:    ErrCodeInternalError,
							Message: "visibility check failed",
						},
					}
				}
				failed++
				continue
			}

			dto, err := depsStore.Create(loopCtx, fromID, dependencies.CreateDepInput{
				ToID: toID,
				Type: depType,
				Gate: gateArg,
			}, caller, actor)
			if err != nil {
				results[i] = depAddManyResultRow{
					FromID: fromID,
					ToID:   toID,
					Type:   depType,
					OK:     false,
					Error:  classifyDepAddError(ctx, loopCtx, err, userID.String(), fromID, toID, depType),
				}
				failed++
				continue
			}

			var gateOut *string
			if g, ok := dto.Metadata["gate"].(string); ok && g != "" {
				gg := g
				gateOut = &gg
			}
			results[i] = depAddManyResultRow{
				FromID: fromID,
				ToID:   toID,
				Type:   depType,
				OK:     true,
				Dep: &depDTO{
					From:      dto.FromID,
					To:        dto.ToID,
					Type:      dto.Type,
					Gate:      gateOut,
					Metadata:  dto.Metadata,
					Version:   dto.Version,
					CreatedAt: dto.CreatedAt,
				},
			}
			succeeded++
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_dep_add_many",
			"userId", userID.String(),
			"requested", len(rawSlice),
			"succeeded", succeeded,
			"failed", failed,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(depAddManyResponse{
			Results: results,
			Summary: batchSummary{
				Requested: len(rawSlice),
				Succeeded: succeeded,
				Failed:    failed,
			},
		})
	}
}

// depErrorDTOFromValidation re-uses the dependencies.ValidationError
// → batchErrorDTO mapping the single tool surfaces.
func depErrorDTOFromValidation(err error) *batchErrorDTO {
	if ve, ok := dependencies.IsValidationError(err); ok {
		return &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: ve.Message,
			Details: map[string]any{"code": ve.Code},
		}
	}
	return &batchErrorDTO{
		Code:    ErrCodeValidationError,
		Message: err.Error(),
	}
}

// depRowContextStop renders a skein_dep_add_many row whose work was cut by
// a context — the batch loop's own deadline (batchLoopDeadline) or the
// caller going away — while the row was in progress, or nil when err is not
// that (readback.go#contextStop decides). The row then answers the code the
// caller already resends on, batch_deadline_exceeded / request_cancelled,
// and not internal_error, which is what production returned when the
// deadline fired inside Store.Create (sk-647.2; the close_many twin is
// close_errors.go#classifyCloseRowError, sk-607).
//
// The message is the edge's own, not contextStop's: a dependency has no
// expectedVersion, and none is needed to resend safely — the row is one
// transaction, and an edge that did commit before the cut answers the
// resend with dependency_already_exists.
func depRowContextStop(parent, loop context.Context, err error, fromID, toID, depType string) *batchErrorDTO {
	code, _, ok := contextStop(parent, loop, err)
	if !ok {
		return nil
	}
	message := "the request ended while this edge was in progress; it did not report a commit — " +
		"resend it if still wanted: an edge that did land answers dependency_already_exists"
	if code == ErrCodeBatchDeadlineExceeded {
		message = "the batch reached its server-side deadline while this edge was in progress; it did not report a commit — " +
			"resend it: an edge that did land answers dependency_already_exists"
	}
	return &batchErrorDTO{
		Code:    code,
		Message: message,
		Details: map[string]any{"fromId": fromID, "toId": toID, "type": depType},
	}
}

// classifyDepAddError mirrors mapDepAddError but returns a
// batchErrorDTO directly. Same closed set of error codes; preserves
// every structured detail (path[] on cycle, fromId/toId/type on
// already-exists, etc.) the agent uses to retry. A row cut by a context
// (loopCtx's deadline or the caller leaving) is depRowContextStop's.
func classifyDepAddError(ctx, loopCtx context.Context, err error, userID, fromID, toID, depType string) *batchErrorDTO {
	if stopErr := depRowContextStop(ctx, loopCtx, err, fromID, toID, depType); stopErr != nil {
		return stopErr
	}
	if vErr, ok := dependencies.IsValidationError(err); ok {
		// sk-118 (F2e): the nested-standing ban (parent-child edge where both
		// ends are standing) is a *ValidationError but carries an actionable,
		// branch-on-able code. Promote it to the row-level code so this batch
		// surface matches the single-row skein_dep_add / skein_reparent
		// contract (which promote nested_standing_forbidden to the envelope
		// `error` rather than burying it under validation_error). Preserve the
		// fromId/toId/type details so the agent can pinpoint the failing row.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return &batchErrorDTO{
				Code:    ErrCodeNestedStandingForbidden,
				Message: vErr.Message,
				Details: map[string]any{
					"code":   vErr.Code,
					"fromId": fromID,
					"toId":   toID,
					"type":   depType,
				},
			}
		}
		// sk-118 (F2f): blocker-target standing ban — same row-level promotion so
		// the batch surface matches the single-row skein_dep_add contract.
		if vErr.Code == ErrCodeBlockerTargetStanding {
			return &batchErrorDTO{
				Code:    ErrCodeBlockerTargetStanding,
				Message: vErr.Message,
				Details: map[string]any{
					"code":   vErr.Code,
					"fromId": fromID,
					"toId":   toID,
					"type":   depType,
				},
			}
		}
		return &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: vErr.Message,
			Details: map[string]any{"code": vErr.Code},
		}
	}
	// Every remaining store error renders identically on both dep-add
	// surfaces (same code, message and details) — only the envelope
	// differs. classifyDepStoreError owns that shared table; the
	// retryable codes (serialization_retry,
	// dependency_target_not_visible_yet) come out of it unchanged so the
	// agent still re-submits the row.
	if cls := classifyDepStoreError(err, fromID, toID, depType); cls != nil {
		return &batchErrorDTO{
			Code:    cls.Code,
			Message: cls.Message,
			Details: cls.Details,
		}
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_dep_add_many",
		"reason", "store create failed",
		"userId", userID,
		"fromId", fromID, "toId", toID, "type", depType, "error", err.Error())
	return &batchErrorDTO{
		Code:    ErrCodeInternalError,
		Message: "failed to create dependency",
	}
}

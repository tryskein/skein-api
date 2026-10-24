package tools

import (
	"context"
	"encoding/json"
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

// jsonUnmarshalString is a thin alias around json.Unmarshal used by
// errorDTOFromCallResult — kept as a package-level binding so future
// tests can stub it without poking encoding/json directly.
var jsonUnmarshalString = func(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// batchMaxItems caps the per-call batch size for every R5.3 batch tool.
// Matches the REST BulkHandler bulkMaxIDs (=100) so the contract is the
// same across MCP and REST surfaces. Larger jobs should be a background
// task, not a synchronous round-trip.
const batchMaxItems = 100

// batchResultRow is the per-row entry in any batch tool's `results`
// array. ok=true success carries a Version (post-state); ok=false
// carries a structured Error matching the single-tool error envelope.
type batchResultRow struct {
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Version int            `json:"version,omitempty"`
	Error   *batchErrorDTO `json:"error,omitempty"`
}

// batchErrorDTO mirrors the body skmcp.ToolError emits for the
// single-issue tools, minus the top-level wrapping.  Code is the
// stable error string ("version_conflict" etc.); Message is the
// human-readable detail; the rest of the structured fields land in
// Details for the agent to inspect.
type batchErrorDTO struct {
	Code    string         `json:"code"`
	Message string         `json:"message,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// batchSummary is the per-call rollup. Lets the agent log "X/Y
// succeeded" without re-iterating results[].
type batchSummary struct {
	Requested int `json:"requested"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// batchResponse is the full MCP envelope for every batch tool. Agents
// always receive HTTP 200 — partial-success is encoded in the body via
// per-row OK/Error pairs.
type batchResponse struct {
	Results []batchResultRow `json:"results"`
	Summary batchSummary     `json:"summary"`
}

// closeManyResultRow is skein_close_many's OWN per-row shape: the
// reference row's four keys plus the close outcome and the close-target
// link state (sk-489, US-9).
//
// # Why a new type instead of two fields on batchResultRow
//
// US-5 names the group close/update response THE reference form and
// forbids repairing it. Widening
// batchResultRow would change the wire of skein_update_many and
// skein_dep_add_many too — tools that close nothing and for which
// `closeOutcome` would be a key about somebody else's operation.
//
// So this is a DELIBERATE contract extension of ONE batch tool, made by
// giving that tool its own pair, and the reference pair stays untouched
// and still guarded. US-9 requires the outcome in the confirmations of
// the three closing surfaces; the batch close is one of them, and a row
// that reported `ok:true` without saying what the issue ended AS would
// force a follow-up read per row — the opposite of what a batch is for.
//
// The first four keys are batchResultRow's, name for name and type for
// type, so a caller that parses a batch row parses this. They are spelled
// out rather than embedded for the reason ackRow states: embedding would
// tie the two shapes together and make any future change to the reference
// row change this one silently.
type closeManyResultRow struct {
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Version int            `json:"version,omitempty"`
	Error   *batchErrorDTO `json:"error,omitempty"`

	// CloseOutcome is the row's post-state outcome, same Go type and same
	// wire name as on the single acknowledgement (ack.go#ackRow).
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseLink is PER ROW, not per call: one item may create the edge and
	// the next find it already there, in the same request. A per-call flag
	// would have to pick one of those answers and be wrong about the other.
	CloseLink issues.CloseLinkState `json:"closeLink,omitempty"`
}

// closeManyResponse is skein_close_many's envelope. Identical in shape to
// batchResponse — same two keys, same summary type — with the rows being
// this tool's own. The envelope is NOT reused so that the guard pinning
// batchResponse keeps guarding only the tools it still describes.
type closeManyResponse struct {
	Results []closeManyResultRow `json:"results"`
	Summary batchSummary         `json:"summary"`
}

// RegisterCloseMany wires the skein_close_many tool. Each item is its
// own SERIALIZABLE transaction via issues.Store.CloseCommitted — partial-success
// is the defining feature of the batch, so a row-level failure never
// poisons the rest of the batch.
//
// `force=true` needs force_unlock on each row's project (sk-674.18 T034,
// K-28; same gate as skein_close); a row without it fails alone.
//
// Annotations match skein_close: not read-only, not destructive (close
// is reversible), not idempotent.
func RegisterCloseMany(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_close_many",
		mcpgo.WithDescription(
			"Close many issues in one round-trip. Each item is processed "+
				"independently in its own SERIALIZABLE transaction; on "+
				"row-level failure (version_conflict, locked_by_other, "+
				"pinned_no_force, ...) the rest of the batch still runs. "+
				"Inspect results[i].ok per row to find the failures and "+
				"results[i].error.code to branch on the cause. "+
				"Result rows preserve input order. "+
				"Caps at 100 items per call (matches the REST bulk "+
				"endpoint). For >100 issues, split client-side. "+
				"`force=true` needs force_unlock on each row's project and bypasses lock + version "+
				"+ pinned predicates uniformly across all rows. "+
				"Per-row `reason` is required (non-empty after trim) — "+
				"agents typically pass the same string in every item. "+
				"Each item may carry an optional `commits[]` (max 10). "+
				"Re-close is NOT idempotent — it overwrites closed_reason / "+
				"closed_at per item; to add commits to already-closed "+
				"issues use `skein_commits_attach` per issue. "+
				"`closeOutcome` / `closeTarget` are per item, as on skein_close. "+
				"closeOutcome is required when transitioning to closed; repeating a close without it preserves the previous outcome. "+
				"Each row answers `closeOutcome`, plus `closeLink` when it named a target.",
		),
		mcpgo.WithArray("items",
			mcpgo.Required(),
			mcpgo.Description("Per-row close requests. Min 1, max 100. Order is preserved in results[]."),
			mcpgo.MinItems(1),
			mcpgo.MaxItems(batchMaxItems),
			mcpgo.Items(map[string]any{
				"type":                 "object",
				"required":             []string{"id", "expectedVersion", "reason"},
				"additionalProperties": false,
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Full hierarchical issue id (e.g. \"demo-42\").",
					},
					"expectedVersion": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "The version the agent observed; close only proceeds when the live version matches.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Free-form reason annotation (non-empty after trim). Lands in events.reason and issues.closed_reason.",
					},
					"commits": map[string]any{
						"type":        "array",
						"description": commitsArrayDescription,
						"maxItems":    maxCommitsPerCloseCall,
						"items":       commitInputItemSchema(),
					},
					"closeOutcome": map[string]any{
						"type":        "string",
						"enum":        issues.ValidCloseOutcomes(),
						"description": closeOutcomeParamDescription,
					},
					"closeTarget": map[string]any{
						"type":        "string",
						"description": closeTargetParamDescription,
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

	addTool(s, tool, handleCloseMany(pool, issuesStore))
}

// closeManyItem is the parsed shape of one input items[] entry.
type closeManyItem struct {
	ID              string
	ExpectedVersion int
	Reason          string
	Commits         []issues.CloseCommitInput
	// CloseOutcome / CloseTarget — sk-489, per item. They are parsed AND
	// checked in parseCloseManyItem, not here and not in close.go: this
	// batch never calls the single tool's handler, so a check that lives
	// only there does not exist for these rows at all.
	CloseOutcome *string
	CloseTarget  string
}

func handleCloseMany(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
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

		// Parse + per-row validation. We do NOT fail the entire batch on
		// a single bad row — invalid rows produce a results[i].ok=false
		// entry with code=validation_error and the rest of the batch
		// runs. This matches the partial-success contract documented in
		// the tool description.
		items := make([]parsedCloseItem, len(rawSlice))
		for i, raw := range rawSlice {
			items[i] = parseCloseManyItem(raw)
		}

		sessionID := auth.SessionIDFrom(ctx)
		actor := IssuesAgentActor(ctx, pool, userID)

		// See batchLoopDeadline: the loop stops on our clock so the caller
		// learns which rows closed instead of losing the response.
		loopCtx, cancelLoop := batchLoopContext(ctx)
		defer cancelLoop()

		results := make([]closeManyResultRow, len(items))
		var succeeded, failed int

		for i, parsed := range items {
			// Bail out cleanly if the client cancelled — fill remaining
			// rows as cancelled and stop (same client-visible result as
			// per-row continue, without extra dead-ctx iterations).
			if code, message, stop := batchStopReason(ctx, loopCtx); stop {
				for j := i; j < len(items); j++ {
					results[j] = closeManyResultRow{
						ID: items[j].rawID,
						OK: false,
						Error: &batchErrorDTO{
							Code:    code,
							Message: message,
						},
					}
					failed++
				}
				slog.WarnContext(ctx, "mcp batch stopped early",
					"tool", "skein_close_many", "code", code,
					"appliedRows", succeeded, "unrunRows", len(items)-i,
					"userId", userID.String())
				break
			}
			if parsed.parseErr != nil {
				results[i] = closeManyResultRow{
					ID:    parsed.rawID,
					OK:    false,
					Error: parsed.parseErr,
				}
				failed++
				continue
			}

			// Visibility check — same shape as skein_close. Per-row, so
			// a missing project on one id doesn't poison the rest.
			issue, err := issuesStore.GetIssue(loopCtx, parsed.item.ID)
			if err != nil {
				// projectID is unknown at this point (the load that would
				// tell us just failed) — uuid.Nil is safe here because a
				// GetIssue failure can never be projects.ErrRepoNotFound /
				// ErrRepoAmbiguous, the only codes that consult it.
				c := classifyCloseRowError(ctx, loopCtx, pool, uuid.Nil, err, userID.String(), parsed.item.ID, parsed.item.CloseTarget)
				results[i] = closeManyResultRow{
					ID:    parsed.item.ID,
					OK:    false,
					Error: &batchErrorDTO{Code: c.Code, Message: c.Message, Details: c.Details},
				}
				failed++
				continue
			}
			// sk-674.18 T066: closing is ActionWrite of US-6.
			rowLevel, vErr := projects.EnsureProjectAction(loopCtx, pool, issue.ProjectID, userID, authz.ActionWrite)
			if vErr == nil && force && projects.RequireAction(rowLevel, authz.ActionForceUnlock) != nil {
				// sk-674.18 T034 (K-28): force on this row's project.
				results[i] = closeManyResultRow{
					ID: parsed.item.ID,
					OK: false,
					Error: &batchErrorDTO{
						Code:    ErrCodeForceUnlockForbidden,
						Message: msgForceUnlockForbidden,
						Details: map[string]any{"id": parsed.item.ID},
					},
				}
				failed++
				continue
			}
			if vErr != nil {
				if errors.Is(vErr, projects.ErrProjectNotVisible) {
					results[i] = closeManyResultRow{
						ID: parsed.item.ID,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeIssueNotFound,
							Message: "issue does not exist or is not visible",
							Details: map[string]any{"id": parsed.item.ID},
						},
					}
					failed++
					continue
				}
				if errors.Is(vErr, projects.ErrProjectActionForbidden) {
					results[i] = closeManyResultRow{
						ID: parsed.item.ID,
						OK: false,
						Error: &batchErrorDTO{
							Code:    ErrCodeForbidden,
							Message: msgActionForbidden,
							Details: map[string]any{"id": parsed.item.ID},
						},
					}
					failed++
					continue
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_close_many",
					"reason", "visibility check failed",
					"userId", userID.String(),
					"issueId", parsed.item.ID,
					"error", vErr.Error())
				results[i] = closeManyResultRow{
					ID: parsed.item.ID,
					OK: false,
					Error: &batchErrorDTO{
						Code:    ErrCodeInternalError,
						Message: "visibility check failed",
					},
				}
				failed++
				continue
			}

			// sk-607: CloseCommitted, not CloseWithResult — the row is
			// {id, ok, version, closeOutcome, closeLink}, all of which the
			// committing transaction read. The full card per row was loaded
			// only to be dropped, and it was that post-commit read the
			// batch's own deadline cut in production, reporting committed
			// closes as internal_error.
			state, link, err := issuesStore.CloseCommitted(loopCtx, issues.CloseInput{
				ID:              parsed.item.ID,
				ExpectedVersion: parsed.item.ExpectedVersion,
				Reason:          parsed.item.Reason,
				Force:           force,
				SessionID:       sessionID,
				Actor:           actor,
				Commits:         parsed.item.Commits,
				CloseOutcome:    parsed.item.CloseOutcome,
				CloseTarget:     parsed.item.CloseTarget,
				// sk-540.1: an explicit outcome is mandatory on a transition;
				// a row without one fails alone, the rest still close.
				RequireOutcomeOnTransition: true,
				// The system role — see close.go. Required with a non-empty
				// target, and always passed: "pass only when
				// needed" is one refactoring away from a store failure.
				DependenciesCaller: &issues.DependenciesCaller{UserID: userID, Role: role},
			})
			if err != nil {
				c := classifyCloseRowError(ctx, loopCtx, pool, issue.ProjectID, err, userID.String(), parsed.item.ID, parsed.item.CloseTarget)
				results[i] = closeManyResultRow{
					ID:    parsed.item.ID,
					OK:    false,
					Error: &batchErrorDTO{Code: c.Code, Message: c.Message, Details: c.Details},
				}
				failed++
				continue
			}

			results[i] = closeManyResultRow{
				ID:           parsed.item.ID,
				OK:           true,
				Version:      state.Version,
				CloseOutcome: state.CloseOutcome,
				CloseLink:    link,
			}
			succeeded++
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_close_many",
			"userId", userID.String(),
			"requested", len(items),
			"succeeded", succeeded,
			"failed", failed,
			"force", force,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(closeManyResponse{
			Results: results,
			Summary: batchSummary{
				Requested: len(items),
				Succeeded: succeeded,
				Failed:    failed,
			},
		})
	}
}

// parsedCloseItem is one partially-parsed input row. parseErr is set
// when validation fails — the dispatcher then emits a results[i] with
// the captured error and skips the store call for that row.
type parsedCloseItem struct {
	rawID    string
	item     closeManyItem
	parseErr *batchErrorDTO
}

func parseCloseManyItem(raw any) parsedCloseItem {
	out := parsedCloseItem{}
	obj, ok := raw.(map[string]any)
	if !ok {
		out.parseErr = &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i] must be an object",
		}
		return out
	}

	idAny, _ := obj["id"]
	rawID, _ := idAny.(string)
	out.rawID = rawID
	id := strings.TrimSpace(rawID)
	if !looksLikeHierarchicalID(id) {
		out.parseErr = &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i].id must be a full hierarchical id like \"demo-42\"",
			Details: map[string]any{"id": rawID},
		}
		return out
	}

	expectedVersion, vErr := readPositiveInt(obj, "expectedVersion")
	if vErr != nil {
		out.parseErr = vErr
		return out
	}

	rawReason, _ := obj["reason"].(string)
	reason := strings.TrimSpace(rawReason)
	if reason == "" {
		out.parseErr = &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i].reason is required and must be non-empty after trim",
			Details: map[string]any{"id": id},
		}
		return out
	}

	// sk-489 — the SECOND input builder. The same helper as in the
	// single skein_close: the parsing rules and all target checks
	// that need no database must sound here word for word the same, otherwise
	// a refusal not intercepted here would reach classifyCloseError and become
	// internal_error at the batch position.
	outcome, outcomeErr := parseCloseOutcomeArgs(obj, id, "items[i].")
	if outcomeErr != nil {
		details := outcomeErr.Details
		if details == nil {
			details = map[string]any{}
		}
		if _, ok := details["id"]; !ok {
			details["id"] = id
		}
		out.parseErr = &batchErrorDTO{
			Code:    outcomeErr.Code,
			Message: outcomeErr.Message,
			Details: details,
		}
		return out
	}

	commits, commitErr := parseCommitInputs(obj["commits"], "items[i].commits")
	if commitErr != nil {
		// Annotate with the issue id so the caller can pinpoint
		// the offending row in a partial-success batch.
		details := commitErr.Details
		if details == nil {
			details = map[string]any{}
		}
		if _, ok := details["id"]; !ok {
			details["id"] = id
		}
		out.parseErr = &batchErrorDTO{
			Code:    commitErr.Code,
			Message: commitErr.Message,
			Details: details,
		}
		return out
	}

	out.item = closeManyItem{
		ID:              id,
		ExpectedVersion: expectedVersion,
		Reason:          reason,
		Commits:         commits,
		CloseOutcome:    outcome.Outcome,
		CloseTarget:     outcome.Target,
	}
	return out
}

// errorDTOFromCallResult unpacks a CallToolResult error envelope into
// a batchErrorDTO. Used by skein_update_many to re-package the
// validation errors produced by buildUpdateOneInput (which mirrors
// the single-tool parser and emits a CallToolResult). Failure paths
// fall back to a generic internal_error envelope rather than dropping
// information silently.
func errorDTOFromCallResult(res *mcpgo.CallToolResult) *batchErrorDTO {
	if res == nil {
		return &batchErrorDTO{Code: ErrCodeInternalError, Message: "nil tool result"}
	}
	body := unmarshalToolErrorBody(res)
	if body == nil {
		return &batchErrorDTO{Code: ErrCodeInternalError, Message: "unparseable tool error"}
	}
	dto := &batchErrorDTO{}
	if code, _ := body["error"].(string); code != "" {
		dto.Code = code
	} else {
		dto.Code = ErrCodeInternalError
	}
	if msg, _ := body["message"].(string); msg != "" {
		dto.Message = msg
	}
	delete(body, "error")
	delete(body, "message")
	if len(body) > 0 {
		dto.Details = body
	}
	return dto
}

// unmarshalToolErrorBody pulls the JSON body out of a CallToolResult.
// skmcp.ToolError emits the JSON via NewToolResultError, which sets
// Content[0] to TextContent{Text: <json>}. We re-parse that text so
// the batch tool can re-package it into a batchErrorDTO without
// duplicating the closed set of error code mappings.
func unmarshalToolErrorBody(res *mcpgo.CallToolResult) map[string]any {
	if len(res.Content) == 0 {
		return nil
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		return nil
	}
	var body map[string]any
	if err := jsonUnmarshalString(tc.Text, &body); err != nil {
		return nil
	}
	return body
}

// readPositiveInt extracts a positive-integer field from a map[string]any
// produced by JSON unmarshal. Numbers may arrive as float64 (mcp-go
// passes them through encoding/json) or json.Number. Both are accepted.
func readPositiveInt(obj map[string]any, key string) (int, *batchErrorDTO) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return 0, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i]." + key + " is required",
		}
	}
	var n int
	switch v := raw.(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	default:
		return 0, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i]." + key + " must be a positive integer",
		}
	}
	if n < 1 {
		return 0, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: "items[i]." + key + " must be >= 1",
		}
	}
	return n, nil
}

package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterUpdateMany wires the skein_update_many tool. Each items[i]
// is a fully-independent partial patch — same shape as skein_update,
// minus the top-level id/expectedVersion (those go inside each item).
// The contract is "N independent patches in one round-trip", not
// "same patch applied to N issues" — agents constructing a mass-edit
// build the same value into each item.
//
// Per-row behaviour mirrors skein_update exactly: each row is one
// atomic change (issues.Store.UpdateIssue, T015, US-14) — a failed row
// writes nothing, and the rest of the batch keeps running.
//
// `force=true` needs the key right manage and force_unlock on each row's
// project, and applies uniformly across the batch.
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
				patchTextContractNote+" Refusal applies only to the offending row. "+
				"Each row is one atomic change, as in skein_update: a "+
				"failed row writes nothing and does not affect the rest "+
				"of the batch. Inspect results[i].ok / results[i].error.code "+
				"per row. A row stopped by batch_deadline_exceeded / "+
				"request_cancelled while in progress may still have "+
				"committed as a whole: re-read it before resending. "+
				"A row answering serialization_retry wrote nothing; resend it unchanged.\n\n"+
				"closeOutcome is required when transitioning to closed; repeating a close without it preserves the previous outcome. "+
				"No per-row `closeTarget`: close an OPEN issue as duplicate / "+
				"superseded with skein_close / skein_update. "+
				"Caps at 100 items per call. "+
				"`force=true` needs the key right `manage` and force_unlock "+
				"on each row's project, and applies uniformly to every row. "+
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
			mcpgo.Description("Bypass lock + version + pinned predicates across the entire batch. Needs the key right `manage` (else `insufficient_scope`) and force_unlock on each row's project (project owner, space owner, instance admin); a row without it fails with `force_unlock_forbidden`. Default: false."),
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
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.UpdateMany(ctx, d, userID, issueops.Args(req.GetArguments()), func(parent context.Context) (context.Context, context.CancelFunc) { return batchLoopContext(parent) })
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(res)
	}
}

func updateRowContextStop(parent, loop context.Context, err error, issueID string) *batchErrorDTO {
	return issueops.UpdateRowContextStop(parent, loop, err, issueID)
}

func classifyUpdateError(ctx context.Context, err error, tool, userID, issueID string) *batchErrorDTO {
	return issueops.ClassifyUpdateError(ctx, err, tool, userID, issueID)
}

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

// batchMaxItems caps the per-call batch size for every R5.3 batch tool.
// Matches the REST BulkHandler bulkMaxIDs (=100) so the contract is the
// same across MCP and REST surfaces. Larger jobs should be a background
// task, not a synchronous round-trip.
const batchMaxItems = issueops.BatchMaxItems

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
				"A row returning serialization_retry rolled back its close without a version change; resend that row unchanged.\n\n"+
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
			mcpgo.Description("Bypass lock + version + pinned predicates across the entire batch. Needs the key right `manage` (else `insufficient_scope`) and force_unlock on each row's project (project owner, space owner, instance admin); a row without it fails with `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleCloseMany(pool, issuesStore))
}

func handleCloseMany(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.CloseMany(ctx, d, userID, issueops.Args(req.GetArguments()), func(parent context.Context) (context.Context, context.CancelFunc) { return batchLoopContext(parent) })
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(res)
	}
}

// The batch types live in issueops (sk-878.3.1 T025).
type (
	batchResultRow     = issueops.BatchResultRow
	batchErrorDTO      = issueops.BatchErrorDTO
	batchSummary       = issueops.BatchSummary
	batchResponse      = issueops.BatchResponse
	closeManyResultRow = issueops.CloseManyResultRow
	closeManyResponse  = issueops.CloseManyResponse
	closeManyItem      = issueops.CloseManyItem
	parsedCloseItem    = issueops.ParsedCloseItem
)

func parseCloseManyItem(raw any) parsedCloseItem { return issueops.ParseCloseManyItem(raw) }

func readPositiveInt(obj map[string]any, key string) (int, *batchErrorDTO) {
	return issueops.ReadPositiveInt(obj, key)
}

package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
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

func handleDepAddMany(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore, Deps: depsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.DepAddMany(ctx, d, userID, issueops.Args(req.GetArguments()), func(parent context.Context) (context.Context, context.CancelFunc) { return batchLoopContext(parent) })
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(res)
	}
}

type (
	depAddManyResultRow = issueops.DepAddManyResultRow
	depAddManyResponse  = issueops.DepAddManyResponse
)

func depErrorDTOFromValidation(err error) *batchErrorDTO {
	return issueops.DepErrorDTOFromValidation(err)
}

func depRowContextStop(parent, loop context.Context, err error, fromID, toID, depType string) *batchErrorDTO {
	return issueops.DepRowContextStop(parent, loop, err, fromID, toID, depType)
}

func classifyDepAddError(ctx, loopCtx context.Context, err error, userID, fromID, toID, depType string) *batchErrorDTO {
	return issueops.ClassifyDepAddError(ctx, loopCtx, err, userID, fromID, toID, depType)
}

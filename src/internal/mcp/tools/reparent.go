package tools

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterReparent wires the skein_reparent tool — THE way to move an
// existing issue under a new parent. It atomically replaces the issue's
// single parent-child edge (self-healing a double-parented child to
// exactly one edge) inside one SERIALIZABLE transaction, bumps the
// issue's version, and emits issue.reparented + the per-edge
// dependency.deleted / dependency.created audit rows.
//
// Contrast with skein_dep_add(type="parent-child"): that path only ADDS
// a parent and now REJECTS a second parent with parent_already_set.
// Setting the parent at creation time uses `parentId` on skein_create;
// MOVING an existing issue afterwards is exclusively skein_reparent.
//
// Optimistic-concurrency: expectedVersion must match the live version
// (mismatch → version_conflict, currentVersion echoed). NOT lock-gated —
// reparent moves a graph edge (like skein_dep_add / skein_dep_remove),
// so a lock held by another session does not block it.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: true  — replaces the existing parent edge.
//   - idempotentHint: false  — re-running with the SAME new parent still
//     performs a real DELETE+INSERT and bumps version (every move is an
//     auditable event).
//   - openWorldHint: false
func RegisterReparent(s *server.MCPServer, pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_reparent",
		mcpgo.WithDescription(withAckAnswerNote(
			"Move an EXISTING issue under a new parent. This is THE tool "+
				"for re-parenting — `skein_dep_add(type=\"parent-child\")` "+
				"only ADDS a parent and rejects a second one with "+
				"`parent_already_set`; setting the parent at creation uses "+
				"`parentId` on skein_create. skein_reparent atomically "+
				"replaces the issue's single parent-child edge in one "+
				"SERIALIZABLE transaction (self-healing a double-parented "+
				"child to exactly one edge), bumps the issue version, and "+
				"emits `issue.reparented` plus the per-edge "+
				"`dependency.deleted` / `dependency.created` audit rows.\n\n"+
				"Required: `id` (the issue to move), `newParentId` (the new "+
				"parent), and `expectedVersion` for optimistic concurrency. "+
				"On version mismatch returns "+
				"`{error:\"version_conflict\", currentVersion:<int>}` — "+
				"re-read with skein_show and retry. The new parent must be "+
				"in the SAME project (`reparent_cross_project` otherwise) "+
				"and must not already be a descendant of the issue "+
				"(`dependency_cycle`). An issue cannot be its own parent "+
				"(`reparent_self`).\n\n"+
				"NOT lock-gated: reparent moves a graph edge (like "+
				"skein_dep_add / skein_dep_remove), so a lock held by "+
				"another session does not block it — concurrency is "+
				"guarded by expectedVersion alone.\n\n"+
				"NOT idempotent: re-running with the same `newParentId` "+
				"still performs a real move (DELETE+INSERT) and bumps the "+
				"version — compare `parentIdCurrent` first if you want to "+
				"avoid a no-op move. On success returns the "+
				"acknowledgement (id, status, the new version); pass "+
				"`verbose` for the full IssueDetail.\n\n"+
				"Post-action summary: report `✓ <id> re-parented under "+
				"<newParentId>` so the user can scan the move.",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the issue to move (e.g. \"demo-42\" or \"demo-42.1\")."),
		),
		mcpgo.WithString("newParentId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the new parent. Must be in the same project and must not be a descendant of the issue."),
		),
		mcpgo.WithNumber("expectedVersion",
			mcpgo.Required(),
			mcpgo.Description("The version the agent expects on the issue. The move only proceeds when the live version matches; otherwise returns version_conflict with the live version."),
			mcpgo.Min(1),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleReparent(pool, depsStore, issuesStore))
}

func handleReparent(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore, Deps: depsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Reparent(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		id, final := res.IssueID, res.Issue
		return writeIssueAck(ctx, "skein_reparent", userID.String(), id,
			final, req.GetBool("verbose", false))
	}
}

func mapReparentError(ctx context.Context, err error, userID uuid.UUID, id, newParentID string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapReparentError(ctx, err, userID, id, newParentID))
}

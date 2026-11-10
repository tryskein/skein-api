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

// RegisterDepRemove wires the skein_dep_remove tool — the inverse of
// skein_dep_add. Removes one typed dependency edge identified by the
// composite (fromId, toId, type). This is the cleanup path: when an edge
// was added by mistake, or a relationship no longer holds, this tool
// drops it and writes the dependency.deleted audit row.
//
// To MOVE a parent-child edge use skein_reparent instead — it atomically
// replaces the parent in one transaction. skein_dep_remove only deletes.
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: true  — deletes a graph edge.
//   - idempotentHint: false  — a second call on an already-removed edge
//     returns dependency_not_found (the store surfaces the miss rather
//     than a silent success so a stale UI dep list is caught).
//   - openWorldHint: false
func RegisterDepRemove(s *server.MCPServer, pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_dep_remove",
		mcpgo.WithDescription(
			"Remove a typed dependency edge — the inverse of skein_dep_add "+
				"and the cleanup path when an edge was added by mistake or a "+
				"relationship no longer holds. Identifies the edge by the "+
				"composite (fromId, toId, type) and writes a "+
				"`dependency.deleted` audit row.\n\n"+
				"To MOVE a parent-child edge (re-parent an issue) use "+
				"`skein_reparent` instead — it atomically replaces the "+
				"parent. skein_dep_remove only deletes.\n\n"+
				"Returns `dependency_not_found` when no edge matches the "+
				"tuple (not a silent success — a stale dep list is surfaced "+
				"so the caller re-reads).\n\n"+
				"Refuses with `close_outcome_link_required` (nothing removed) "+
				"when the edge is the LAST one behind a close outcome: the last "+
				"`duplicates` edge FROM an issue closed as `duplicate`, or the "+
				"last `supersedes` edge TO an issue closed as `superseded`. "+
				"The error names that issue only when you can see it. "+
				"Change that issue's closeOutcome or reopen it first; with "+
				"another edge of the same type and direction left, the removal "+
				"goes through.",
		),
		mcpgo.WithString("fromId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the FROM-side issue (e.g. \"demo-42\")."),
		),
		mcpgo.WithString("toId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the TO-side issue (e.g. \"billing-9\")."),
		),
		mcpgo.WithString("type",
			mcpgo.Required(),
			mcpgo.Description("Dependency type of the edge to remove."),
			mcpgo.Enum(
				dependencies.DepBlocks,
				dependencies.DepRelated,
				dependencies.DepDiscoveredFrom,
				dependencies.DepSupersedes,
				dependencies.DepDuplicates,
				dependencies.DepWaitsFor,
				dependencies.DepParentChild,
			),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleDepRemove(pool, depsStore, issuesStore))
}

// depRemoveResponse is the success body for skein_dep_remove. We echo the
// removed edge tuple so the agent can confirm exactly what was dropped.
type depRemoveResponse struct {
	Removed bool   `json:"removed"`
	FromID  string `json:"fromId"`
	ToID    string `json:"toId"`
	Type    string `json:"type"`
}

func handleDepRemove(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore, Deps: depsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.DepRemove(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(depRemoveResponse{Removed: true, FromID: res.FromID, ToID: res.ToID, Type: res.Type})
	}
}

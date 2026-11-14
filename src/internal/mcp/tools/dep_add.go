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

// RegisterDepAdd wires the skein_dep_add tool. Wraps dependencies.Store.Create
// behind the same visibility / cycle / dedup gates the REST endpoint uses.
//
// Cross-project edges are allowed (e.g. demo-42 blocks billing-9) provided
// the caller can see the project on each end and both lie in one space
// (INV-24). The store enforces both itself; a TO-side issue of another
// space or of an invisible project answers issue_not_found, as a
// nonexistent one (INV-3, sk-674.18 T033).
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false  — second call collides on composite PK and
//     returns dependency_already_exists.
//   - openWorldHint: false
func RegisterDepAdd(s *server.MCPServer, pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_dep_add",
		mcpgo.WithDescription(
			"Add a typed dependency edge between two issues (blocks, "+
				"waits-for, related, discovered-from, supersedes, "+
				"duplicates, parent-child). The PRIMARY way to express ANY "+
				"relationship between issues — always this or "+
				"`skein_create({dependencies:[...]})`, never a free-text "+
				"ref (\"depends on X\", \"see also Y\", \"requires Z\") in "+
				"description / acceptance / notes / design: prose is "+
				"invisible to the dependency graph, skein_ready gating, "+
				"the UI Dependencies section and close-time warnings.\n\n"+
				depHierarchyNote+"\n\n"+
				"`gate` is accepted only on type=waits-for, optional there; "+
				"it is stored but not evaluated — readiness waits for `to` "+
				"itself to close, not for its children. "+
				"Cross-project edges are allowed within one space if you "+
				"can see both projects; a target of another space or of a "+
				"project you cannot see answers issue_not_found, exactly "+
				"as a nonexistent one; a cycle "+
				"returns dependency_cycle. Type table and direction semantics: "+
				"skein_get_instructions (\"Dependency edge types\").",
		),
		mcpgo.WithString("fromId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the FROM-side issue (e.g. \"demo-42\")."),
		),
		mcpgo.WithString("toId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the TO-side issue (e.g. \"billing-9\"). May be in a different project."),
		),
		mcpgo.WithString("type",
			mcpgo.Required(),
			mcpgo.Description(depTypeParamDescription),
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
		mcpgo.WithString("gate",
			mcpgo.Description(depGateParamDescription),
			mcpgo.Enum(dependencies.GateAllChildren, dependencies.GateAnyChildren),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleDepAdd(pool, depsStore, issuesStore))
}

// depAddResponse mirrors the REST POST /dependencies success body but uses
// camelCase field names directly (DependencyDTO already has them) and
// re-exports gate from metadata as a top-level field for symmetry with the
// input schema.
type depAddResponse struct {
	Dependency depDTO `json:"dependency"`
}

func handleDepAdd(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore, Deps: depsStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		dto, f := issueops.DepAdd(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(depAddResponse{Dependency: dto})
	}
}

type (
	depDTO             = issueops.DepDTO
	depStoreErrorClass = issueops.DepStoreErrorClass
)

func classifyDepStoreError(err error, fromID, toID, depType string) *depStoreErrorClass {
	return issueops.ClassifyDepStoreError(err, fromID, toID, depType)
}

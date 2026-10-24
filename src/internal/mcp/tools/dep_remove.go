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

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		// ---- Required: fromId / toId / type ----
		rawFromID, err := req.RequireString("fromId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "fromId is required", nil), nil
		}
		fromID := strings.TrimSpace(rawFromID)
		if !looksLikeHierarchicalID(fromID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"fromId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"fromId": rawFromID}), nil
		}

		rawToID, err := req.RequireString("toId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "toId is required", nil), nil
		}
		toID := strings.TrimSpace(rawToID)
		if !looksLikeHierarchicalID(toID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"toId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"toId": rawToID}), nil
		}

		rawType, err := req.RequireString("type")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "type is required", nil), nil
		}
		depType, vErr := dependencies.ValidateType(rawType)
		if vErr != nil {
			return depValidationToolError(vErr), nil
		}

		// ---- Pre-check FROM-side visibility ----
		// Same shape as skein_dep_add: the caller asserts permission via the
		// FROM-side project. A missing / invisible FROM surfaces as
		// issue_not_found (no leak).
		fromIssue, err := issuesStore.GetIssue(ctx, fromID)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"fromId": fromID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_dep_remove",
				"reason", "load fromId failed",
				"userId", userID.String(), "fromId", fromID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load fromId", nil), nil
		}
		if res := issueActionGate(ctx, pool, "skein_dep_remove", "fromId", fromID, fromIssue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		// ---- Remove the dependency ----
		actor := DependenciesAgentActor(ctx, pool, userID)
		if err := depsStore.Delete(ctx, fromID, toID, depType, actor); err != nil {
			return mapDepRemoveError(ctx, err, userID, fromID, toID, depType), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_dep_remove",
			"userId", userID.String(),
			"fromId", fromID, "toId", toID, "type", depType,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(depRemoveResponse{
			Removed: true,
			FromID:  fromID,
			ToID:    toID,
			Type:    depType,
		})
	}
}

// mapDepRemoveError translates dependencies.Store.Delete typed errors
// into stable MCP error codes. ErrNotFound (no matching edge) maps to
// dependency_not_found; validation errors map to validation_error.
//
// close_outcome_link_required (sk-540.8) carries the closed issue, its
// outcome and the edge type in details so the agent can act on the hint —
// the issue and outcome only when the caller can see that issue (sk-423);
// serialization_retry marks a lost SERIALIZABLE race (nothing written).
//
// Intentional subset: sk-118 standing-graph codes (nested_standing,
// standing_parent_required, …) are NOT promoted here. Delete only removes
// an existing edge and does not re-validate standing hierarchy; those
// codes fire on Create / Reparent paths (mapDepAddError / mapReparentError).
func mapDepRemoveError(ctx context.Context, err error, userID uuid.UUID, fromID, toID, depType string) *mcpgo.CallToolResult {
	if vErr, ok := dependencies.IsValidationError(err); ok {
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var linkErr *dependencies.ErrCloseOutcomeLinkRequired
	if errors.As(err, &linkErr) {
		return skmcp.ToolError(ErrCodeCloseOutcomeLinkRequired, linkErr.Error(),
			linkErr.Details())
	}
	if errors.Is(err, dependencies.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"dependency removal lost a serialization race — nothing was written, retry verbatim", nil)
	}
	if errors.Is(err, dependencies.ErrNotFound) {
		return skmcp.ToolError(ErrCodeDependencyNotFound,
			"no dependency edge matches (fromId, toId, type)",
			map[string]any{"fromId": fromID, "toId": toID, "type": depType})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_dep_remove",
		"reason", "store delete failed",
		"userId", userID.String(),
		"fromId", fromID, "toId", toID, "type", depType, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to remove dependency", nil)
}

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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		// ---- Required: id / newParentId / expectedVersion ----
		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		id := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"demo-42\"",
				map[string]any{"id": rawID}), nil
		}

		rawNewParentID, err := req.RequireString("newParentId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "newParentId is required", nil), nil
		}
		newParentID := strings.TrimSpace(rawNewParentID)
		if !looksLikeHierarchicalID(newParentID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"newParentId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"newParentId": rawNewParentID}), nil
		}

		expectedVersion := req.GetInt("expectedVersion", 0)
		if expectedVersion < 1 {
			return skmcp.ToolError(ErrCodeValidationError,
				"expectedVersion is required and must be >= 1", nil), nil
		}

		// ---- Visibility: load the issue once, ensure project visible ----
		// Same pre-check shape as skein_update / skein_dep_add: surface
		// issue_not_found for both "missing" and "not visible" so we don't
		// leak the existence of a foreign issue.
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_reparent",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		if res := issueActionGate(ctx, pool, "skein_reparent", "id", id, issue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		// ---- Reparent ----
		actor := DependenciesAgentActor(ctx, pool, userID)
		_, err = depsStore.Reparent(ctx, id, newParentID, expectedVersion,
			dependencies.CallerAccess{UserID: userID, Role: role}, actor)
		if err != nil {
			return mapReparentError(ctx, err, userID, id, newParentID), nil
		}

		// ---- Re-fetch the updated IssueDetail to return ----
		// Reparent does not return an IssueDetail (it can't import issues),
		// so we re-fetch the same way every other write tool returns its
		// post-state. The version is now bumped and parentIdCurrent reflects
		// the new parent.
		final, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_reparent",
				"reason", "re-fetch after reparent failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"reparent committed but re-fetch failed; re-read with skein_show", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_reparent",
			"userId", userID.String(), "issueId", id, "newParentId", newParentID,
			"version", final.Version,
			"latencyMs", time.Since(start).Milliseconds())
		return writeIssueAck(ctx, "skein_reparent", userID.String(), id,
			final, req.GetBool("verbose", false))
	}
}

// mapReparentError translates dependencies.Store.Reparent typed errors
// into stable MCP error codes. Every frozen sentinel from the domain
// layer is mapped here — none fall through to internal_error except a
// genuinely unrecognised error.
func mapReparentError(ctx context.Context, err error, userID uuid.UUID, id, newParentID string) *mcpgo.CallToolResult {
	if vErr, ok := dependencies.IsValidationError(err); ok {
		// sk-118 (F2e): the nested-standing ban is a *ValidationError but
		// carries an actionable, branch-on-able code — promote it to the
		// top-level envelope `error` (matching the F2c issues-side promotion)
		// rather than burying it under validation_error.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return skmcp.ToolError(ErrCodeNestedStandingForbidden, vErr.Message,
				map[string]any{"id": id, "newParentId": newParentID})
		}
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var verr *dependencies.ErrVersionConflict
	if errors.As(err, &verr) {
		return skmcp.ToolError(ErrCodeVersionConflict,
			"the issue was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	if errors.Is(err, dependencies.ErrReparentSelf) {
		return skmcp.ToolError(ErrCodeReparentSelf,
			"an issue cannot be its own parent",
			map[string]any{"id": id})
	}
	if errors.Is(err, dependencies.ErrReparentCrossProject) {
		return skmcp.ToolError(ErrCodeReparentCrossProject,
			"new parent must be in the same project as the issue",
			map[string]any{"id": id, "newParentId": newParentID})
	}
	if errors.Is(err, dependencies.ErrFromNotFound) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": id})
	}
	// INV-3 (sk-674.18 T033): a parent in a project the caller cannot see
	// answers exactly as a nonexistent one — same code, text and details.
	if errors.Is(err, dependencies.ErrTargetNotFound) || errors.Is(err, dependencies.ErrDependencyTargetNotVisible) {
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"new parent does not exist or is not visible",
			map[string]any{"newParentId": newParentID})
	}
	if errors.Is(err, dependencies.ErrSerializationRetry) {
		return skmcp.ToolError(ErrCodeSerializationRetry,
			"reparent lost a serialization race — safe to retry",
			map[string]any{"id": id})
	}
	if errors.Is(err, dependencies.ErrDependencyTargetNotReady) {
		return skmcp.ToolError(ErrCodeDependencyTargetNotVisibleYet,
			"new parent not visible yet — retry shortly",
			map[string]any{"newParentId": newParentID})
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(err, &cycle) {
		return skmcp.ToolError(ErrCodeDependencyCycle,
			"this move would create a cycle (new parent is a descendant of the issue)",
			map[string]any{"path": cycle.Path})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_reparent",
		"reason", "store reparent failed",
		"userId", userID.String(),
		"issueId", id, "newParentId", newParentID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to reparent issue", nil)
}

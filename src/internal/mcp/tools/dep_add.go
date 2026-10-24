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
				"returns dependency_cycle. Type table, direction semantics "+
				"and error codes: skein_get_instructions.",
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

// depDTO is the wire shape returned by skein_dep_add. We hoist `gate` out
// of metadata so the agent gets the same field shape on the way out as it
// passed in, without having to dig into a generic map.
type depDTO struct {
	From      string         `json:"from"`
	To        string         `json:"to"`
	Type      string         `json:"type"`
	Gate      *string        `json:"gate"`
	Metadata  map[string]any `json:"metadata"`
	Version   int            `json:"version"`
	CreatedAt time.Time      `json:"createdAt"`
}

func handleDepAdd(pool *pgxpool.Pool, depsStore *dependencies.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

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
		// Validate up front to catch bad type / gate combos before any DB hit.
		depType, vErr := dependencies.ValidateType(rawType)
		if vErr != nil {
			return depValidationToolError(vErr), nil
		}

		// ---- Optional: gate ----
		var gateArg *string
		if g := strings.TrimSpace(req.GetString("gate", "")); g != "" {
			gateArg = &g
		}
		if _, vErr := dependencies.ValidateGate(depType, gateArg); vErr != nil {
			return depValidationToolError(vErr), nil
		}

		// ---- Pre-check FROM-side visibility ----
		// dependencies.Store.Create will reject ErrFromNotFound when the
		// FROM-side issue genuinely doesn't exist OR when it sits in a
		// project the caller cannot see. We do an explicit visibility check
		// up front so the error code can surface as issue_not_found rather
		// than the generic "from-issue not found" — the spec calls out
		// issue_not_found as the right code for either condition (no leak).
		fromIssue, err := issuesStore.GetIssue(ctx, fromID)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"fromId": fromID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_dep_add",
				"reason", "load fromId failed",
				"userId", userID.String(), "fromId", fromID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load fromId", nil), nil
		}
		if res := issueActionGate(ctx, pool, "skein_dep_add", "fromId", fromID, fromIssue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		// ---- Create the dependency ----
		actor := DependenciesAgentActor(ctx, pool, userID)
		dto, err := depsStore.Create(ctx, fromID, dependencies.CreateDepInput{
			ToID: toID,
			Type: depType,
			Gate: gateArg,
		}, dependencies.CallerAccess{UserID: userID, Role: role}, actor)
		if err != nil {
			return mapDepAddError(ctx, err, userID, fromID, toID, depType), nil
		}

		// ---- Build response ----
		// Pull gate out of metadata for symmetry with the input. The store
		// stores gate inside metadata.gate; only emit `gate` when set.
		var gateOut *string
		if g, ok := dto.Metadata["gate"].(string); ok && g != "" {
			gg := g
			gateOut = &gg
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_dep_add",
			"userId", userID.String(),
			"fromId", dto.FromID, "toId", dto.ToID, "type", dto.Type,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(depAddResponse{
			Dependency: depDTO{
				From:      dto.FromID,
				To:        dto.ToID,
				Type:      dto.Type,
				Gate:      gateOut,
				Metadata:  dto.Metadata,
				Version:   dto.Version,
				CreatedAt: dto.CreatedAt,
			},
		})
	}
}

// mapDepAddError translates dependencies.Store.Create typed errors into
// stable MCP error codes. Mirrors the handler's mapDepErr but emits MCP
// codes (validation_error / issue_not_found / dependency_*).
func mapDepAddError(ctx context.Context, err error, userID uuid.UUID, fromID, toID, depType string) *mcpgo.CallToolResult {
	if vErr, ok := dependencies.IsValidationError(err); ok {
		// sk-118 (F2e/F2f): the standing-edge guards carry actionable,
		// branch-on-able codes — promote them to the top-level envelope `error`
		// (matching the F2c issues-side promotion) instead of burying them under
		// validation_error.
		switch vErr.Code {
		case ErrCodeNestedStandingForbidden:
			return skmcp.ToolError(ErrCodeNestedStandingForbidden, vErr.Message,
				map[string]any{"code": vErr.Code})
		case ErrCodeBlockerTargetStanding:
			return skmcp.ToolError(ErrCodeBlockerTargetStanding, vErr.Message,
				map[string]any{"code": vErr.Code, "toId": toID, "type": depType})
		}
		return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if cls := classifyDepStoreError(err, fromID, toID, depType); cls != nil {
		return skmcp.ToolError(cls.Code, cls.Message, cls.Details)
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_dep_add",
		"reason", "store create failed",
		"userId", userID.String(),
		"fromId", fromID, "toId", toID, "type", depType, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to create dependency", nil)
}

// depStoreErrorClass is the (code, message, details) triple that the
// single-row (skein_dep_add) and batch (skein_dep_add_many) surfaces render
// identically for a given dependencies.Store failure. Only the envelope
// differs — mapDepAddError wraps it in a CallToolResult, classifyDepAddError
// in a batchErrorDTO.
type depStoreErrorClass struct {
	Code    string
	Message string
	Details map[string]any
}

// classifyDepStoreError maps the dependencies.Store errors whose rendering is
// shared by both dep-add surfaces. Returns nil when err is not one of them —
// the caller then applies its own handling (ValidationError promotion, which
// carries per-surface detail shapes, and the internal_error fallback, which
// logs its own tool name).
func classifyDepStoreError(err error, fromID, toID, depType string) *depStoreErrorClass {
	switch {
	case errors.Is(err, dependencies.ErrFromNotFound):
		// Race vs the pre-check (concurrent delete): surface as
		// issue_not_found, which is the right code regardless.
		return &depStoreErrorClass{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"fromId": fromID},
		}
	case errors.Is(err, dependencies.ErrTargetNotFound),
		// INV-3 (sk-674.18 T033): a target in a project the caller cannot
		// see answers exactly as a nonexistent one.
		errors.Is(err, dependencies.ErrDependencyTargetNotVisible):
		return &depStoreErrorClass{
			Code:    ErrCodeIssueNotFound,
			Message: "issue does not exist or is not visible",
			Details: map[string]any{"toId": toID},
		}
	case errors.Is(err, dependencies.ErrDependencyExists):
		return &depStoreErrorClass{
			Code:    ErrCodeDependencyAlreadyExists,
			Message: "this dependency already exists",
			Details: map[string]any{"fromId": fromID, "toId": toID, "type": depType},
		}
	case errors.Is(err, dependencies.ErrParentAlreadySet):
		// Defect 2 / sk-43: a parent-child edge cannot ADD a second parent.
		// Re-parenting must go through skein_reparent (atomic replace).
		return &depStoreErrorClass{
			Code:    ErrCodeParentAlreadySet,
			Message: "issue already has a parent — use skein_reparent to move it",
			Details: map[string]any{"fromId": fromID},
		}
	case errors.Is(err, dependencies.ErrSerializationRetry):
		// Transient SERIALIZABLE loss — no durable change; safe to retry.
		return &depStoreErrorClass{
			Code:    ErrCodeSerializationRetry,
			Message: "dependency write lost a serialization race — safe to retry",
			Details: map[string]any{"fromId": fromID, "toId": toID, "type": depType},
		}
	case errors.Is(err, dependencies.ErrDependencyTargetNotReady):
		// Target row not visible in this snapshot (mid-commit elsewhere) —
		// retry once the target has committed.
		return &depStoreErrorClass{
			Code:    ErrCodeDependencyTargetNotVisibleYet,
			Message: "dependency target not visible yet — retry shortly",
			Details: map[string]any{"toId": toID},
		}
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(err, &cycle) {
		return &depStoreErrorClass{
			Code:    ErrCodeDependencyCycle,
			Message: "adding this dependency would create a cycle",
			Details: map[string]any{"path": cycle.Path},
		}
	}
	return nil
}

// depValidationToolError adapts a dependencies.ValidationError into the MCP
// tool error envelope. Centralised so dep tools emit the same shape.
func depValidationToolError(err error) *mcpgo.CallToolResult {
	if ve, ok := dependencies.IsValidationError(err); ok {
		// sk-118 (F2e): the nested-standing ban (parent-child edge where both
		// ends are standing) is a *ValidationError but carries an actionable,
		// branch-on-able code — promote it to the top-level envelope `error`
		// (matching the F2c issues-side promotion) instead of burying it under
		// validation_error.
		if ve.Code == ErrCodeNestedStandingForbidden {
			return skmcp.ToolError(ErrCodeNestedStandingForbidden, ve.Message,
				map[string]any{"code": ve.Code})
		}
		if ve.Code == ErrCodeBlockerTargetStanding {
			return skmcp.ToolError(ErrCodeBlockerTargetStanding, ve.Message,
				map[string]any{"code": ve.Code})
		}
		return skmcp.ToolError(ErrCodeValidationError, ve.Message,
			map[string]any{"code": ve.Code})
	}
	return skmcp.ToolError(ErrCodeValidationError, err.Error(), nil)
}

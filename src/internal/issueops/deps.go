package issueops

// deps.go — the dependency operations (sk-878.3.1 T026, the owner's
// decision 01a1c640): add and remove one edge, moved out of the MCP tools
// skein_dep_add and skein_dep_remove with their checks and refusal codes
// unchanged.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// depEnds parses fromId, toId and type, and checks type before the
// database is touched.
func depEnds(a Args) (fromID, toID, depType string, f *Failure) {
	rawFromID, err := a.RequireString("fromId")
	if err != nil {
		return "", "", "", Fail(ErrCodeValidationError, "fromId is required", nil)
	}
	fromID = strings.TrimSpace(rawFromID)
	if !LooksLikeHierarchicalID(fromID) {
		return "", "", "", Fail(ErrCodeValidationError,
			"fromId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"fromId": rawFromID})
	}
	rawToID, err := a.RequireString("toId")
	if err != nil {
		return "", "", "", Fail(ErrCodeValidationError, "toId is required", nil)
	}
	toID = strings.TrimSpace(rawToID)
	if !LooksLikeHierarchicalID(toID) {
		return "", "", "", Fail(ErrCodeValidationError,
			"toId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"toId": rawToID})
	}
	rawType, err := a.RequireString("type")
	if err != nil {
		return "", "", "", Fail(ErrCodeValidationError, "type is required", nil)
	}
	depType, vErr := dependencies.ValidateType(rawType)
	if vErr != nil {
		return "", "", "", DepValidationFailure(vErr)
	}
	return fromID, toID, depType, nil
}

// depFromGate checks that the caller may write in the project of the FROM
// issue: a missing FROM and one out of reach are one answer, issue_not_found
// on fromId (no leak).
func depFromGate(ctx context.Context, d Deps, tool string, userID uuid.UUID, fromID string) *Failure {
	fromIssue, err := d.Issues.GetIssue(ctx, fromID)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible",
				map[string]any{"fromId": fromID})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "load fromId failed",
			"userId", userID.String(), "fromId", fromID, "error", err.Error())
		return Fail(ErrCodeInternalError, "failed to load fromId", nil)
	}
	return IssueActionGate(ctx, d.Pool, tool, "fromId", fromID, fromIssue.ProjectID, userID, authz.ActionWrite)
}

// DepAdd adds one dependency edge (skein_dep_add; addDependency): FROM
// depends on TO by type, a waits-for edge optionally qualified by gate. The
// caller writes through FROM's project; a TO that does not exist or is out
// of reach is refused with the store's code (a reference of the request).
func DepAdd(ctx context.Context, d Deps, userID uuid.UUID, a Args) (DepDTO, *Failure) {
	const tool = "skein_dep_add"
	start := time.Now()
	fromID, toID, depType, f := depEnds(a)
	if f != nil {
		return DepDTO{}, f
	}
	var gateArg *string
	if g := strings.TrimSpace(a.GetString("gate", "")); g != "" {
		gateArg = &g
	}
	if _, vErr := dependencies.ValidateGate(depType, gateArg); vErr != nil {
		return DepDTO{}, DepValidationFailure(vErr)
	}
	if f := depFromGate(ctx, d, tool, userID, fromID); f != nil {
		return DepDTO{}, f
	}

	actor := DependenciesAgentActor(ctx, d.Pool, userID)
	dto, err := d.Deps.Create(ctx, fromID, dependencies.CreateDepInput{
		ToID: toID, Type: depType, Gate: gateArg,
	}, dependencies.CallerAccess{UserID: userID, Role: CallerRole(ctx)}, actor)
	if err != nil {
		return DepDTO{}, MapDepAddError(ctx, err, userID, fromID, toID, depType)
	}
	// gate travels in metadata; it is answered as its own field, as given.
	var gateOut *string
	if g, ok := dto.Metadata["gate"].(string); ok && g != "" {
		gg := g
		gateOut = &gg
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", tool,
		"userId", userID.String(),
		"fromId", dto.FromID, "toId", dto.ToID, "type", dto.Type,
		"latencyMs", time.Since(start).Milliseconds())
	return DepDTO{
		From: dto.FromID, To: dto.ToID, Type: dto.Type, Gate: gateOut,
		Metadata: dto.Metadata, Version: dto.Version, CreatedAt: dto.CreatedAt,
	}, nil
}

// MapDepAddError maps a refusal of dependencies.Store.Create to its code.
// The standing-edge guards (sk-118) are their own codes, not
// validation_error.
func MapDepAddError(ctx context.Context, err error, userID uuid.UUID, fromID, toID, depType string) *Failure {
	if vErr, ok := dependencies.IsValidationError(err); ok {
		switch vErr.Code {
		case ErrCodeNestedStandingForbidden:
			return Fail(ErrCodeNestedStandingForbidden, vErr.Message, map[string]any{"code": vErr.Code})
		case ErrCodeBlockerTargetStanding:
			return Fail(ErrCodeBlockerTargetStanding, vErr.Message,
				map[string]any{"code": vErr.Code, "toId": toID, "type": depType})
		}
		return Fail(ErrCodeValidationError, vErr.Message, map[string]any{"code": vErr.Code})
	}
	if cls := ClassifyDepStoreError(err, fromID, toID, depType); cls != nil {
		return Fail(cls.Code, cls.Message, cls.Details)
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_dep_add",
		"reason", "store create failed",
		"userId", userID.String(),
		"fromId", fromID, "toId", toID, "type", depType, "error", err.Error())
	return Fail(ErrCodeInternalError, "failed to create dependency", nil)
}

// DepValidationFailure is the refusal of a dependencies validator: the
// standing-edge guards keep their own codes, anything else is
// validation_error with the validator's code.
func DepValidationFailure(err error) *Failure {
	if ve, ok := dependencies.IsValidationError(err); ok {
		if ve.Code == ErrCodeNestedStandingForbidden || ve.Code == ErrCodeBlockerTargetStanding {
			return Fail(ve.Code, ve.Message, map[string]any{"code": ve.Code})
		}
		return Fail(ErrCodeValidationError, ve.Message, map[string]any{"code": ve.Code})
	}
	return Fail(ErrCodeValidationError, err.Error(), nil)
}

// DepRemoved is the acknowledgement of a removed edge.
type DepRemoved struct {
	FromID, ToID, Type string
}

// DepRemove removes one dependency edge (skein_dep_remove;
// removeDependency), written through FROM's project like DepAdd. No edge
// matching (fromId, toId, type) is dependency_not_found.
func DepRemove(ctx context.Context, d Deps, userID uuid.UUID, a Args) (DepRemoved, *Failure) {
	const tool = "skein_dep_remove"
	start := time.Now()
	fromID, toID, depType, f := depEnds(a)
	if f != nil {
		return DepRemoved{}, f
	}
	if f := depFromGate(ctx, d, tool, userID, fromID); f != nil {
		return DepRemoved{}, f
	}
	actor := DependenciesAgentActor(ctx, d.Pool, userID)
	if err := d.Deps.Delete(ctx, fromID, toID, depType, actor); err != nil {
		return DepRemoved{}, MapDepRemoveError(ctx, err, userID, fromID, toID, depType)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", tool,
		"userId", userID.String(),
		"fromId", fromID, "toId", toID, "type", depType,
		"latencyMs", time.Since(start).Milliseconds())
	return DepRemoved{FromID: fromID, ToID: toID, Type: depType}, nil
}

// MapDepRemoveError maps a refusal of dependencies.Store.Delete to its code:
// close_outcome_link_required with its details (sk-540.8), serialization_retry
// (nothing written), dependency_not_found. The standing-graph codes do not
// arise here: a delete re-validates no hierarchy.
func MapDepRemoveError(ctx context.Context, err error, userID uuid.UUID, fromID, toID, depType string) *Failure {
	if vErr, ok := dependencies.IsValidationError(err); ok {
		return Fail(ErrCodeValidationError, vErr.Message, map[string]any{"code": vErr.Code})
	}
	var linkErr *dependencies.ErrCloseOutcomeLinkRequired
	if errors.As(err, &linkErr) {
		return Fail(ErrCodeCloseOutcomeLinkRequired, linkErr.Error(), linkErr.Details())
	}
	if errors.Is(err, dependencies.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"dependency removal lost a serialization race — nothing was written, retry verbatim", nil)
	}
	if errors.Is(err, dependencies.ErrNotFound) {
		return Fail(ErrCodeDependencyNotFound, "no dependency edge matches (fromId, toId, type)",
			map[string]any{"fromId": fromID, "toId": toID, "type": depType})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_dep_remove",
		"reason", "store delete failed",
		"userId", userID.String(),
		"fromId", fromID, "toId", toID, "type", depType, "error", err.Error())
	return Fail(ErrCodeInternalError, "failed to remove dependency", nil)
}

package issueops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"strings"

	"github.com/google/uuid"
)

// ParseDetachHashes reads the `hashes` argument: a non-empty array of
// strings, capped at the same maximum the attach side uses.
//
// Shape errors are classified here rather than at the store boundary so
// the caller is told WHICH element was wrong. The hash CONTENT is left
// to the store's commitHashRegex — one validator, one message, whichever
// door the request came through.
func ParseDetachHashes(raw any) ([]string, *Failure) {
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, &Failure{
			Code:    ErrCodeValidationError,
			Message: "hashes must be an array",
		}
	}
	if len(rawSlice) == 0 {
		return nil, &Failure{
			Code:    ErrCodeValidationError,
			Message: "hashes must be non-empty",
		}
	}
	if len(rawSlice) > issues.MaxCommitsPerClose {
		return nil, &Failure{
			Code:    ErrCodeValidationError,
			Message: fmt.Sprintf("hashes may carry at most %d items (got %d)", issues.MaxCommitsPerClose, len(rawSlice)),
			Fields:  map[string]any{"max": issues.MaxCommitsPerClose, "got": len(rawSlice)},
		}
	}

	out := make([]string, 0, len(rawSlice))
	for i, entry := range rawSlice {
		s, ok := entry.(string)
		if !ok {
			return nil, &Failure{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("hashes[%d] must be a string", i),
				Fields:  map[string]any{"field": fmt.Sprintf("hashes[%d]", i)},
			}
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, &Failure{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("hashes[%d] is empty", i),
				Fields:  map[string]any{"field": fmt.Sprintf("hashes[%d]", i)},
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// MapAcceptError preserves the distinction between a transient abort and a
// genuine optimistic-version mismatch for the acceptance surface.
func MapAcceptError(ctx context.Context, err error, userID uuid.UUID, id string) *Failure {
	if code, message, ok := ContextStop(ctx, ctx, err); ok {
		return Fail(code, message, map[string]any{"id": id})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return Fail(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": id})
	}
	if errors.Is(err, issues.ErrCannotAcceptClosed) {
		return Fail(ErrCodeCannotAcceptClosed,
			"cannot accept a closed issue — reopen it first",
			map[string]any{"id": id})
	}
	if errors.Is(err, issues.ErrForbidden) {
		return Fail(ErrCodeForbidden,
			"only the issue's assignee can accept",
			map[string]any{"id": id})
	}
	if errors.Is(err, issues.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"the write transaction lost a serialization race and made no change; retry the request",
			map[string]any{"id": id})
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return Fail(ErrCodeVersionConflict,
			"the issue was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_accept",
		"reason", "store call failed",
		"userId", userID.String(), "issueId", id, "error", err.Error())
	return Fail(ErrCodeInternalError, "accept failed", nil)
}

// MapReparentError translates dependencies.Store.Reparent typed errors
// into stable MCP error codes. Every frozen sentinel from the domain
// layer is mapped here — none fall through to internal_error except a
// genuinely unrecognised error.
func MapReparentError(ctx context.Context, err error, userID uuid.UUID, id, newParentID string) *Failure {
	if vErr, ok := dependencies.IsValidationError(err); ok {
		// sk-118 (F2e): the nested-standing ban is a *ValidationError but
		// carries an actionable, branch-on-able code — promote it to the
		// top-level envelope `error` (matching the F2c issues-side promotion)
		// rather than burying it under validation_error.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return Fail(ErrCodeNestedStandingForbidden, vErr.Message,
				map[string]any{"id": id, "newParentId": newParentID})
		}
		return Fail(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var verr *dependencies.ErrVersionConflict
	if errors.As(err, &verr) {
		return Fail(ErrCodeVersionConflict,
			"the issue was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	if errors.Is(err, dependencies.ErrReparentSelf) {
		return Fail(ErrCodeReparentSelf,
			"an issue cannot be its own parent",
			map[string]any{"id": id})
	}
	if errors.Is(err, dependencies.ErrReparentCrossProject) {
		return Fail(ErrCodeReparentCrossProject,
			"new parent must be in the same project as the issue",
			map[string]any{"id": id, "newParentId": newParentID})
	}
	if errors.Is(err, dependencies.ErrFromNotFound) {
		return Fail(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": id})
	}
	// INV-3 (sk-674.18 T033): a parent in a project the caller cannot see
	// answers exactly as a nonexistent one — same code, text and details.
	if errors.Is(err, dependencies.ErrTargetNotFound) || errors.Is(err, dependencies.ErrDependencyTargetNotVisible) {
		return Fail(ErrCodeIssueNotFound,
			"new parent does not exist or is not visible",
			map[string]any{"newParentId": newParentID})
	}
	if errors.Is(err, dependencies.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"reparent lost a serialization race — safe to retry",
			map[string]any{"id": id})
	}
	if errors.Is(err, dependencies.ErrDependencyTargetNotReady) {
		return Fail(ErrCodeDependencyTargetNotVisibleYet,
			"new parent not visible yet — retry shortly",
			map[string]any{"newParentId": newParentID})
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(err, &cycle) {
		return Fail(ErrCodeDependencyCycle,
			"this move would create a cycle (new parent is a descendant of the issue)",
			map[string]any{"path": cycle.Path})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_reparent",
		"reason", "store reparent failed",
		"userId", userID.String(),
		"issueId", id, "newParentId", newParentID, "error", err.Error())
	return Fail(ErrCodeInternalError, "failed to reparent issue", nil)
}

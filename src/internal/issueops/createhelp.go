package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// HasInlineEdge reports whether the caller's inline dependencies already
// name the edge (toID, type).
func HasInlineEdge(deps []issues.DependencyEdgeInput, toID, depType string) bool {
	for _, d := range deps {
		if d.ToID == toID && d.Type == depType {
			return true
		}
	}
	return false
}

// MapDuplicateOfEdgeError renders a failure of the duplicateOf edge, which
// rides in the create's transaction as inline edge dupEdgeIndex (sk-607),
// with the code duplicateOf always answered for a target that is gone —
// validation_error — and, since sk-674.18 T033 (INV-3), the same answer
// for one the caller cannot see. Nothing was created — the edge failure rolled the issue back —
// so, unlike the old post-commit write, the details carry no issueId. nil
// when err is not that edge's failure.
func MapDuplicateOfEdgeError(ctx context.Context, err error, dupEdgeIndex int, duplicateOfID string, userID uuid.UUID) *Failure {
	var depFail *issues.ErrInlineDepFailed
	if dupEdgeIndex < 0 || !errors.As(err, &depFail) || depFail.Index != dupEdgeIndex {
		return nil
	}
	details := map[string]any{"duplicateOf": duplicateOfID}
	switch {
	case errors.Is(depFail.Err, dependencies.ErrTargetNotFound), errors.Is(depFail.Err, dependencies.ErrFromNotFound),
		// INV-3 (sk-674.18 T033): an invisible target answers as a nonexistent one.
		errors.Is(depFail.Err, dependencies.ErrDependencyTargetNotVisible):
		return Fail(ErrCodeValidationError, "duplicateOf target issue does not exist", details)
	}
	slog.ErrorContext(ctx, "issue operation error",
		"op", "create",
		"reason", "duplicates edge failed; the create was rolled back",
		"userId", userID.String(), "duplicateOf", duplicateOfID, "error", depFail.Err.Error())
	return Fail(ErrCodeInternalError,
		"failed to add the duplicates edge; nothing was created", details)
}

// MapCreateError translates issues.Store / dependencies typed errors into
// stable tool-error codes. Mirrors the shape of mapIssueErr in the REST
// handler but emits MCP error codes instead of HTTP statuses.
func MapCreateError(ctx context.Context, err error, userID, projectID uuid.UUID) *Failure {
	// sk-350.3: CreateIssue exhausting createMaxAttempts against a genuine
	// SERIALIZABLE abort now degrades to issues.ErrSerializationRetry (see
	// Store.CreateIssue) instead of leaking the raw pgconn.PgError through
	// to the ErrCodeInternalError default below — the same fix sk-303.17
	// already applied to skein_comment / skein_comment_update /
	// skein_comment_delete / skein_epic_create. Checked first, and NOT
	// mapped to ErrCodeVersionConflict: the issue was never created, so
	// there is no version for a re-read to quote.
	if errors.Is(err, issues.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"the create lost a serialization race and made no change — retry the call",
			map[string]any{"projectId": projectID.String()})
	}
	// sk-607: the caller's context ended inside the create's single
	// transaction — a stop, not a failure of the server.
	if code, message, ok := ContextStop(ctx, ctx, err); ok {
		return Fail(code, message, map[string]any{"projectId": projectID.String()})
	}
	// Inline-dep failure: surface the failing index + classified dep
	// error code so the agent can patch the offending dependencies[i]
	// without re-binding the whole call.
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		return AssigneeNotAllowed(map[string]any{"projectId": projectID.String()})
	}
	var depFail *issues.ErrInlineDepFailed
	if errors.As(err, &depFail) {
		return MapInlineDepError(ctx, depFail, userID, projectID)
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		// sk-118 (F2i / R7): the create-path nested-standing ban
		// (skein_create(parentId=<standing>, standing:true)) carries the
		// actionable `nested_standing_forbidden` code. Promote it to the
		// top-level envelope `error` — matching the inline-dep promotion in
		// MapInlineDepError and skein_dep_add — so agents branch on the
		// specific cause instead of a generic validation_error.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return Fail(ErrCodeNestedStandingForbidden, vErr.Message, nil)
		}
		return Fail(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrIDTaken) {
		return Fail(ErrCodeIDTaken,
			"an issue with this id already exists", nil)
	}
	var perr *issues.ErrParentNotEligible
	if errors.As(err, &perr) {
		switch perr.Code {
		case "parent_not_found":
			return Fail(ErrCodeParentNotFound, perr.Message, nil)
		case "parent_closed":
			return Fail(ErrCodeParentClosed, perr.Message, nil)
		case "parent_wrong_project":
			// sk-35: cross-project parents are allowed; this code path
			// is unreachable for the main store. Kept for back-compat
			// with any other caller of fetchParent that we might add.
			return Fail(ErrCodeParentWrongProject, perr.Message, nil)
		}
		return Fail(ErrCodeValidationError, perr.Message, nil)
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		return Fail(ErrCodeComponentsWrongProject,
			"some components do not belong to this project",
			map[string]any{"componentIds": cerr.ComponentIDs})
	}
	if errors.Is(err, issues.ErrHashCollisionExhausted) {
		return Fail(ErrCodeInternalError,
			"could not allocate a unique hash id; try again", nil)
	}
	slog.ErrorContext(ctx, "issue operation error",
		"op", "create",
		"reason", "create failed",
		"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
	return Fail(ErrCodeInternalError, "failed to create issue", nil)
}

// MapInlineDepError translates ErrInlineDepFailed into the stable MCP
// error envelope. Mirrors mapDepAddError but always carries
// `dependencyIndex`, `toId`, and `type` in details so the agent can
// retry the create with the offending entry removed / fixed without
// guessing which one failed.
func MapInlineDepError(ctx context.Context, depFail *issues.ErrInlineDepFailed, userID, projectID uuid.UUID) *Failure {
	inner := depFail.Err
	base := map[string]any{
		"dependencyIndex": depFail.Index,
		"toId":            depFail.ToID,
		"type":            depFail.Type,
	}
	if vErr, ok := dependencies.IsValidationError(inner); ok {
		base["code"] = vErr.Code
		// sk-118 (F2e/F2f): the standing-edge guards carry actionable,
		// branch-on-able codes — promote them to the top-level envelope `error`
		// (matching skein_dep_add / skein_dep_add_many) instead of burying them
		// under a generic validation_error. The inline-dep details
		// (dependencyIndex/toId/type/code) ride along in `base`.
		switch vErr.Code {
		case ErrCodeNestedStandingForbidden:
			return Fail(ErrCodeNestedStandingForbidden,
				"dependencies["+strconv.Itoa(depFail.Index)+"]: "+vErr.Message, base)
		case ErrCodeBlockerTargetStanding:
			return Fail(ErrCodeBlockerTargetStanding,
				"dependencies["+strconv.Itoa(depFail.Index)+"]: "+vErr.Message, base)
		}
		return Fail(ErrCodeValidationError,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: "+vErr.Message, base)
	}
	if errors.Is(inner, dependencies.ErrFromNotFound) {
		// Shouldn't happen on inline-deps (we just inserted FROM in the
		// same tx) — but surface clearly if it does.
		slog.ErrorContext(ctx, "inline dep create: from-issue missing in same tx",
			"userId", userID.String(), "projectId", projectID.String(),
			"index", depFail.Index, "toId", depFail.ToID)
		return Fail(ErrCodeInternalError,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: from-issue not visible inside its own tx",
			base)
	}
	// INV-3 (sk-674.18 T033): an invisible target answers as a nonexistent one.
	if errors.Is(inner, dependencies.ErrTargetNotFound) || errors.Is(inner, dependencies.ErrDependencyTargetNotVisible) {
		return Fail(ErrCodeIssueNotFound,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: target issue does not exist or is not visible",
			base)
	}
	if errors.Is(inner, dependencies.ErrDependencyExists) {
		return Fail(ErrCodeDependencyAlreadyExists,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: this dependency already exists",
			base)
	}
	if errors.Is(inner, dependencies.ErrParentAlreadySet) {
		// Defect 2 / sk-43: an inline parent-child edge cannot ADD a second
		// parent — point the caller at skein_reparent for the move.
		return Fail(ErrCodeParentAlreadySet,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: issue already has a parent — use skein_reparent to move it",
			base)
	}
	if errors.Is(inner, dependencies.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: dependency write lost a serialization race — safe to retry",
			base)
	}
	if errors.Is(inner, dependencies.ErrDependencyTargetNotReady) {
		return Fail(ErrCodeDependencyTargetNotVisibleYet,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: dependency target not visible yet — retry shortly",
			base)
	}
	var cycle *dependencies.ErrDepCycle
	if errors.As(inner, &cycle) {
		base["path"] = cycle.Path
		return Fail(ErrCodeDependencyCycle,
			"dependencies["+strconv.Itoa(depFail.Index)+"]: would create a cycle",
			base)
	}
	slog.ErrorContext(ctx, "issue operation error",
		"op", "create",
		"reason", "inline dep create failed",
		"userId", userID.String(), "projectId", projectID.String(),
		"index", depFail.Index, "toId", depFail.ToID, "type", depFail.Type,
		"error", inner.Error())
	return Fail(ErrCodeInternalError,
		"dependencies["+strconv.Itoa(depFail.Index)+"]: failed to create dependency", base)
}

// ParseInlineDeps reads + validates the `dependencies` array argument
// from the MCP tool request. Returns the parsed slice (possibly empty)
// or a populated *Failure error envelope ready to be
// returned by the handler.
//
// Validation is intentionally tight here so the failure points at the
// offending dependencies[i] field, not at the eventual store-level
// rejection. Cap, item shape, hierarchical-id shape, and dep-type/gate
// combos are all checked before the duplicate gate runs — a malformed
// inline-dep should never burn the embedding cost of duplicate
// detection.
func ParseInlineDeps(args map[string]any) ([]issues.DependencyEdgeInput, *Failure) {
	raw, ok := args["dependencies"]
	if !ok || raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, Fail(ErrCodeValidationError,
			"dependencies must be an array of {toId, type, gate?} objects",
			nil)
	}
	if len(rawSlice) == 0 {
		return nil, nil
	}
	if len(rawSlice) > InlineDepMaxItems {
		return nil, Fail(ErrCodeValidationError,
			"dependencies exceeds the per-call cap",
			map[string]any{"max": InlineDepMaxItems, "got": len(rawSlice)})
	}
	out := make([]issues.DependencyEdgeInput, 0, len(rawSlice))
	for i, entry := range rawSlice {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, Fail(ErrCodeValidationError,
				"dependencies["+strconv.Itoa(i)+"] must be an object",
				map[string]any{"dependencyIndex": i})
		}
		rawToID, _ := obj["toId"].(string)
		toID := strings.TrimSpace(rawToID)
		if !LooksLikeHierarchicalID(toID) {
			return nil, Fail(ErrCodeValidationError,
				"dependencies["+strconv.Itoa(i)+"].toId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"dependencyIndex": i, "toId": rawToID})
		}
		rawType, _ := obj["type"].(string)
		depType, vErr := dependencies.ValidateType(rawType)
		if vErr != nil {
			ve, _ := dependencies.IsValidationError(vErr)
			details := map[string]any{"dependencyIndex": i, "toId": toID}
			if ve != nil {
				details["code"] = ve.Code
			}
			msg := "dependencies[" + strconv.Itoa(i) + "]: " + vErr.Error()
			return nil, Fail(ErrCodeValidationError, msg, details)
		}
		var gateArg *string
		if g, ok := obj["gate"].(string); ok {
			if trimmed := strings.TrimSpace(g); trimmed != "" {
				gateArg = &trimmed
			}
		}
		if _, vErr := dependencies.ValidateGate(depType, gateArg); vErr != nil {
			ve, _ := dependencies.IsValidationError(vErr)
			details := map[string]any{"dependencyIndex": i, "toId": toID, "type": depType}
			if ve != nil {
				details["code"] = ve.Code
			}
			msg := "dependencies[" + strconv.Itoa(i) + "]: " + vErr.Error()
			return nil, Fail(ErrCodeValidationError, msg, details)
		}
		out = append(out, issues.DependencyEdgeInput{
			ToID: toID,
			Type: depType,
			Gate: gateArg,
		})
	}
	return out, nil
}

// InlineDepMaxItems caps how many edges a single create may write inline.
const InlineDepMaxItems = 50

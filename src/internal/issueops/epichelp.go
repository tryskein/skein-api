package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/issues"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ParseChildTemplates reads + validates the `childTemplates` array
// from the MCP tool request. Returns a slice of CreateIssueInput
// (possibly empty when the caller wants just the epic) or a populated
// *Failure error envelope ready for return.
//
// Validation is tight at the call boundary so the failure points at
// the offending childTemplates[i] field — the store-side errors don't
// know the original input index.
func ParseChildTemplates(args map[string]any) ([]issues.CreateIssueInput, *Failure) {
	raw, ok := args["childTemplates"]
	if !ok || raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, Fail(ErrCodeValidationError,
			"childTemplates must be an array of {title, type?, ...} objects",
			nil)
	}
	if len(rawSlice) == 0 {
		return nil, nil
	}
	if len(rawSlice) > EpicChildTemplateMaxItems {
		return nil, Fail(ErrCodeValidationError,
			"childTemplates exceeds the per-call cap",
			map[string]any{"max": EpicChildTemplateMaxItems, "got": len(rawSlice)})
	}
	out := make([]issues.CreateIssueInput, 0, len(rawSlice))
	for i, entry := range rawSlice {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, Fail(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"] must be an object",
				map[string]any{"childIndex": i})
		}
		rawTitle, _ := obj["title"].(string)
		title, vErr := issues.ValidateTitle(rawTitle)
		if vErr != nil {
			ve, _ := issues.IsValidationError(vErr)
			details := map[string]any{"childIndex": i}
			if ve != nil {
				details["code"] = ve.Code
			}
			return nil, Fail(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].title: "+vErr.Error(), details)
		}

		// Type defaults to task. Whitelist excludes 'epic' — nested
		// epics aren't a supported decomposition; agents needing
		// nested epics call skein_epic_create per epic and skein_create
		// for cross-epic deps.
		rawType, _ := obj["type"].(string)
		childType := strings.TrimSpace(rawType)
		if childType == "" {
			childType = issues.TypeTask
		}
		validType, vErr := issues.ValidateType(childType)
		if vErr != nil {
			ve, _ := issues.IsValidationError(vErr)
			details := map[string]any{"childIndex": i}
			if ve != nil {
				details["code"] = ve.Code
			}
			return nil, Fail(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].type: "+vErr.Error(), details)
		}
		if validType == issues.TypeEpic {
			return nil, Fail(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].type cannot be \"epic\" — nested epics are not supported via skein_epic_create",
				map[string]any{"childIndex": i})
		}

		// Priority — default 3 (match skein_create + epic default).
		var priority int
		if pf, ok := obj["priority"].(float64); ok {
			priority = int(pf)
		} else if pi, ok := obj["priority"].(int); ok {
			priority = pi
		} else {
			priority = 3
		}
		if vErr := issues.ValidatePriority(priority); vErr != nil {
			ve, _ := issues.IsValidationError(vErr)
			details := map[string]any{"childIndex": i}
			if ve != nil {
				details["code"] = ve.Code
			}
			return nil, Fail(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(i)+"].priority: "+vErr.Error(), details)
		}

		description, _ := obj["description"].(string)
		design, _ := obj["design"].(string)
		acceptance, _ := obj["acceptanceCriteria"].(string)
		notes, _ := obj["notes"].(string)

		// Guard each child's free-text fields (sk-28). One bad child
		// fails the whole batch — atomic with the parent epic.
		for _, pair := range [...]struct{ field, val string }{
			{"description", description},
			{"design", design},
			{"acceptanceCriteria", acceptance},
			{"notes", notes},
		} {
			if vErr := issues.ValidateProse(
				"childTemplates["+strconv.Itoa(i)+"]."+pair.field, pair.val,
			); vErr != nil {
				return nil, ValidationFailure(vErr)
			}
		}

		// sk-605: a non-string assignee is refused, naming the child.
		assigneeArg, assigneeErr := AssigneeFromArgs(obj,
			"childTemplates["+strconv.Itoa(i)+"].", map[string]any{"childIndex": i})
		if assigneeErr != nil {
			return nil, assigneeErr
		}
		// sk-565 (P9, US-5): the child's own `unassigned`, never the epic's.
		childAssignByDefault, modeErr := AssignCreatorByDefault(obj, assigneeArg,
			"childTemplates["+strconv.Itoa(i)+"].", map[string]any{"childIndex": i})
		if modeErr != nil {
			return nil, modeErr
		}

		// Per-child componentIds override. When absent (key missing on
		// the child object, or present-with-null), ComponentIDsExplicit
		// stays false and the store-level inheritance step fires — the
		// child inherits the epic's components (recorded just before
		// this insert in the same SERIALIZABLE tx). When present with a
		// non-null value (including an empty array), the explicit value
		// wins and no inheritance fires. A bare `null` is treated as
		// "caller said nothing" — same as omitting the key.
		idxCopy := i
		childComponentIDs, parseErr := ParseComponentIDs(obj, "componentIds", &idxCopy)
		if parseErr != nil {
			return nil, parseErr
		}
		childComponentIDsExplicit := false
		if v, exists := obj["componentIds"]; exists && v != nil {
			childComponentIDsExplicit = true
		}

		// Per-child labels. Absent key means no labels — the epic's own
		// labels are NOT inherited, unlike componentIds: a component says
		// where the work lives (shared by construction across a
		// decomposition), a label is an assertion about one issue.
		var childLabels []string
		if rawChildLabels, exists := obj["labels"]; exists && rawChildLabels != nil {
			rawSliceLabels, okLabels := rawChildLabels.([]any)
			if !okLabels {
				return nil, Fail(ErrCodeValidationError,
					"childTemplates["+strconv.Itoa(i)+"].labels must be an array of strings",
					map[string]any{"childIndex": i})
			}
			childLabels = make([]string, 0, len(rawSliceLabels))
			for j, rawLabel := range rawSliceLabels {
				str, okStr := rawLabel.(string)
				if !okStr {
					return nil, Fail(ErrCodeValidationError,
						"childTemplates["+strconv.Itoa(i)+"].labels["+strconv.Itoa(j)+"] must be a string",
						map[string]any{"childIndex": i, "labelIndex": j})
				}
				canon, vErr := issues.ValidateLabel(str)
				if vErr != nil {
					ve, _ := issues.IsValidationError(vErr)
					details := map[string]any{"childIndex": i}
					if ve != nil {
						details["code"] = ve.Code
					}
					return nil, Fail(ErrCodeValidationError,
						"childTemplates["+strconv.Itoa(i)+"].labels: "+vErr.Error(), details)
				}
				childLabels = append(childLabels, canon)
			}
		}

		out = append(out, issues.CreateIssueInput{
			Title:                title,
			Description:          description,
			Type:                 validType,
			Priority:             priority,
			Design:               design,
			AcceptanceCriteria:   acceptance,
			Notes:                notes,
			Assignee:             assigneeArg,
			ComponentIDs:         childComponentIDs,
			ComponentIDsExplicit: childComponentIDsExplicit,
			Labels:               childLabels,
			// sk-565 (P1).
			AssignCreatorByDefault: childAssignByDefault,
		})
	}
	return out, nil
}

// ParseComponentIDs reads a `componentIds` array of UUID strings from
// the supplied args map under `key`. Returns nil + nil when the key is
// absent (i.e. not supplied). Empty arrays return an empty non-nil
// slice so the caller can treat "explicit empty" as a real signal.
// indexHint, when non-nil, is folded into validation error details
// (used by the per-child path to surface `childIndex` in the typed
// error).
func ParseComponentIDs(args map[string]any, key string, indexHint *int) ([]uuid.UUID, *Failure) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		details := map[string]any{}
		if indexHint != nil {
			details["childIndex"] = *indexHint
		}
		return nil, Fail(ErrCodeValidationError,
			key+" must be an array of UUID strings", details)
	}
	if len(rawSlice) == 0 {
		return []uuid.UUID{}, nil
	}
	out := make([]uuid.UUID, 0, len(rawSlice))
	for i, entry := range rawSlice {
		s, ok := entry.(string)
		if !ok {
			details := map[string]any{"position": i}
			if indexHint != nil {
				details["childIndex"] = *indexHint
			}
			return nil, Fail(ErrCodeValidationError,
				key+"["+strconv.Itoa(i)+"] must be a UUID string", details)
		}
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			details := map[string]any{"position": i}
			if indexHint != nil {
				details["childIndex"] = *indexHint
			}
			return nil, Fail(ErrCodeValidationError,
				key+"["+strconv.Itoa(i)+"] is not a valid UUID", details)
		}
		out = append(out, id)
	}
	return out, nil
}

// MapEpicCreateError translates the layered errors from
// CreateEpicWithChildren into the stable MCP error envelope. When the
// failure was on a child template, we surface the index so the agent
// can fix that one entry without re-binding the whole call.
//
// sk-303.17 coderev: CreateEpicWithChildren lost EVERY
// epicCreateMaxAttempts retry against a genuine SERIALIZABLE abort used
// to fall straight through every branch below into the
// ErrCodeInternalError default — the exact sk-305 defect shape this
// diff's retry was meant to close, just pushed to the tail of the
// budget. issues.Store now degrades that exhaustion to
// issues.ErrSerializationRetry (neither the epic nor any child was ever
// committed, so there is no version to blame it on); map it to the SAME
// `serialization_retry` code Claim already uses for the identical
// situation. Checked FIRST, before the *ErrEpicChildFailed unwrap, since
// an exhausted retry arrives as the bare sentinel (the store already
// unwraps through ErrEpicChildFailed to detect it) — there is no
// meaningful childIndex to report for a whole-transaction abort.
func MapEpicCreateError(ctx context.Context, err error, userID, projectID interface{ String() string }) *Failure {
	if errors.Is(err, issues.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"epic create lost a serialization race and made no change — retry the call",
			map[string]any{"projectId": projectID.String()})
	}
	// sk-607: the caller's context ended inside the one transaction that
	// writes the epic and its children — a stop, not a failure. Checked
	// before the child unwrap: a child insert cut by the context is the
	// same stop, and its index says nothing about what to fix.
	if code, message, ok := ContextStop(ctx, ctx, err); ok {
		return Fail(code, message, map[string]any{"projectId": projectID.String()})
	}
	if errors.Is(err, issues.ErrAssigneeNotAllowed) && !errors.As(err, new(*issues.ErrEpicChildFailed)) {
		return AssigneeNotAllowed(map[string]any{"projectId": projectID.String()})
	}
	var childFail *issues.ErrEpicChildFailed
	if errors.As(err, &childFail) {
		inner := childFail.Err
		base := map[string]any{"childIndex": childFail.Index}
		if errors.Is(inner, issues.ErrAssigneeNotAllowed) {
			return AssigneeNotAllowed(base)
		}
		if vErr, ok := issues.IsValidationError(inner); ok {
			base["code"] = vErr.Code
			return Fail(ErrCodeValidationError,
				"childTemplates["+strconv.Itoa(childFail.Index)+"]: "+vErr.Message, base)
		}
		var cerr *issues.ErrComponentsWrongProject
		if errors.As(inner, &cerr) {
			base["componentIds"] = cerr.ComponentIDs
			return Fail(ErrCodeComponentsWrongProject,
				"childTemplates["+strconv.Itoa(childFail.Index)+"]: some components do not belong to this project", base)
		}
		if errors.Is(inner, issues.ErrIDTaken) {
			return Fail(ErrCodeIDTaken,
				"childTemplates["+strconv.Itoa(childFail.Index)+"]: an issue with this id already exists", base)
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", "skein_epic_create",
			"reason", "child create failed",
			"userId", userID.String(), "projectId", projectID.String(),
			"childIndex", childFail.Index, "error", inner.Error())
		return Fail(ErrCodeInternalError,
			"childTemplates["+strconv.Itoa(childFail.Index)+"]: failed to create child", base)
	}
	// Epic itself failed — map the same way create.go does.
	if vErr, ok := issues.IsValidationError(err); ok {
		// Same promotion create.go does: the nested-standing ban carries
		// an actionable code, so it must not be flattened into a generic
		// validation_error.
		if vErr.Code == ErrCodeNestedStandingForbidden {
			return Fail(ErrCodeNestedStandingForbidden, vErr.Message, nil)
		}
		return Fail(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	// Parent ineligible — reachable only since the epic accepts parentId.
	// Codes and their surfaces are create.go's, deliberately: two tools
	// that reject the same parent for the same reason must answer with
	// the same code.
	var perr *issues.ErrParentNotEligible
	if errors.As(err, &perr) {
		switch perr.Code {
		case "parent_not_found":
			return Fail(ErrCodeParentNotFound, perr.Message, nil)
		case "parent_closed":
			return Fail(ErrCodeParentClosed, perr.Message, nil)
		case "parent_wrong_project":
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
	if errors.Is(err, issues.ErrIDTaken) {
		return Fail(ErrCodeIDTaken,
			"an issue with this id already exists", nil)
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_epic_create",
		"reason", "epic create failed",
		"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
	return Fail(ErrCodeInternalError, "failed to create epic", nil)
}

// EpicChildTemplateMaxItems caps how many children one epic create writes.
const EpicChildTemplateMaxItems = 50

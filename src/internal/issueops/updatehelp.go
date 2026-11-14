package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/issues"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PatchTextValue preserves explicit null as a clear while refusing other
// non-string values before either update surface can write a field patch.
func PatchTextValue(field string, raw any) (string, *Failure) {
	s, isString := raw.(string)
	if raw != nil && !isString {
		return "", Fail(ErrCodeValidationError, field+" must be a string or null",
			map[string]any{"code": "text_field_not_string", "field": field})
	}
	if err := issues.ValidateProse(field, s); err != nil {
		return "", ValidationFailure(err)
	}
	return s, nil
}

// BuildPatchInputFromArgs is the args-map variant of buildPatchInput.
// Used by skein_update (via the wrapper above) and skein_update_many
// (which only has the per-item map[string]any in hand). Behaviour is
// identical — the single-tool tests assert this.
func BuildPatchInputFromArgs(args map[string]any, expectedVersion int) (issues.PatchIssueInput, bool, *Failure) {
	input := issues.PatchIssueInput{ExpectedVersion: expectedVersion}
	has := false

	// Only set a pointer when the key is present in the JSON. We do NOT
	// fall back to a defaulted-empty value — the empty default would
	// alias "not supplied" with "set to empty" and the patch path would
	// silently clear fields the caller didn't touch.
	if v, ok := args["title"]; ok {
		s, _ := v.(string)
		s = strings.TrimSpace(s)
		canon, vErr := issues.ValidateTitle(s)
		if vErr != nil {
			return input, false, ValidationFailure(vErr)
		}
		input.Title = &canon
		has = true
	}
	if v, ok := args["description"]; ok {
		s, vErr := PatchTextValue("description", v)
		if vErr != nil {
			return input, false, vErr
		}
		input.Description = &s
		has = true
	}
	if v, ok := args["type"]; ok {
		s, _ := v.(string)
		canon, vErr := issues.ValidateType(s)
		if vErr != nil {
			return input, false, ValidationFailure(vErr)
		}
		input.Type = &canon
		has = true
	}
	if v, ok := args["priority"]; ok {
		p := ReadIntFromAny(v, -1)
		if vErr := issues.ValidatePriority(p); vErr != nil {
			return input, false, ValidationFailure(vErr)
		}
		input.Priority = &p
		has = true
	}
	if v, ok := args["assignee"]; ok {
		// sk-621: null and "" both clear the assignee (the contract this
		// parser always had — null used to reach here as ""). Any other
		// non-string value is refused: read as "" it silently dropped the
		// assignee.
		s := ""
		if v != nil {
			str, isString := v.(string)
			if !isString {
				return input, false, AssigneeNotStringError(v, "", nil)
			}
			s = str
		}
		input.Assignee = &s
		has = true
	}
	if v, ok := args["design"]; ok {
		s, vErr := PatchTextValue("design", v)
		if vErr != nil {
			return input, false, vErr
		}
		input.Design = &s
		has = true
	}
	if v, ok := args["acceptanceCriteria"]; ok {
		s, vErr := PatchTextValue("acceptanceCriteria", v)
		if vErr != nil {
			return input, false, vErr
		}
		input.AcceptanceCriteria = &s
		has = true
	}
	if v, ok := args["notes"]; ok {
		s, vErr := PatchTextValue("notes", v)
		if vErr != nil {
			return input, false, vErr
		}
		input.Notes = &s
		has = true
	}
	if v, ok := args["estimatedMinutes"]; ok {
		i := ReadIntFromAny(v, 0)
		input.EstimatedMinutes = &i
		has = true
	}
	if v, ok := args["dueAt"]; ok && v != nil {
		s, _ := v.(string)
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			return input, false, Fail(ErrCodeValidationError,
				"dueAt must be RFC3339 timestamp",
				map[string]any{"dueAt": s})
		}
		input.DueAt = &t
		has = true
	}
	if v, ok := args["deferUntil"]; ok && v != nil {
		s, _ := v.(string)
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			return input, false, Fail(ErrCodeValidationError,
				"deferUntil must be RFC3339 timestamp",
				map[string]any{"deferUntil": s})
		}
		input.DeferUntil = &t
		has = true
	}
	if v, ok := args["externalRef"]; ok {
		s, vErr := PatchTextValue("externalRef", v)
		if vErr != nil {
			return input, false, vErr
		}
		input.ExternalRef = &s
		has = true
	}
	if v, ok := args["sourceContext"]; ok {
		s, vErr := PatchTextValue("sourceContext", v)
		if vErr != nil {
			return input, false, vErr
		}
		input.SourceContext = &s
		has = true
	}
	return input, has, nil
}

// ReadIntFromAny extracts an int from a JSON-decoded interface. mcp-go
// passes numbers through encoding/json, so they arrive as float64. The
// switch covers the alternate forms callers may construct directly in
// tests.
func ReadIntFromAny(v any, fallback int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return fallback
}

// MapUpdateError translates the typed errors emitted by issues.Store
// across PatchIssue / UpdateStatus / ReplaceComponents / AddLabel /
// RemoveLabel / Close into stable MCP error codes.
// StandingTransitionCode maps a ValidationError.Code emitted by the
// sk-118 (F2c) standing transition guards onto its top-level MCP envelope
// code. Returns (code, true) for the three standing guard codes; ("", false)
// for any other validation code (which stays under validation_error). Shared
// by mapUpdateError (single tool) and classifyUpdateError (batch) so both
// surfaces promote the same codes identically.
func StandingTransitionCode(vCode string) (string, bool) {
	switch vCode {
	case ErrCodeCannotPromoteLockedToStanding:
		return ErrCodeCannotPromoteLockedToStanding, true
	case ErrCodeCannotWorkStandingDirectly:
		return ErrCodeCannotWorkStandingDirectly, true
	case ErrCodeCannotDeferStanding:
		return ErrCodeCannotDeferStanding, true
	case ErrCodeNestedStandingForbidden:
		return ErrCodeNestedStandingForbidden, true
	default:
		return "", false
	}
}

func MapUpdateError(ctx context.Context, err error, userID uuid.UUID, issueID, closeTarget string) *Failure {
	if code, message, ok := ContextStop(ctx, ctx, err); ok {
		return Fail(code, message, map[string]any{"id": issueID})
	}
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		return AssigneeNotAllowed(map[string]any{"id": issueID})
	}
	if cls := ClassifyCloseTargetError(err, closeTarget); cls != nil {
		return Fail(cls.Code, cls.Message, cls.Details)
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		// sk-118 (F2c): the standing transition guards are *ValidationError
		// values but carry actionable, branch-on-able codes — promote them
		// to the top-level envelope `error` instead of the generic
		// validation_error so callers don't have to dig into detail.code.
		if code, ok := StandingTransitionCode(vErr.Code); ok {
			return Fail(code, vErr.Message, map[string]any{"id": issueID})
		}
		return Fail(ErrCodeValidationError, vErr.Message,
			map[string]any{"code": vErr.Code})
	}
	var labelErr *ErrLabelNotPresent
	if errors.As(err, &labelErr) {
		return Fail(ErrCodeValidationError,
			"label is not currently set on this issue",
			map[string]any{"label": labelErr.Label})
	}
	var verr *issues.ErrVersionConflict
	if errors.Is(err, issues.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"the write transaction lost a serialization race; nothing was written — resend the call unchanged",
			map[string]any{"id": issueID})
	}
	if errors.As(err, &verr) {
		return Fail(ErrCodeVersionConflict,
			"the issue was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	var lockErr *issues.ErrLockedByOther
	if errors.As(err, &lockErr) {
		return Fail(ErrCodeLockedByOther,
			"issue is locked by another session",
			map[string]any{"id": issueID, "lockedBy": lockErr.LockedBy})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return Fail(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": issueID})
	}
	if errors.Is(err, issues.ErrPinnedNoForce) {
		return Fail(ErrCodePinnedNoForce,
			"pinned issues require the dedicated unpin tool to be closed", nil)
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		return Fail(ErrCodeComponentsWrongProject,
			"some components do not belong to this project",
			map[string]any{"componentIds": cerr.ComponentIDs})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_update",
		"reason", "store call failed",
		"userId", userID.String(), "issueId", issueID, "error", err.Error())
	return Fail(ErrCodeInternalError, "update failed", nil)
}

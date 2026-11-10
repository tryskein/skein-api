package issueops

import (
	"skein-api/internal/issues"
	"strings"
)

// CloseOutcomeArgs — the parsed pair "outcome and target" of one call.
type CloseOutcomeArgs struct {
	// Outcome — nil means "the caller said not a word about the outcome".
	// A pointer exactly for this distinction: "not passed" and "passed
	// an empty string" have different consequences, and the second must be a refusal.
	Outcome *string
	// Target — an empty string = no target.
	Target string
}

// HasAny reports whether the caller mentioned at least one of the two fields.
func (a CloseOutcomeArgs) HasAny() bool {
	return a.Outcome != nil || a.Target != ""
}

// ParseCloseOutcomeArgs reads `closeOutcome` / `closeTarget` from the map
// of arguments and performs all the checks that need no database.
//
// `issueID` — the issue the call refers to; it catches a target
// equal to the issue itself. An empty string disables exactly this check (it is
// done by whoever knows the id of the position).
//
// `field` — a prefix of the field name in the error message: for the batch it is
// `items[i].`, for the single tool it is empty. The message is read by an agent, and
// "items[i].closeTarget" versus "closeTarget" is the difference between "some one of
// a hundred positions" and "my call".
//
// The error is returned in the batchErrorDTO form, because both sides need it:
// the batch puts it into results[i], the single tool unfolds it
// into the tool error envelope via closeOutcomeArgError.
func ParseCloseOutcomeArgs(args map[string]any, issueID, field string) (CloseOutcomeArgs, *Failure) {
	var out CloseOutcomeArgs

	if raw, present := args["closeOutcome"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return out, &Failure{
				Code:    ErrCodeValidationError,
				Message: field + "closeOutcome must be a string",
			}
		}
		// Not trimmed: the value is checked against a closed set of four, and
		// " done" is not "done with a space", it is not one of the four.
		// Silently fixing it here, we would accept an input that REST rejects on the
		// same value.
		out.Outcome = &s
	}
	if raw, present := args["closeTarget"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return out, &Failure{
				Code:    ErrCodeValidationError,
				Message: field + "closeTarget must be a string",
			}
		}
		out.Target = strings.TrimSpace(s)
	}

	if !out.HasAny() {
		return out, nil
	}

	// One resolver for all surfaces and both writers. The decision
	// is discarded deliberately: here it would be a decision for an issue
	// whose state we have not read yet, and the real one is made by the writer,
	// knowing the previous outcome and whether the issue is closed.
	if _, err := issues.DecideCloseOutcome(issues.CloseOutcomeRequest{
		IssueID:   issueID,
		Requested: out.Outcome,
		Target:    out.Target,
		// sk-540.2: whether duplicate/superseded may go without a target
		// depends on the row (already closed? edge present?), which is not
		// read yet — the writer enforces that rule.
		RowStateUnknown: true,
	}); err != nil {
		if ve, ok := issues.IsValidationError(err); ok {
			return out, &Failure{
				Code:    ErrCodeValidationError,
				Message: ve.Message,
				Fields:  map[string]any{"code": ve.Code},
			}
		}
		return out, &Failure{
			Code:    ErrCodeValidationError,
			Message: err.Error(),
		}
	}
	return out, nil
}

// CloseTargetDetails — the refusal detail for the target, shared by all surfaces.
// The key is exactly `closeTarget`, not `toId`: an agent that got a refusal must
// see WHICH of its arguments did not resolve, and it did not send `toId`.
func CloseTargetDetails(target string) map[string]any {
	return map[string]any{"closeTarget": target}
}

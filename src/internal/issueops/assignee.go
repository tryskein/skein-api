package issueops

// assignee.go — the assignee arguments of an issue write, shared by the MCP
// tools and the public API (sk-878.3.1 T024a).

import "strings"

const (
	UnassignedNotBooleanCode   = "unassigned_not_boolean"
	UnassignedWithAssigneeCode = "unassigned_with_assignee"
	AssigneeNotStringCode      = "assignee_not_string"
)

// AssigneeFromArgs parses `assignee` from one raw argument object (sk-605)
// and returns it trimmed, nil when absent, null or blank. A value of any
// other JSON type is refused. field and details are as for
// AssignCreatorByDefault.
func AssigneeFromArgs(obj map[string]any, field string, details map[string]any) (*string, *Failure) {
	raw, ok := obj["assignee"]
	if !ok || raw == nil {
		return nil, nil
	}
	s, isString := raw.(string)
	if !isString {
		return nil, AssigneeNotStringError(raw, field, details)
	}
	if trimmed := strings.TrimSpace(s); trimmed != "" {
		return &trimmed, nil
	}
	return nil, nil
}

// AssigneeNotStringError is the refusal of a non-string, non-null
// `assignee` (sk-605, sk-621): validation_error with code
// assignee_not_string and the offending value under "got". Shared by
// AssigneeFromArgs (create tools, skein_claim, list/ready filters) and the skein_update /
// skein_update_many patch parser, whose null and "" keep their own meaning
// («clear the assignee») and therefore cannot go through assigneeFromArgs.
func AssigneeNotStringError(raw any, field string, details map[string]any) *Failure {
	d := map[string]any{"code": AssigneeNotStringCode, "got": raw}
	for k, v := range details {
		d[k] = v
	}
	return Fail(ErrCodeValidationError, field+"assignee must be a string", d)
}

// AssignCreatorByDefault parses `unassigned` from one raw argument object
// and returns whether the creator's default applies (P1). assignee is the
// already-trimmed assignee (nil when absent or blank). field names the
// object in the error message ("" for the top level, "childTemplates[3]."
// for a child); details are merged into the refusal (e.g. childIndex).
func AssignCreatorByDefault(obj map[string]any, assignee *string, field string, details map[string]any) (bool, *Failure) {
	unassigned := false
	if raw, ok := obj["unassigned"]; ok && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			d := map[string]any{"code": UnassignedNotBooleanCode, "got": raw}
			for k, v := range details {
				d[k] = v
			}
			return false, Fail(ErrCodeValidationError,
				field+"unassigned must be a boolean", d)
		}
		unassigned = flag
	}
	if unassigned && assignee != nil {
		d := map[string]any{"code": UnassignedWithAssigneeCode}
		for k, v := range details {
			d[k] = v
		}
		return false, Fail(ErrCodeValidationError,
			field+"unassigned:true cannot be combined with a non-empty "+field+"assignee", d)
	}
	return !unassigned, nil
}

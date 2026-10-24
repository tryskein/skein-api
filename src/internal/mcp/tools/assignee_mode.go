package tools

// assignee_mode.go — sk-565 (US-4, D5, INV-2, INV-9, P9): the `unassigned`
// argument of skein_create, skein_epic_create and every
// skein_epic_create childTemplates[i], and the assignee mode it selects.
//
// «No assignee» is a separate boolean, not an empty or null `assignee`:
// models send an empty string by accident, and MCP clients do not tell
// null from an absent key (D5). So:
//   - absent, null, false — not passed: the creator's «assign to me»
//     setting decides (P1 is set);
//   - true — the issue stays unassigned whatever the setting says;
//   - any other type — validation_error, nothing created;
//   - true together with a non-empty (after trimming) `assignee` —
//     validation_error, nothing created.
//
// Unlike createReviewRequiredArg, null is accepted here: it means «not
// passed» (D5). The refusal is decided from the raw arguments, before any
// duplicate check or write.
//
// `assignee` itself (sk-605) is read from the same raw arguments by
// assigneeFromArgs: absent or null — not passed; a string — trimmed, blank
// is not passed; any other type — validation_error, nothing created.
// Reading it with GetString (or a bare type assertion) took a number,
// array, object or boolean for «not passed», and after sk-565 that put the
// issue on the key owner instead of telling the caller about the mistake.
// skein_claim reads `assignee` through the same function (sk-621): a
// non-string value used to mean «not passed» there too, and the issue went
// to the caller. skein_update / skein_update_many keep their own reading —
// null and "" clear the assignee — and share only the refusal
// (assigneeNotStringError).

import (
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	skmcp "skein-api/internal/mcp"
)

// Error detail codes of the assignee/unassigned refusals — machine-readable, so a caller
// (and a test) branches on the code, not on the message.
const (
	unassignedNotBooleanCode   = "unassigned_not_boolean"
	unassignedWithAssigneeCode = "unassigned_with_assignee"
	assigneeNotStringCode      = "assignee_not_string"
)

// assigneeFromArgs parses `assignee` from one raw argument object (sk-605)
// and returns it trimmed, nil when absent, null or blank. A value of any
// other JSON type is refused. field and details are as for
// assignCreatorByDefault.
func assigneeFromArgs(obj map[string]any, field string, details map[string]any) (*string, *mcpgo.CallToolResult) {
	raw, ok := obj["assignee"]
	if !ok || raw == nil {
		return nil, nil
	}
	s, isString := raw.(string)
	if !isString {
		return nil, assigneeNotStringError(raw, field, details)
	}
	if trimmed := strings.TrimSpace(s); trimmed != "" {
		return &trimmed, nil
	}
	return nil, nil
}

// assigneeNotStringError is the refusal of a non-string, non-null
// `assignee` (sk-605, sk-621): validation_error with code
// assignee_not_string and the offending value under "got". Shared by
// assigneeFromArgs (create tools, skein_claim) and the skein_update /
// skein_update_many patch parser, whose null and "" keep their own meaning
// («clear the assignee») and therefore cannot go through assigneeFromArgs.
func assigneeNotStringError(raw any, field string, details map[string]any) *mcpgo.CallToolResult {
	d := map[string]any{"code": assigneeNotStringCode, "got": raw}
	for k, v := range details {
		d[k] = v
	}
	return skmcp.ToolError(ErrCodeValidationError, field+"assignee must be a string", d)
}

// assignCreatorByDefault parses `unassigned` from one raw argument object
// and returns whether the creator's default applies (P1). assignee is the
// already-trimmed assignee (nil when absent or blank). field names the
// object in the error message ("" for the top level, "childTemplates[3]."
// for a child); details are merged into the refusal (e.g. childIndex).
func assignCreatorByDefault(obj map[string]any, assignee *string, field string, details map[string]any) (bool, *mcpgo.CallToolResult) {
	unassigned := false
	if raw, ok := obj["unassigned"]; ok && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			d := map[string]any{"code": unassignedNotBooleanCode, "got": raw}
			for k, v := range details {
				d[k] = v
			}
			return false, skmcp.ToolError(ErrCodeValidationError,
				field+"unassigned must be a boolean", d)
		}
		unassigned = flag
	}
	if unassigned && assignee != nil {
		d := map[string]any{"code": unassignedWithAssigneeCode}
		for k, v := range details {
			d[k] = v
		}
		return false, skmcp.ToolError(ErrCodeValidationError,
			field+"unassigned:true cannot be combined with a non-empty "+field+"assignee", d)
	}
	return !unassigned, nil
}

// unassignedParamDescription is the schema text of `unassigned` on
// skein_create and skein_epic_create.
const unassignedParamDescription = "Leave the issue without an assignee even when your personal " +
	"\"Assign issues my agent creates to me\" setting is on. Pass it when the user asks for an " +
	"unassigned issue. Cannot be combined with a non-empty `assignee`. Omitted, null or " +
	"false — the setting decides."

// assigneeDefaultParamDescription is the schema text of `assignee` on the
// create tools.
const assigneeDefaultParamDescription = "Assignee handle (username, email, or user UUID — " +
	"stored verbatim, resolved on read; it resolves only to a person who may work in the project). " +
	"A user UUID that does not name such a person is refused with assignee_not_allowed. Omitted or blank — your personal \"Assign " +
	"issues my agent creates to me\" setting decides (on by default: you become the assignee, " +
	"accepted), except for standing tasks and issues under someone else's parent. " +
	"null counts as omitted; a non-string value (number, array, object, boolean) is " +
	"refused with validation_error and nothing is created."

// assigneeDefaultNote is the paragraph the create-tool descriptions carry
// about the default (US-10).
const assigneeDefaultNote = "Assignee default: without `assignee`, the issue is assigned to " +
	"YOU (the API key's owner) and accepted, if your personal setting \"Assign issues my " +
	"agent creates to me\" is on (the default; switch it in the web UI notification settings). Not " +
	"for standing tasks, and not under someone else's parent: when the nearest assigned " +
	"ancestor is another person, or one you cannot see, the issue stays unassigned — " +
	"see skein_get_instructions. Pass `unassigned: true` when the user " +
	"asks for an unassigned issue; to drop the assignee later, clear it with skein_update " +
	"(assignee=\"\"). Being the assignee is not a lock: skein_claim is still needed to start work."

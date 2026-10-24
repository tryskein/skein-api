// Package textvalidate guards free-text fields against agent tool-call
// wire-format leakage. Claude Code and similar agent clients parse their
// tool-call format (XML-like `<function_calls>` / `<invoke>` / `<parameter>`)
// with naive regex tokenisation — when a literal wrapper substring appears
// inside a string VALUE rather than as a structural separator, the parser
// silently truncates or corrupts the call. The agent then stores the broken
// data and only notices when a human reads the field.
//
// Server-side guard: scan inbound string values for known wrapper literals
// and reject the call with a stable error code so the agent can self-correct.
package textvalidate

import (
	"fmt"
	"strings"
)

// ToolCallSyntaxCode is the wire-stable error code returned when a value
// contains tool-call wrapper literals. Never change without coordinating
// with MCP clients — agents branch on this exact string.
const ToolCallSyntaxCode = "tool_call_syntax_leaked"

// ToolCallSyntaxError is returned by ScanToolCallSyntax when a value
// contains a known wrapper literal. Field is the logical field name passed
// by the caller (e.g. "description"); Needle is the exact substring that
// matched, so the agent can grep its draft and fix the right spot.
type ToolCallSyntaxError struct {
	Field  string
	Needle string
}

func (e *ToolCallSyntaxError) Error() string {
	return fmt.Sprintf(
		"field %q contains tool-call wrapper literal %q — your XML tool-call format leaked into the value. "+
			"The usual cause is a parameter block closed with the wrong tag (e.g. a closing tag named after the "+
			"field instead of the generic parameter close), which glues the next parameter onto this one: "+
			"check how you closed the parameter BEFORE %s, then resend",
		e.Field, e.Needle, e.Field,
	)
}

// needles is the closed set of substrings that signal wire-format leakage
// into free-text fields (tool-call XML wrappers). Agents that hit
// tool_call_syntax_leaked should strip these before retrying. Case-sensitive
// on purpose — the wire format is case-sensitive too.
//
//	needle                 matches
//	---------------------  ------------------------------------------------
//	<function_calls>       opening wrapper tag (exact)
//	</function_calls>      closing wrapper tag (exact)
//	<invoke                both <invoke> and <invoke name="…">
//	</invoke>              closing invoke tag
//	<parameter name=       parametrised opening tag (not bare <parameter>)
//	</parameter>           closing parameter tag
//
// The same table is published to MCP clients in the agent guide
// (internal/mcp/instructions/full.md, "Text fields — forbidden literals"),
// which is what skein_get_instructions returns, so client authors can
// preflight instead of learning the set from a rejection.
//
// Deliberately UNEXPORTED and never aliased: this slice is the validator's
// backing store, so an exported handle would let any package in the module
// mutate the rule set at runtime — silently disabling the check, and racing
// with concurrent ScanToolCallSyntax calls. Callers that need the list get
// it from the guide above, not from a package variable.
var needles = []string{
	"<function_calls>",
	"</function_calls>",
	"<invoke",
	"</invoke>",
	"<parameter name=",
	"</parameter>",
}

// ScanToolCallSyntax returns *ToolCallSyntaxError when value contains any of
// the forbidden needles. Returns nil for empty or clean strings. Returns on
// the first match — order of needles in the slice is the search order but
// callers should not rely on it.
func ScanToolCallSyntax(field, value string) error {
	if value == "" {
		return nil
	}
	for _, n := range needles {
		if strings.Contains(value, n) {
			return &ToolCallSyntaxError{Field: field, Needle: n}
		}
	}
	return nil
}

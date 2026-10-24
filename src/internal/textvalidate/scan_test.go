package textvalidate

import (
	"errors"
	"strings"
	"testing"

	"skein-api/internal/mcp/instructions"
)

func TestScanToolCallSyntax_EachNeedleDetected(t *testing.T) {
	tests := []struct {
		name   string
		needle string
	}{
		{"function_calls_open", "<function_calls>"},
		{"function_calls_close", "</function_calls>"},
		{"invoke_open", "<invoke"},
		{"invoke_close", "</invoke>"},
		{"parameter_open", "<parameter name="},
		{"parameter_close", "</parameter>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ScanToolCallSyntax("description", tt.needle)
			if err == nil {
				t.Fatalf("expected error for needle %q, got nil", tt.needle)
			}
			var tcErr *ToolCallSyntaxError
			if !errors.As(err, &tcErr) {
				t.Fatalf("expected *ToolCallSyntaxError, got %T", err)
			}
			if tcErr.Field != "description" {
				t.Errorf("Field = %q, want %q", tcErr.Field, "description")
			}
			if tcErr.Needle != tt.needle {
				t.Errorf("Needle = %q, want %q", tcErr.Needle, tt.needle)
			}
		})
	}
}

func TestScanToolCallSyntax_NeedleEmbeddedInProse(t *testing.T) {
	value := "## My doc\n\nSome text...</parameter>\n\nMore text"
	err := ScanToolCallSyntax("description", value)
	if err == nil {
		t.Fatal("expected error for prose containing </parameter>, got nil")
	}
	var tcErr *ToolCallSyntaxError
	if !errors.As(err, &tcErr) {
		t.Fatalf("expected *ToolCallSyntaxError, got %T", err)
	}
	if tcErr.Needle != "</parameter>" {
		t.Errorf("Needle = %q, want %q", tcErr.Needle, "</parameter>")
	}
}

func TestScanToolCallSyntax_CleanText_NoError(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"plain_ascii_markdown", "# Heading\n\nA paragraph with *emphasis* and `code`."},
		{"cyrillic", "Привет, мир! Это описание задачи на русском языке."},
		{"html_div_span", "```html\n<div class=\"foo\"><span>hello</span></div>\n```"},
		{"multi_line", "Line one\nLine two\nLine three\n\nParagraph break."},
		{"json_looking", `{"key": "value", "nested": {"a": 1}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ScanToolCallSyntax("description", tt.value); err != nil {
				t.Errorf("expected nil error, got %v", err)
			}
		})
	}
}

func TestScanToolCallSyntax_EmptyString_NoError(t *testing.T) {
	if err := ScanToolCallSyntax("description", ""); err != nil {
		t.Errorf("expected nil error for empty string, got %v", err)
	}
}

// TestScanToolCallSyntax_CaseSensitive — wire format is case-sensitive;
// uppercase/mixed-case variants must not trigger so prose mentioning
// "Parameter" or "Invoke" in human contexts isn't a false positive.
func TestScanToolCallSyntax_CaseSensitive(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"uppercase_parameter_close", "</PARAMETER>"},
		{"mixed_case_invoke_open", "<Invoke"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ScanToolCallSyntax("description", tt.value); err != nil {
				t.Errorf("expected nil error for %q (case-sensitive match), got %v", tt.value, err)
			}
		})
	}
}

func TestScanToolCallSyntax_FieldNamePropagated(t *testing.T) {
	err := ScanToolCallSyntax("notes", "leaked </invoke> in notes")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var tcErr *ToolCallSyntaxError
	if !errors.As(err, &tcErr) {
		t.Fatalf("expected *ToolCallSyntaxError, got %T", err)
	}
	if tcErr.Field != "notes" {
		t.Errorf("Field = %q, want %q", tcErr.Field, "notes")
	}
}

// sk-275: the message names the MECHANISM as well as the literal. The
// literal the agent is shown is one it did not type — it arrived because a
// parameter block was closed with the wrong tag — so a message that only
// says "strip the wrapper tags" sends the agent hunting through prose that
// contains nothing to strip, and it retries the same malformed call.
func TestToolCallSyntaxError_ErrorMessage(t *testing.T) {
	e := &ToolCallSyntaxError{Field: "body", Needle: "</invoke>"}
	got := e.Error()

	for _, want := range []string{
		`field "body"`,
		`literal "</invoke>"`,
		"leaked into the value",
		"closed with the wrong tag",
		"BEFORE body",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q\nmissing %q", got, want)
		}
	}

	// The message must not itself contain a forbidden literal beyond the
	// quoted needle: it is copied into agent transcripts and, worse, into
	// issue prose — where THIS validator would then reject it.
	stripped := strings.Replace(got, e.Needle, "", 1)
	if err := ScanToolCallSyntax("message", stripped); err != nil {
		t.Errorf("the rejection message is itself unwritable prose: %v", err)
	}
}

func TestToolCallSyntaxCode_Constant(t *testing.T) {
	if ToolCallSyntaxCode != "tool_call_syntax_leaked" {
		t.Errorf("ToolCallSyntaxCode = %q, want %q", ToolCallSyntaxCode, "tool_call_syntax_leaked")
	}
}

func TestScanToolCallSyntax_ReturnsTypedError(t *testing.T) {
	err := ScanToolCallSyntax("description", "<function_calls>")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var tcErr *ToolCallSyntaxError
	if !errors.As(err, &tcErr) {
		t.Fatalf("errors.As to *ToolCallSyntaxError failed, got %T", err)
	}
}

// TestNeedles_MatchGuideTable binds the validator's rejected set to the
// table skein_get_instructions publishes, in both directions.
//
// This is the guard the sk-255.10 defect went through: skein_create and
// skein_update told callers `</description>` and `</design>` were
// rejected, the guide's table did not list them, and nothing failed
// because nothing compared the two. The schemas now carry a pointer to
// the guide instead of their own copy, which makes the guide the single
// statement of the contract — and a single statement that can silently
// drift from the code it describes is worse than the duplicate it
// replaced.
//
// Direction 1 (needle → table): a literal the validator rejects but the
// guide omits is a rejection agents cannot preflight.
// Direction 2 (table → needle): a row the validator does not scan is the
// original defect verbatim — a promise of protection that does not exist.
func TestNeedles_MatchGuideTable(t *testing.T) {
	const heading = "## Text fields — forbidden literals"

	start := strings.Index(instructions.FullGuide, heading)
	if start < 0 {
		t.Fatalf("full.md is missing the %q section — the MCP tool schemas "+
			"point callers at it and carry no copy of the literal set", heading)
	}
	section := instructions.FullGuide[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	// Table rows look like "| `<invoke` | both bare … |". Only the first
	// backticked cell is the literal; the prose around the table is not
	// pipe-prefixed, so it cannot leak in.
	documented := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		rest := line[len("| `"):]
		close := strings.Index(rest, "`")
		if close < 0 {
			t.Errorf("malformed table row in %q section: %q", heading, line)
			continue
		}
		documented[rest[:close]] = true
	}

	for _, n := range needles {
		if !documented[n] {
			t.Errorf("validator rejects %q but the guide table does not list it — "+
				"agents cannot preflight a literal they are never shown", n)
		}
	}
	scanned := make(map[string]bool, len(needles))
	for _, n := range needles {
		scanned[n] = true
	}
	for literal := range documented {
		if !scanned[literal] {
			t.Errorf("guide table lists %q as rejected but the validator does not "+
				"scan for it — values carrying it are accepted and stored", literal)
		}
	}
}

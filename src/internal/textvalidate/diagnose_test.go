package textvalidate

import (
	"strings"
	"testing"
)

// The shape the incident behind sk-275 actually produced: a parameter
// block closed with the FIELD NAME instead of the generic parameter close,
// so the client never terminated the value and appended the next
// parameter's opening tag to it. The forbidden literal therefore sits at
// the very END of the value, at a seam — not in the middle of prose.
const misclosedReason = "Done: rewrote the handler and added a regression test.\n</reason>\n" +
	"<parameter name=\"commits\">"

func TestDiagnoseArguments_LocatesTheSeam(t *testing.T) {
	args := map[string]any{
		"id":              "sk-42",
		"expectedVersion": float64(7),
		"reason":          misclosedReason,
	}
	d := DiagnoseArguments(args)

	// Two string leaves: id and reason. expectedVersion is a number.
	if d.TextFields != 2 {
		t.Errorf("TextFields = %d, want 2", d.TextFields)
	}
	if d.FindingsTotal != 1 || len(d.Findings) != 1 {
		t.Fatalf("findings = %d (total %d), want exactly 1", len(d.Findings), d.FindingsTotal)
	}
	f := d.Findings[0]
	if f.Field != "reason" {
		t.Errorf("Field = %q, want reason", f.Field)
	}
	if f.Needle != "<parameter name=" {
		t.Errorf("Needle = %q, want the parametrised opening tag", f.Needle)
	}
	if f.ValueLen != len(misclosedReason) {
		t.Errorf("ValueLen = %d, want %d", f.ValueLen, len(misclosedReason))
	}
	// The offset is what separates "glued at a seam" from "written in the
	// middle of prose" — the whole point of recording it.
	if f.Offset != strings.Index(misclosedReason, "<parameter name=") {
		t.Errorf("Offset = %d, want %d", f.Offset, strings.Index(misclosedReason, "<parameter name="))
	}
	// The window shows the SEAM, not the prose (see redactWindow): the
	// wrong closing tag named after the field, then the next parameter's
	// opening tag glued straight onto it. That shape is the whole
	// diagnosis, and it survives redaction intact.
	if !strings.Contains(f.Snippet, "\n</aaaaaa>\n<parameter name=") {
		t.Errorf("snippet does not show the seam: %q", f.Snippet)
	}
	if strings.Contains(f.Snippet, "regression test") {
		t.Errorf("snippet reproduced the caller's prose: %q", f.Snippet)
	}
}

// The earliest match wins across the WHOLE needle set — a diagnosis that
// reported a later literal would misplace the seam it claims to locate.
func TestDiagnoseArguments_ReportsEarliestMatch(t *testing.T) {
	v := "a </invoke> b <function_calls> c"
	d := DiagnoseArguments(map[string]any{"description": v})
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(d.Findings))
	}
	if d.Findings[0].Needle != "</invoke>" {
		t.Errorf("Needle = %q, want the earliest match </invoke>", d.Findings[0].Needle)
	}
}

// The window is bounded — prose fields run to tens of kilobytes and the
// log line must stay readable.
func TestDiagnoseArguments_SnippetIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 40_000) + "</parameter>" + strings.Repeat("y", 40_000)
	d := DiagnoseArguments(map[string]any{"notes": huge})
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(d.Findings))
	}
	f := d.Findings[0]
	if f.ValueLen != len(huge) {
		t.Errorf("ValueLen = %d, want %d", f.ValueLen, len(huge))
	}
	// needle + both radii + the two ellipsis runes, generously bounded.
	max := len("</parameter>") + 2*SnippetRadius + 8
	if len(f.Snippet) > max {
		t.Errorf("snippet is %d bytes, want <= %d — the point is NOT to log the value", len(f.Snippet), max)
	}
	if !strings.HasPrefix(f.Snippet, "…") || !strings.HasSuffix(f.Snippet, "…") {
		t.Errorf("snippet should be elided on both sides, got %q", f.Snippet)
	}
}

// Cutting a multi-byte draft at an arbitrary byte offset would emit invalid
// UTF-8 and the log line would render as escape soup.
func TestDiagnoseArguments_SnippetIsRuneSafe(t *testing.T) {
	v := strings.Repeat("ы", 200) + "</invoke>" + strings.Repeat("ж", 200)
	d := DiagnoseArguments(map[string]any{"design": v})
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(d.Findings))
	}
	s := d.Findings[0].Snippet
	for i, r := range s {
		if r == '�' {
			t.Fatalf("snippet contains an invalid rune at %d: %q", i, s)
		}
	}
}

// Nested arguments (skein_epic_create children, inline dependencies) carry
// prose too, and a path that only named the top-level key would point at
// the wrong place.
func TestDiagnoseArguments_WalksNestedValues(t *testing.T) {
	args := map[string]any{
		"title": "epic",
		"children": []any{
			map[string]any{"title": "one", "description": "clean"},
			map[string]any{"title": "two", "description": "oops </parameter>"},
		},
	}
	d := DiagnoseArguments(args)
	if d.TextFields != 5 {
		t.Errorf("TextFields = %d, want 5", d.TextFields)
	}
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(d.Findings))
	}
	if got := d.Findings[0].Field; got != "children[1].description" {
		t.Errorf("Field = %q, want children[1].description", got)
	}
}

// A clean call produces the field count and nothing else — the diagnosis is
// only ever LOGGED on a rejection, but it must be honest when asked.
func TestDiagnoseArguments_CleanCall(t *testing.T) {
	d := DiagnoseArguments(map[string]any{"id": "sk-1", "reason": "all good"})
	if d.TextFields != 2 || d.FindingsTotal != 0 || len(d.Findings) != 0 {
		t.Errorf("clean call diagnosed as %+v", d)
	}
}

// --- sk-243 security follow-up (F2) ---------------------------------------

// TestDiagnoseArguments_SnippetCarriesNoContent is the confidentiality
// boundary of the diagnostic.
//
// The window is cut out of a FREE-TEXT field. Free text is whatever the
// caller pasted, and what people paste into a task description at 2am
// includes API keys, connection strings and private URLs. The record needs
// the SHAPE of the seam — punctuation, tag structure, where the literal
// sits — and never needed the words, so it must not carry them.
func TestDiagnoseArguments_SnippetCarriesNoContent(t *testing.T) {
	const secret = "ska_live_xxxxxxxxxxxxxx"
	v := "deploy key " + secret + " </parameter>\n<parameter name=\"commits\">"

	d := DiagnoseArguments(map[string]any{"reason": v})
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(d.Findings))
	}
	s := d.Findings[0].Snippet

	if strings.Contains(s, secret) {
		t.Errorf("the window reproduced a secret verbatim: %q", s)
	}
	// Not just the literal secret: no alphanumeric run from the value may
	// survive, or the next secret with a different shape leaks anyway.
	for _, word := range []string{"deploy", "commits", "9Zq7TdKm42PwXr"} {
		if strings.Contains(s, word) {
			t.Errorf("the window reproduced %q from the value: %q", word, s)
		}
	}
	// …while the structure that makes the record worth writing survives:
	// the literal itself, and the punctuation that shows it sat at a seam.
	if !strings.Contains(s, "</parameter>") {
		t.Errorf("the window lost the literal it exists to locate: %q", s)
	}
	if !strings.Contains(s, `<`) || !strings.Contains(s, `"`) || !strings.Contains(s, "\n") {
		t.Errorf("the window lost the seam structure: %q", s)
	}
}

// Non-ASCII prose is content too — a Cyrillic draft must be masked, not
// passed through because the masking table only knew about ASCII.
func TestDiagnoseArguments_SnippetMasksNonASCIIContent(t *testing.T) {
	v := "пароль от базы hunter2 </invoke>"
	d := DiagnoseArguments(map[string]any{"notes": v})
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(d.Findings))
	}
	s := d.Findings[0].Snippet
	for _, word := range []string{"пароль", "базы", "hunter2"} {
		if strings.Contains(s, word) {
			t.Errorf("the window reproduced %q: %q", word, s)
		}
	}
	if !strings.Contains(s, "</invoke>") {
		t.Errorf("the window lost the literal: %q", s)
	}
}

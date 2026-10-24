package textvalidate

// diagnose.go — the sk-275 attribution record.
//
// # The question this exists to answer
//
// A `tool_call_syntax_leaked` rejection has two possible authors and the
// rejection itself names neither: either the CLIENT glued a neighbouring
// parameter into the value (its wire format leaked, and the literal is
// physically present in what arrived), or the VALIDATOR matched something
// the caller legitimately wrote (a false positive, and the rule is too
// wide). Four refusals were once reported across three agents in a single
// session and the case was closed as "unknown" because nobody had looked
// at the inbound request.
//
// One reproduction has since settled it — a parameter block closed with
// `</reason>` instead of `</parameter>`, which made the client append the
// NEXT parameter's opening tag to the reason text — so the rule stays as
// it is. What this file adds is the record that makes the next occurrence
// answerable from data instead of from argument: the literal, a bounded
// window of the text AROUND it, the field it landed in, and how many text
// fields the call carried. A leak shows the wrapper sitting at a seam
// between two values; a false positive shows it inside ordinary prose.
//
// # Why a window and not the value
//
// Prose fields run to tens of kilobytes. A log line that carried the whole
// value would be unreadable, would dominate the log volume of the very
// incident it documents, and would copy the caller's entire draft into a
// second system. The window is what the question needs and no more.

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SnippetRadius is how much text is kept on each side of a matched
// literal. Wide enough to show the seam — the tail of the value that was
// being written and the head of the one that was glued onto it — and
// narrow enough that the whole line stays readable.
const SnippetRadius = 60

// maxFindings caps how many offending fields one diagnosis reports. A
// malformed call normally has exactly one seam; a caller that manages more
// than a handful has a wholesale client problem, and the first few are
// enough to say so.
const maxFindings = 5

// Finding is one leaked literal, located.
type Finding struct {
	// Field is the path of the offending value inside the call's
	// arguments — "description", or "dependencies[1].reason" for a value
	// nested in a structured argument.
	Field string
	// Needle is the literal that matched (one of the closed set).
	Needle string
	// Offset is the byte offset of the match within the value. Read with
	// ValueLen it says WHERE the seam is: a match at the very end is the
	// signature of a mis-closed parameter block, one in the middle is
	// what a genuine false positive would look like.
	Offset int
	// ValueLen is the length of the whole value, in bytes.
	ValueLen int
	// Snippet is the bounded window around the match, elided with "…" on
	// whichever side was cut — and REDACTED: every letter and digit
	// outside the matched literal is replaced by a placeholder rune.
	//
	// The window is cut out of a free-text field, which is to say out of
	// whatever the caller pasted. The record needs the SHAPE of the seam
	// (tag structure, quoting, where the literal sits relative to the
	// punctuation around it) and has never needed the words, so it does
	// not carry them. See redactWindow.
	Snippet string
}

// Diagnosis is the whole record for one rejected call.
type Diagnosis struct {
	// TextFields counts the string leaves in the call's arguments — the
	// axis the original report named ("two and more text fields in one
	// call"). Counted after the walk, so nested strings count too.
	TextFields int
	// Findings is the located leaks, capped at maxFindings.
	Findings []Finding
	// FindingsTotal is how many there were before the cap.
	FindingsTotal int
}

// DiagnoseArguments walks a tool call's arguments and reports every string
// leaf that carries a forbidden literal.
//
// It re-scans the ARGUMENTS rather than reading the rejection, because the
// arguments are the evidence: they are what the server actually received
// and parsed, which is precisely the thing nobody had looked at. The scan
// is the same closed needle set the validator uses — one source of truth,
// so the diagnosis can never describe a rule the guard does not apply.
//
// Traversal is depth-first over maps and slices; map keys are visited in
// sorted order so two runs of the same call log the same line.
func DiagnoseArguments(args map[string]any) Diagnosis {
	var d Diagnosis
	walkStrings(args, "", func(path, value string) {
		d.TextFields++
		off, needle, ok := findNeedle(value)
		if !ok {
			return
		}
		d.FindingsTotal++
		if len(d.Findings) >= maxFindings {
			return
		}
		d.Findings = append(d.Findings, Finding{
			Field:    path,
			Needle:   needle,
			Offset:   off,
			ValueLen: len(value),
			Snippet:  snippet(value, off, len(needle)),
		})
	})
	return d
}

// findNeedle returns the EARLIEST match in value across the whole needle
// set, not the first needle that happens to hit. Order matters here in a
// way it does not for the guard: the guard only answers yes/no, while the
// diagnosis is claiming "this is where the seam is", and reporting a later
// match as the location would misplace it.
func findNeedle(value string) (offset int, needle string, ok bool) {
	best := -1
	for _, n := range needles {
		i := strings.Index(value, n)
		if i < 0 {
			continue
		}
		if best < 0 || i < best || (i == best && len(n) > len(needle)) {
			best, needle = i, n
		}
	}
	if best < 0 {
		return 0, "", false
	}
	return best, needle, true
}

// snippet cuts a window of SnippetRadius bytes either side of the match and
// redacts everything in it except the matched literal.
//
// Both cuts are pulled back to a UTF-8 rune boundary. Slicing a Cyrillic
// or emoji-bearing draft at an arbitrary byte offset yields invalid UTF-8,
// which the log handler then renders as escape soup — the one thing a
// human reading an incident line cannot skim.
//
// # Why the window is redacted and not merely bounded
//
// This record goes to WARN, in production, on text the caller authored and
// nobody reviewed. A bound answers "how much" and says nothing about
// "what": 120 bytes of a description field is still 120 bytes of whatever
// the caller pasted, and people paste API keys, connection strings and
// private URLs into task text. The log handler cannot save us here —
// logging.DualHandler redacts by ATTRIBUTE KEY (password, token, secret,
// …), so a window travelling under the key `snippet` is invisible to it by
// construction.
//
// So the window is made safe where it is BUILT rather than where it is
// written: letters and digits become placeholders, everything else — ASCII
// punctuation, quoting, whitespace, and the matched literal itself —
// survives verbatim. That is precisely the evidence sk-275 asked for. A
// mis-closed parameter block reads as `aaaa aaa"></aaaaaa><parameter name=`
// — the seam is plain — while the same literal inside ordinary prose reads
// as `aaaa aaaaaa, </parameter> aaa aaaa.` The words were never the
// distinguishing feature; the punctuation is.
func snippet(value string, offset, needleLen int) string {
	start := offset - SnippetRadius
	if start < 0 {
		start = 0
	}
	for start > 0 && !utf8.RuneStart(value[start]) {
		start--
	}
	end := offset + needleLen + SnippetRadius
	if end > len(value) {
		end = len(value)
	}
	for end < len(value) && !utf8.RuneStart(value[end]) {
		end++
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(redactWindow(value[start:offset]))
	// The literal is the one thing in the window that is ours, not the
	// caller's: it comes from the closed needle set, so reproducing it
	// discloses nothing and losing it would blind the record.
	b.WriteString(value[offset : offset+needleLen])
	b.WriteString(redactWindow(value[offset+needleLen : end]))
	if end < len(value) {
		b.WriteString("…")
	}
	return b.String()
}

// redactWindow replaces content with shape.
//
// Letters collapse to 'a'/'A' (case kept — a run of capitals is structure,
// a CONSTANT_NAME reads differently from prose) and digits to '0'. ASCII
// punctuation, spaces, tabs and newlines pass through: they are the seam.
// Anything else — every non-ASCII rune, and every ASCII control character
// that is not tab/newline/carriage-return — collapses to 'a' as well, so a
// Cyrillic draft or an emoji cannot ride through on the grounds that the
// table only knew about ASCII.
func redactWindow(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteByte('0')
		case unicode.IsDigit(r):
			b.WriteByte('0')
		case unicode.IsUpper(r):
			b.WriteByte('A')
		case unicode.IsLetter(r):
			b.WriteByte('a')
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteRune(r)
		case r >= ' ' && r < utf8.RuneSelf:
			// Printable ASCII that is neither letter nor digit: the
			// punctuation and quoting that make the seam legible.
			b.WriteRune(r)
		default:
			b.WriteByte('a')
		}
	}
	return b.String()
}

// walkStrings visits every string leaf of a decoded JSON argument tree.
//
// Only map[string]any and []any are descended: those are the two shapes
// encoding/json produces for an `any`, which is what an MCP tool call's
// arguments decode into. Anything else is a scalar and either is a string
// or is not.
func walkStrings(v any, path string, visit func(path, value string)) {
	switch t := v.(type) {
	case string:
		visit(path, t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkStrings(t[k], join(path, k), visit)
		}
	case []any:
		for i, item := range t {
			walkStrings(item, path+"["+strconv.Itoa(i)+"]", visit)
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

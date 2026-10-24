package tools

// truncate.go — the ONE text-truncation rule of the MCP layer (sk-282
// T004, spec DRAFT ADR R-8).
//
// # Why one rule
//
// Three independent surfaces have to shorten text at the edge of an MCP
// response: the comment preview inside a thread index, a project's
// description inside a list row, and the old/new values inside an inbox
// row. Before this file each of them would have grown its own cutter, and
// the first thing three private cutters lose is the truncation MARKER —
// each one decides for itself whether to set it, and they stop agreeing.
// Making the marker a property of the rule instead of a per-site
// convention is the whole point.
//
// # The rule
//
//	unit of measure  runes, never bytes
//	marker           set if and ONLY if text was actually dropped
//
// Runes, because a byte cut lands in the middle of a multi-byte character
// and produces mojibake — Cyrillic is two bytes per letter here, emoji are
// four.
//
// The marker's discipline is INV-1, second row of the marker table:
// "truncated" means "data was delivered partially, the remainder exists",
// and it is set ONLY when truncation really happened — never on a short
// string and never on an empty one. A marker that is always set carries no
// information and costs bytes on every response, which is the very thing
// this work removes.
//
// # What this file deliberately does NOT do
//
//   - It does not reuse internal/notifications.truncateRunesFlag (plan,
//     D-8). That cutter is unexported and bound to the monitor-poll path,
//     so it is not reachable from here at all; exporting it would make the
//     shared MCP rule the property of one domain package, and every future
//     consumer of the rule would inherit a dependency on notifications it
//     has no other reason to hold. The two implementations are
//     intentionally identical in behaviour: rune-safe, single pass, marker
//     only on a real cut, and a test binds their shared constant.
//   - It does not truncate anything in storage. The cut is on the wire
//     only — INV-3: no change may make text stop being stored or stop
//     being reachable through a reading tool.
//   - It does not wire itself into any tool. The consumers are T011
//     (thread-index preview), T016 (project description in a list row) and
//     T019 (inbox values), each in its own group. The plan promises "not one
//     change on the wire beyond the truncation rule being available", and
//     that promise is kept here by shipping the mechanism unattached.
//
// # Logging
//
// Neither the truncated prefix nor the full text may be logged — a log
// line is the one place where the whole point of shortening the payload is
// undone, and comment bodies are user content. Consumers log the LENGTH
// and the MARKER, never the value. This file logs nothing at all, which is
// why it takes no logger.

// The three assigned lengths. They are constants rather than parameters
// because R-8 assigns them: a rule whose lengths are decided per call site
// is not a rule. Should some future surface genuinely need a different
// length, it takes a parameter — that is cheaper than reconciling three
// implementations that drifted apart.
const (
	// ThreadIndexPreviewMaxRunes is the leading fragment of a comment kept
	// in a thread-index entry. 80 runes puts an entry at 222 characters as
	// built — id, author, RFC3339 date, the preview, the truncation flag —
	// so an index over a 48-comment thread lands near 10 700. That is the
	// measured figure and it replaces an estimate of ~90 per entry (and
	// ≈4 300 for the same thread) that this comment used to carry: the
	// conclusion is unchanged — «read the index, then fetch what you need»
	// still beats the 64 672 characters the same thread cost whole — but
	// the margin is a fifth of what the old number implied, and the index
	// at its 200-entry cap is 44 400, not ≈18 000. Consumer: T011.
	ThreadIndexPreviewMaxRunes = 80

	// ProjectDescriptionPreviewMaxRunes is how much of a project's
	// description survives into a project-list row. Consumer: T016.
	ProjectDescriptionPreviewMaxRunes = 200

	// InboxValuePreviewMaxRunes is the ceiling for an old/new value in an
	// inbox row. It is NOT a new number: 1 024 runes is the preview limit
	// the system already applies on the monitor-poll path
	// (notifications.BodyPreviewMaxRunes); R-8 extends the same limit to a
	// second path rather than inventing a competing one. It is spelled out
	// here rather than aliased to the notifications constant so the rule
	// stays owned by the MCP layer (D-8) — a future consumer that has
	// nothing to do with notifications must not inherit that package just
	// to learn a length. TestInboxValuePreviewMatchesMonitorPreview binds
	// the two numbers together so they cannot drift apart silently.
	InboxValuePreviewMaxRunes = 1024
)

// TruncateRunes returns the first maxRunes runes of s, and whether
// anything was dropped.
//
// The returned bool is the INV-1 "truncated" marker: true only when the
// input was longer than maxRunes. Both the empty string and a string that
// fits return false — "nothing was dropped" is the honest answer in both
// cases, and a caller that sets the marker anyway makes it lie.
//
// Single pass: the loop walks s by rune and slices at the byte offset of
// the (maxRunes+1)-th rune, so nothing is counted twice and no allocation
// happens. Invalid UTF-8 follows range-over-string semantics — each bad
// byte advances one position as U+FFFD — so a corrupt input is shortened
// safely instead of panicking.
//
// A negative maxRunes is clamped to zero: it is a programming error, and
// silently keeping the whole string would hide it behind an
// oversized response. maxRunes == 0 on a non-empty input returns
// ("", true) — nothing was delivered, and the remainder exists.
func TruncateRunes(s string, maxRunes int) (prefix string, truncated bool) {
	if maxRunes < 0 {
		maxRunes = 0
	}
	if maxRunes == 0 {
		return "", s != ""
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i], true
		}
		n++
	}
	return s, false
}

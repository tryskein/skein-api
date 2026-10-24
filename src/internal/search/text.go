package search

import (
	"strings"
	"unicode/utf8"
)

// MaxEmbedRunes caps the rune length of any single text we send to the
// embedder. qwen3-embedding:0.6b has a 32k token context window; one
// English token ≈ 4 chars, one Cyrillic char ≈ 1.5 tokens, so 160 000
// runes is comfortably below the ceiling for a mixed-language document
// (~24k tokens worst case) while still letting unusually long issue
// descriptions through with their context intact.
//
// We measure runes (not bytes) so the truncation point doesn't land in the
// middle of a multi-byte UTF-8 sequence — that would produce invalid input
// for the embedder.
const MaxEmbedRunes = 160_000

// truncateRunes returns text trimmed to at most max runes, plus a flag
// telling the caller whether truncation actually happened (so it can
// log + bump a Prometheus counter exactly once per truncated row, not
// once per "is this too long" check).
func truncateRunes(text string, max int) (string, bool) {
	if utf8.RuneCountInString(text) <= max {
		return text, false
	}
	// Walking runes is O(n) but unavoidable — Go strings are byte-indexed
	// and UTF-8 runes are variable width. We only do this for the long-tail
	// of oversized rows; steady-state cost is zero.
	rs := []rune(text)
	return string(rs[:max]), true
}

// joinNonEmpty concatenates the supplied parts with a blank-line separator,
// dropping any empty entries. Useful for embed text where a missing field
// shouldn't produce a `\n\n\n\n` run that wastes embedder context.
func joinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// BuildIssueEmbedText assembles the embed input for one issue. We
// deliberately drop `design` and `notes` — they tend to contain ad-hoc
// scratch content (TODOs, broken links, copy-pasted shell snippets) that
// pollutes the semantic vector. Title + description + acceptance criteria
// is the "what does this issue mean" summary.
//
// Returns the (possibly truncated) text and a `truncated` flag so the
// worker can record a metric.
func BuildIssueEmbedText(title, description, acceptance string) (string, bool) {
	full := joinNonEmpty(title, description, acceptance)
	return truncateRunes(full, MaxEmbedRunes)
}

// BuildCommentEmbedText embeds the comment body alone — comments don't
// have titles or any metadata that would help semantic match.
func BuildCommentEmbedText(body string) (string, bool) {
	return truncateRunes(strings.TrimSpace(body), MaxEmbedRunes)
}

// BuildProjectEmbedText: name (vector anchor) + description.
func BuildProjectEmbedText(name, description string) (string, bool) {
	full := joinNonEmpty(name, description)
	return truncateRunes(full, MaxEmbedRunes)
}

// BuildComponentEmbedText: name + description, same shape as project.
func BuildComponentEmbedText(name, description string) (string, bool) {
	full := joinNonEmpty(name, description)
	return truncateRunes(full, MaxEmbedRunes)
}

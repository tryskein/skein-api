package notifications

import (
	"strings"
	"testing"
)

// TestExtractMentionContext covers the four documented cases from
// sk-38:
//  1. mention in middle of body
//  2. mention at start
//  3. mention at end
//  4. mention not found (defensive fallback)
//
// Plus a handful of edge cases that the implementation has to handle:
// case-insensitive match, multi-line body collapsed to one line, and
// empty body.
func TestExtractMentionContext(t *testing.T) {
	const username = "admin"

	check := func(t *testing.T, name, body, wantMatch string) {
		t.Helper()
		got := extractMentionContext(body, username)

		// Empty body short-circuit.
		if body == "" {
			if got != "" {
				t.Fatalf("[%s] empty body should return empty; got %q", name, got)
			}
			return
		}

		// Single-line invariant.
		if strings.ContainsAny(got, "\n\r\t") {
			t.Fatalf("[%s] snippet must be single-line; got %q", name, got)
		}

		// Defensive fallback case (no mention).
		if !strings.Contains(strings.ToLower(body), "@"+username) {
			if len(got) == 0 {
				t.Fatalf("[%s] expected non-empty fallback; got empty", name)
			}
			if len(got) > 130 {
				t.Fatalf("[%s] fallback too long (%d): %q", name, len(got), got)
			}
			return
		}

		// Mention-found case: snippet must include the marker (with
		// its original casing — case-folding is only used for search).
		if wantMatch != "" && !strings.Contains(got, wantMatch) {
			t.Errorf("[%s] expected snippet to contain %q; got %q", name, wantMatch, got)
		}

		// Bound on length: 60+60 window + needle + a few chars of
		// ellipsis padding. Allow slop for word-boundary trimming.
		if len(got) > 200 {
			t.Errorf("[%s] snippet unexpectedly long (%d chars): %q", name, len(got), got)
		}
	}

	check(t, "mention in middle",
		"We have been working on this for two weeks and we should loop in @admin on this before the merge happens because final review is required.",
		"@admin")
	check(t, "mention at start",
		"@admin please review the design doc before tomorrow morning's standup.",
		"@admin")
	check(t, "mention at end",
		"Anyway, before we wrap this up could you take one more look @admin",
		"@admin")
	check(t, "mention not found",
		"This body has no at-sign mention of the caller at all so the fallback kicks in and returns just the first 120 characters of content here.",
		"")
	check(t, "case insensitive",
		"Note from earlier: please confirm with @Admin about the rollout schedule on Tuesday morning before the demo.",
		"@Admin")
	check(t, "multi-line body collapsed",
		"Line one of the body.\nLine two mentions @admin in the middle of a paragraph.\nLine three is here.",
		"@admin")
	check(t, "empty body", "", "")
}

// TestExtractMentionContext_LeadingEllipsis verifies that a snippet
// taken from the middle of the body has a leading "..." prefix and a
// trailing "..." suffix indicating content was trimmed on both sides.
func TestExtractMentionContext_LeadingEllipsis(t *testing.T) {
	// Body long enough that the 60-char before/after window doesn't
	// reach either edge.
	body := strings.Repeat("filler word here ", 20) + "and now @admin is mentioned right here " + strings.Repeat("more filler text ", 20)
	got := extractMentionContext(body, "admin")
	if !strings.HasPrefix(got, "...") {
		t.Errorf("expected leading ellipsis, got %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected trailing ellipsis, got %q", got)
	}
	if !strings.Contains(got, "@admin") {
		t.Errorf("snippet missing mention marker: %q", got)
	}
}

// TestExtractMentionContext_AtStartNoLeadingEllipsis verifies that when
// the mention is near the start of the body, no leading ellipsis is
// emitted (we didn't truncate anything from the start).
func TestExtractMentionContext_AtStartNoLeadingEllipsis(t *testing.T) {
	body := "@admin please confirm by EOD on Friday so we can ship the release."
	got := extractMentionContext(body, "admin")
	if strings.HasPrefix(got, "...") {
		t.Errorf("expected no leading ellipsis when mention is at start; got %q", got)
	}
	if !strings.Contains(got, "@admin") {
		t.Errorf("snippet missing mention marker: %q", got)
	}
}

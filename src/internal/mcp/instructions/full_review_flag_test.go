package instructions

// full_review_flag_test.go — sk-303.14, task T022; reduced in sk-908.
//
// The hole found in review: the review workflow changes the behaviour of
// a whole class of issues, observable by agents, and the guide an agent
// reads at the start of EVERY session said not a word about it.
// `reviewRequired` was described only in one tool's schema — i.e. only
// someone who had already decided to pass it would see it.
//
// Since sk-908 the guide carries one rule about it: the flag exists, its
// built-in default, and how the request "do not make me review this one"
// is expressed. The privacy and author-only claims left the guide (they
// are the mechanic's own invariants, not something an agent acts on
// before a call), so this gate no longer checks them against the code.
//
// ⚠ Why not "check that the words are there". A check "the text contains
// the word" is equally green when the text tells the truth and when it
// lies. The default is therefore read from the code, not from the text.

import (
	"strings"
	"testing"

	"skein-api/internal/review"
)

// TestFullGuide_ReviewFlagDefaultMatchesTheCode — the flag's factory
// value in the guide against the constant that sets it.
//
// Both directions are real. If the guide said "not required by default"
// — the agent would stop passing `false` where the user asked not to be
// notified, because it would decide that is already the default. If the
// constant flipped — the guide would become a lie without changing a
// single character.
func TestFullGuide_ReviewFlagDefaultMatchesTheCode(t *testing.T) {
	// A markdown line wrap must not decide the check: whitespace runs are
	// collapsed.
	flat := strings.Join(strings.Fields(FullGuide), " ")
	if !strings.Contains(flat, "reviewRequired") {
		t.Fatal("full.md no longer mentions reviewRequired — the agent will not " +
			"learn that the review flag exists before it has to pass it")
	}

	if review.DefaultReviewRequired {
		if !strings.Contains(flat, "built-in default is `true`") {
			t.Error("review.DefaultReviewRequired is true, but the guide does not " +
				"name `true` as the built-in default — the agent will not learn that " +
				"\"do not notify\" must be passed explicitly")
		}
		if !strings.Contains(flat, "reviewRequired: false") {
			t.Error("the guide does not show HOW the request \"do not notify on " +
				"completion\" is expressed: without an explicit `reviewRequired: false` " +
				"the agent will leave it in the description, where it means nothing")
		}
		return
	}
	t.Error("review.DefaultReviewRequired is false — the built-in default flipped, " +
		"and the guide rule together with this gate must be rewritten, not left as-is")
}

package instructions

import (
	"strings"
	"testing"
)

// Section helpers shared by the guide tests of this package.

// sectionBody returns the text of the `## `-level section introduced by
// heading, up to the next `## ` heading or EOF. Empty string when the
// heading is absent.
//
// Facts are asserted against the section body rather than the whole
// guide: a token that also occurs elsewhere in full.md would keep a
// whole-file strings.Contains green with the section deleted.
func sectionBody(t *testing.T, guide, heading string) string {
	t.Helper()

	start := strings.Index(guide, heading)
	if start < 0 {
		return ""
	}
	rest := guide[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// requireSectionFacts asserts every want is present in the named
// section, failing fast if the section itself is gone (the per-fact
// errors would be pure noise at that point).
func requireSectionFacts(t *testing.T, heading string, wants []string) {
	t.Helper()

	body := sectionBody(t, FullGuide, heading)
	if body == "" {
		t.Fatalf("full.md is missing the %q section — MCP tool schemas point "+
			"callers at it and now carry no fallback copy of the contract", heading)
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("full.md %q section is missing operational fact %q", heading, w)
		}
	}
}

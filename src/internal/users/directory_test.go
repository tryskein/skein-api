package users

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClampDirectoryLimit — sk-285.5.5. Pure unit (no DB): the default /
// ceiling rule behind GET /api/v1/users/directory. The REST handler
// rejects out-of-range values outright; this clamp is the store-level
// backstop for non-REST callers, so "0 means default, 10 000 means the
// ceiling" has to hold on its own.
func TestClampDirectoryLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset falls back to the default", 0, DirectoryDefaultLimit},
		{"negative falls back to the default", -1, DirectoryDefaultLimit},
		{"large negative falls back to the default", -1_000_000, DirectoryDefaultLimit},
		{"one is honoured", 1, 1},
		{"below the ceiling is honoured", DirectoryMaxLimit - 1, DirectoryMaxLimit - 1},
		{"the ceiling itself is honoured", DirectoryMaxLimit, DirectoryMaxLimit},
		{"above the ceiling is capped", DirectoryMaxLimit + 1, DirectoryMaxLimit},
		{"absurd is capped", 1_000_000, DirectoryMaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampDirectoryLimit(tc.in); got != tc.want {
				t.Errorf("ClampDirectoryLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestDirectoryLimitBounds pins the two constants against the sibling
// admin listing's contract (ListUsers: default 50, ceiling 100). They are
// documented as deliberately identical; a silent divergence would mean the
// two user-listing endpoints disagree about page sizes on the same
// resource.
func TestDirectoryLimitBounds(t *testing.T) {
	if DirectoryDefaultLimit != 50 {
		t.Errorf("DirectoryDefaultLimit = %d, want 50", DirectoryDefaultLimit)
	}
	if DirectoryMaxLimit != 100 {
		t.Errorf("DirectoryMaxLimit = %d, want 100", DirectoryMaxLimit)
	}
	if DirectoryDefaultLimit > DirectoryMaxLimit {
		t.Fatalf("default (%d) above ceiling (%d)", DirectoryDefaultLimit, DirectoryMaxLimit)
	}
}

// TestEscapeLikePattern — pure unit. A search term is user input pasted
// straight into an ILIKE pattern; `%` and `_` must come out matching
// THEMSELVES, and the backslash pass must run first so the escapes this
// function inserts are not themselves doubled.
func TestEscapeLikePattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is untouched", "alice", "alice"},
		{"percent is escaped", "%", `\%`},
		{"underscore is escaped", "_", `\_`},
		{"backslash is escaped", `\`, `\\`},
		{"match-everything pattern is neutralised", "%%", `\%\%`},
		{"mixed", `a%b_c\d`, `a\%b\_c\\d`},
		{"a pre-escaped percent stays literal, not an escape", `\%`, `\\\%`},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeLikePattern(tc.in); got != tc.want {
				t.Errorf("escapeLikePattern(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDirectorySearchTooLong — sk-285.5.5 review F6. Pure unit. `q` is
// user-supplied and reaches a LIKE pattern; the ceiling is what stops one
// authenticated caller from making a single request arbitrarily expensive.
// Measured in RUNES, and whitespace is stripped before the verdict because
// it is stripped before the term reaches the query builder.
func TestDirectorySearchTooLong(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"ordinary term", "alice", false},
		{"exactly at the ceiling", strings.Repeat("a", DirectorySearchMaxRunes), false},
		{"one rune over", strings.Repeat("a", DirectorySearchMaxRunes+1), true},
		{"far over", strings.Repeat("a", 50_000), true},
		{
			"multibyte is counted in runes, not bytes",
			// 128 Cyrillic runes = 256 bytes: at the ceiling, not over it.
			strings.Repeat("щ", DirectorySearchMaxRunes),
			false,
		},
		{
			"multibyte one rune over",
			strings.Repeat("щ", DirectorySearchMaxRunes+1),
			true,
		},
		{
			"padding whitespace does not push a legal term over",
			"  " + strings.Repeat("a", DirectorySearchMaxRunes) + "  ",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirectorySearchTooLong(tc.in); got != tc.want {
				t.Errorf("DirectorySearchTooLong(len=%d) = %v, want %v",
					utf8.RuneCountInString(tc.in), got, tc.want)
			}
		})
	}
}

// TestDirectorySearchIsPrefix — sk-285.5.5 review F5. Pure unit. The
// threshold decides which SQL shape a term gets: below
// DirectoryTrigramMinLen pg_trgm has no complete trigram to look up, so
// those terms take the prefix branch instead of degrading to a seq scan.
// Empty is not a search at all.
func TestDirectorySearchIsPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty is not a search", "", false},
		{"blank is not a search", "   ", false},
		{"one character", "a", true},
		{"two characters", "ad", true},
		{"exactly the threshold is substring", "adm", false},
		{"longer is substring", "admin", false},
		{"whitespace is trimmed before counting", "  ad  ", true},
		{"trimming can push a term up to the threshold", " adm ", false},
		{"multibyte counted in runes", "щщ", true},
		{"multibyte at the threshold", "щщщ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirectorySearchIsPrefix(tc.in); got != tc.want {
				t.Errorf("DirectorySearchIsPrefix(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDirectorySearchThresholdIsTrigramMinimum pins the threshold to 3.
// It is not a tunable: pg_trgm extracts trigrams, so two characters is the
// longest term that yields none. Lowering it would silently reintroduce
// the seq scan; raising it would give up substring matching that the
// trigram indexes can actually serve.
func TestDirectorySearchThresholdIsTrigramMinimum(t *testing.T) {
	if DirectoryTrigramMinLen != 3 {
		t.Errorf("DirectoryTrigramMinLen = %d, want 3 (pg_trgm trigram width)",
			DirectoryTrigramMinLen)
	}
}

package mentions

import (
	"strings"
	"testing"
)

// TestParse covers the regex edge cases the R4.3 plan calls out
// explicitly: start-of-text, end-of-text, mid-text, after-newline,
// after-punctuation, before-punctuation, in-code-block (no escape —
// known issue), email-like (`foo@bar.com` should NOT match),
// dup-mention dedup, and cap-25 enforcement.
func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "no mention",
			in:   "ordinary prose with no @ marks at all",
			// The lone `@` is not followed by an alnum — no match.
			want: nil,
		},
		{
			name: "start of text",
			in:   "@alice please review",
			want: []string{"alice"},
		},
		{
			name: "end of text",
			in:   "ping @bob",
			want: []string{"bob"},
		},
		{
			name: "mid text",
			in:   "cc @charlie for context",
			want: []string{"charlie"},
		},
		{
			name: "after newline",
			in:   "first line\n@dave second line",
			want: []string{"dave"},
		},
		{
			name: "after punctuation",
			in:   "(see @eve)",
			want: []string{"eve"},
		},
		{
			name: "before punctuation",
			in:   "@frank, please confirm",
			want: []string{"frank"},
		},
		{
			name: "before period",
			in:   "@grace.",
			want: []string{"grace"},
		},
		{
			name: "before colon",
			in:   "@henry: ack",
			want: []string{"henry"},
		},
		{
			name: "in code block (known issue, P2)",
			// Triple-backticks aren't recognised — this is documented in
			// parser.go and plan §F-12 as a P2 deferral. The expected
			// behaviour today is "matches anyway".
			in:   "```\n@iris is mentioned in code\n```",
			want: []string{"iris"},
		},
		{
			name: "email-like (must NOT match)",
			// `o` before `@` is a word char → boundary fails.
			in:   "send to foo@bar.com please",
			want: nil,
		},
		{
			name: "email-like with valid mention",
			// Mixed: email doesn't match, the standalone @julia does.
			in:   "mail foo@bar.com and ping @julia",
			want: []string{"julia"},
		},
		{
			name: "dedup repeated mention",
			in:   "@kate again, @kate again, and once more @kate",
			want: []string{"kate"},
		},
		{
			name: "preserve first-seen order across multiple users",
			in:   "@liam then @mia then @noah",
			want: []string{"liam", "mia", "noah"},
		},
		{
			name: "username with underscore",
			in:   "@oscar_p please",
			want: []string{"oscar_p"},
		},
		{
			name: "username with dash",
			in:   "@pat-q please",
			want: []string{"pat-q"},
		},
		{
			name: "username with digits",
			in:   "@quinn42 please",
			want: []string{"quinn42"},
		},
		{
			name: "too short to be a username (2 chars)",
			// users.username CHECK requires 3..30 chars total — `@ab` is
			// 2 chars after the @, fails the {2,29} suffix count.
			in:   "@ab is too short",
			want: nil,
		},
		{
			name: "exactly 3 chars (min)",
			in:   "@abc is fine",
			want: []string{"abc"},
		},
		{
			name: "starts with underscore (invalid)",
			// First char must be alnum per CHECK.
			in:   "@_alice is rejected",
			want: nil,
		},
		{
			name: "starts with dash (invalid)",
			in:   "@-alice is rejected",
			want: nil,
		},
		{
			name: "contiguous text after @ ends mention at non-word non-dash",
			// `@alice!bob` — alice ends at `!`, then `bob` has no leading
			// `@` so it's not a candidate.
			in:   "@alice!bob",
			want: []string{"alice"},
		},
		{
			name: "comma separates multiple mentions",
			in:   "cc @ross,@sam,@tina",
			want: []string{"ross", "sam", "tina"},
		},
		{
			name: "trailing dash (invalid as mention boundary)",
			// `@uma-` — the trailing `-` is consumed into the username
			// (regex allows trailing `-` since the class includes `-`),
			// then followed by space which is non-word-non-dash. Resolves
			// to `uma-` which won't match a real user (rare edge case).
			// Documented behaviour — Resolve() will silently drop it.
			in:   "@uma- continues",
			want: []string{"uma-"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in)
			if !equalStringSlices(got, tc.want) {
				t.Errorf("Parse(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParse_Cap25 covers the §F-7 hard ceiling. We feed 30 unique
// usernames; only the first 25 are returned.
func TestParse_Cap25(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		// Generate `@name00`, `@name01`, ... `@name29` — two-digit
		// padding keeps the regex simple ({2,29} matches name00 just
		// fine; 6-char username, 6 ≤ 1+29).
		if i > 0 {
			b.WriteByte(' ')
		}
		// Two-digit zero-pad keeps the username 6 chars long, which is
		// safely inside [3,30]. We rely on Sprintf-free formatting.
		b.WriteString("@name")
		if i < 10 {
			b.WriteByte('0')
		}
		b.WriteString(itoa(i))
	}
	// The §F-7 ceiling is 25 unique mentions in first-seen order: the
	// expected result is written out from the plan, not read from the
	// constant under test.
	want := []string{
		"name00", "name01", "name02", "name03", "name04",
		"name05", "name06", "name07", "name08", "name09",
		"name10", "name11", "name12", "name13", "name14",
		"name15", "name16", "name17", "name18", "name19",
		"name20", "name21", "name22", "name23", "name24",
	}
	if got := Parse(b.String()); !equalStringSlices(got, want) {
		t.Errorf("Parse(30 unique mentions) = %v (%d), want the first 25 %v", got, len(got), want)
	}
}

// TestParse_AdjacentByteBoundary pins the rule for the one byte right before
// `@`: a word byte [0-9A-Za-z_] makes it part of a word (an address, an
// identifier) and not a mention; any other byte starts a mention. Each input
// has exactly that one byte before `@`, so the position right after the start
// of the text is checked too. The bytes are the edges of every word range and
// their ASCII neighbours just outside it.
func TestParse_AdjacentByteBoundary(t *testing.T) {
	cases := []struct {
		before  string
		mention bool
	}{
		{"0", false}, {"5", false}, {"9", false},
		{"A", false}, {"M", false}, {"Z", false},
		{"a", false}, {"m", false}, {"z", false},
		{"_", false},
		{"/", true}, // just below '0'
		{":", true}, // just above '9'
		{"@", true}, // just below 'A'
		{"[", true}, // just above 'Z'
		{"^", true}, // just below '_'
		{"`", true}, // just below 'a', just above '_'
		{"{", true}, // just above 'z'
		{"-", true}, // a username byte, but not a word byte
		{" ", true}, // whitespace
		{"é", true}, // a multibyte UTF-8 letter is not a word byte
	}
	for _, tc := range cases {
		t.Run("before="+tc.before, func(t *testing.T) {
			in := tc.before + "@alice"
			var want []string
			if tc.mention {
				want = []string{"alice"}
			}
			if got := Parse(in); !equalStringSlices(got, want) {
				t.Errorf("Parse(%q) = %v, want %v", in, got, want)
			}
		})
	}
}

// TestParse_InvalidCandidateDoesNotStopTheScan — an `@` that is not followed
// by a username body is skipped, and the scan goes on to the next mention.
func TestParse_InvalidCandidateDoesNotStopTheScan(t *testing.T) {
	for _, in := range []string{
		"@ab then @alice",
		"@_x then @alice",
		"@ then @alice",
		"foo@bar then @alice",
	} {
		if got := Parse(in); !equalStringSlices(got, []string{"alice"}) {
			t.Errorf("Parse(%q) = %v, want [alice]", in, got)
		}
	}
}

// TestParse_DedupBeyondCap covers a subtle interaction: dedup runs
// BEFORE the cap, so 100 repeats of "@alice" still produce a length-1
// result, not a length-25 result that's all-alice.
func TestParse_DedupBeyondCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("@alice")
	}
	got := Parse(b.String())
	if len(got) != 1 || got[0] != "alice" {
		t.Errorf("Parse(100×@alice) = %v, want [alice]", got)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoa is a tiny strconv-free int → decimal-string converter for the
// cap test. Avoids pulling fmt for a single Sprintf call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

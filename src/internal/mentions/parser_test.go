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
	got := Parse(b.String())
	if len(got) != MaxMentionsPerText {
		t.Fatalf("Parse capped slice length = %d, want %d", len(got), MaxMentionsPerText)
	}
	// First mention is "name00", 25th is "name24".
	if got[0] != "name00" {
		t.Errorf("Parse[0] = %q, want %q", got[0], "name00")
	}
	if got[24] != "name24" {
		t.Errorf("Parse[24] = %q, want %q", got[24], "name24")
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

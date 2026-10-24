// Package mentions extracts and resolves @username references from
// free-form text fields (comment bodies, issue description / notes /
// acceptance_criteria / design). Two stages:
//
//  1. Parse(text) — pure scan, returns deduplicated lower-case
//     usernames in first-seen order. Caps at 25 unique mentions per
//     text; anything beyond is dropped silently. The cap matches the
//     R4.3 plan §F-7 — a hard ceiling against pathological input
//     ("@a @b @c…" pasted from another tool) that would otherwise
//     fan-out 25× watcher rows + 25× notifications per write.
//
//  2. Resolve(ctx, tx, projectID, candidates) — joins the candidate
//     list against users/project_members so only project-visible (or
//     admin) usernames resolve. Lives in resolve.go to keep the parser
//     tested without a DB.
//
// The split lets callers parse early (e.g. validate cap before opening
// the SERIALIZABLE tx) while still resolving inside the same tx as the
// source mutation — the inline call-site in AddComment / CreateIssue /
// PatchIssue (plan §F-13) requires atomicity.
package mentions

import "regexp"

// MaxMentionsPerText is the hard cap on unique usernames extracted from
// one Parse() call. Anything beyond this index is dropped silently.
//
// 25 is the R4.3 plan §F-7 ceiling — large enough to accommodate
// "@everyone-substitute" patterns without tripping legitimate users,
// small enough that worst-case fan-out (25 × N issues × pool watchers)
// stays bounded.
const MaxMentionsPerText = 25

// usernameRegex matches a username body following the literal `@`.
// Mirrors the active users.username CHECK
// (`^[a-z0-9][a-z0-9_-]{2,29}$`): first char alphanumeric, 3..30 total
// length, only [a-z0-9_-] thereafter. The `^` anchor makes the regex a
// strict prefix matcher — Parse anchors the regex at each candidate
// position by slicing the input.
//
// Plan §A flagged the spec/code mismatch (spec said `[a-z0-9_-]{3,30}`
// without the leading-alphanumeric anchor); we honour the live CHECK
// so usernames that don't actually exist in `users` aren't even
// candidates.
//
// Plan §F-12 acknowledges code-block escapes are deferred (P2) —
// `@user` inside triple-backticks today still matches.
var usernameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,29}`)

// isWordByte reports whether the byte is a "word" character per
// Postgres' \w semantics (and Go's regexp \w): [0-9A-Za-z_]. Matches
// the boundary rule used by users.username CHECK so an email-like
// substring (`o` before `@` in `foo@bar.com`) does NOT count as a
// mention boundary.
func isWordByte(b byte) bool {
	switch {
	case b >= '0' && b <= '9':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b == '_':
		return true
	default:
		return false
	}
}

// Parse extracts unique lower-case usernames from text in first-seen
// order. Returns nil for an empty input. Caps the result at
// MaxMentionsPerText; mentions beyond that index are dropped silently
// (no error) so a 26-mention paste doesn't reject the whole write.
//
// Algorithm: scan byte-by-byte for `@`, check the preceding byte is
// either start-of-string or a non-word char (so `foo@bar.com` doesn't
// match — the `o` before `@` is a word char), then anchor a username
// regex at the position right after `@`. The trailing boundary is
// implicit: `usernameRegex` is greedy within `[a-z0-9_-]{2,29}` and
// stops at the first non-matching byte, which is a valid mention
// terminator (whitespace, punctuation, or end of string).
//
// Why not a single Go regex with `(?:^|[^\w])` lookahead-style boundary:
// FindAllStringSubmatch consumes the boundary char, so adjacent
// mentions like `@a,@b` would skip the second match (the `@` of the
// second mention gets eaten as the boundary of a non-existent third
// match). Manual scanning advances exactly past the matched username,
// preserving adjacency.
//
// The username body uses ASCII-only character classes; treating the
// input as bytes (not runes) is correct here because `@` and the
// allowed username chars are all single-byte ASCII. UTF-8 multibyte
// sequences before `@` are non-word for our purposes (they fail
// isWordByte), which is the correct boundary behaviour.
func Parse(text string) []string {
	if text == "" {
		return nil
	}

	var (
		seen = make(map[string]struct{})
		out  = make([]string, 0, 4)
	)

	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		// Boundary check: previous byte is either non-existent
		// (start-of-string) or a non-word char.
		if i > 0 && isWordByte(text[i-1]) {
			continue
		}
		// Anchor username regex at the position after `@`. The regex is
		// `^...` so it only matches when the username body starts here.
		// FindString returns "" on no-match — short-circuit.
		body := usernameRegex.FindString(text[i+1:])
		if body == "" {
			continue
		}

		// users.username is always lowercase (DB CHECK); the regex
		// character class is already lowercase-only. The de-dupe key is
		// the body verbatim — no further normalisation needed.
		if _, dup := seen[body]; !dup {
			seen[body] = struct{}{}
			out = append(out, body)
			if len(out) >= MaxMentionsPerText {
				// Skip the rest — but advance i past the consumed body so
				// the next iteration doesn't reprocess characters inside
				// it. The break does the same work — drop out entirely.
				break
			}
		}
		// Advance i past the consumed username body so adjacent mentions
		// (`@a,@b`) get scanned independently. The for-loop's i++ then
		// moves us one further, landing on the boundary char. Because we
		// only added len(body), and the boundary char is the byte right
		// after, the next iteration's `text[i] == '@'` check correctly
		// detects a fresh mention.
		i += len(body)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

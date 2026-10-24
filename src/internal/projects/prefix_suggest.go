package projects

// prefix_suggest.go — the suggestion of a free project prefix (sk-674.18
// T025, US-15, SC-016; SAAS-PLAN 4.4, after OpenProject's
// ProjectIdentifierSuggestionGenerator):
//
//  1. the name is transliterated to ASCII (go-unidecode) and lowercased;
//     its words are the runs of letters and digits;
//  2. a name of several words gives its initials (up to 5 words), then
//     lengthens them one character at a time, word by word in turn
//     ("Pls Shop" → ps → pls → plsh → …); a one-word name gives its first
//     3 characters, then one more each time;
//  3. a candidate is never shorter than SKEIN_PROJECT_PREFIX_MIN_LENGTH and
//     never longer than 10 characters;
//  4. when every candidate is taken — the first one with a number: ps2,
//     ps3, …;
//  5. the first free one goes into the field, the next 3–4 are offered;
//  6. a suggestion reserves nothing: the unique index decides at creation.
//
// A prefix never starts with a digit: leading digits of the name are
// dropped; a name with no letter at all suggests from "p". Taken means the
// prefix of any project of the installation (prefixes are global, D-4) or a
// reserved prefix of a deleted project (US-15).

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mozillazg/go-unidecode"
)

const (
	// prefixSuggestionMax — the ceiling of a suggested prefix (plan 4.4).
	prefixSuggestionMax = 10
	// prefixInitialsWords — initials are taken from at most 5 words.
	prefixInitialsWords = 5
	// prefixOneWordStart — a one-word name starts from its first 3
	// characters.
	prefixOneWordStart = 3
	// prefixNumberedMax bounds the numbered fallback (ps2 … ps99).
	prefixNumberedMax = 99
	// prefixFallbackBase is the base of a name with no letter.
	prefixFallbackBase = "p"
)

// prefixWords are the transliterated, lowercased words of name, the first
// one stripped of leading digits so no candidate starts with a digit.
func prefixWords(name string) []string {
	ascii := strings.ToLower(unidecode.Unidecode(name))
	words := strings.FieldsFunc(ascii, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	for len(words) > 0 {
		w := strings.TrimLeft(words[0], "0123456789")
		if w != "" {
			words[0] = w
			break
		}
		words = words[1:]
	}
	if len(words) == 0 {
		return []string{prefixFallbackBase}
	}
	return words
}

// PrefixCandidates lists the candidates of steps 1–3 for name, in order,
// without duplicates; minLen is SKEIN_PROJECT_PREFIX_MIN_LENGTH (0 = none).
// The numbered fallback (step 4) is not here — it depends on what is taken.
func PrefixCandidates(name string, minLen int) []string {
	words := prefixWords(name)
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		if len(c) > prefixSuggestionMax || len(c) < minLen || seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(words) == 1 {
		w := words[0]
		for n := min(prefixOneWordStart, len(w)); n <= min(len(w), prefixSuggestionMax); n++ {
			add(w[:n])
		}
		return out
	}
	if len(words) > prefixInitialsWords {
		words = words[:prefixInitialsWords]
	}
	// taken[i] is how many characters of word i the candidate uses.
	taken := make([]int, len(words))
	for i := range taken {
		taken[i] = 1
	}
	build := func() string {
		var b strings.Builder
		for i, w := range words {
			b.WriteString(w[:taken[i]])
		}
		return b.String()
	}
	add(build())
	for {
		grew := false
		for i := range words {
			if taken[i] >= len(words[i]) {
				continue
			}
			taken[i]++
			c := build()
			if len(c) > prefixSuggestionMax {
				return out
			}
			add(c)
			grew = true
		}
		if !grew {
			return out
		}
	}
}

// numbered is step 4: base with the number n, cut so the whole stays within
// the ceiling.
func numbered(base string, n int) string {
	suffix := strconv.Itoa(n)
	if len(base)+len(suffix) > prefixSuggestionMax {
		base = base[:prefixSuggestionMax-len(suffix)]
	}
	return base + suffix
}

// suggestFree runs steps 1–5: up to want free prefixes, the candidates first,
// then the numbered fallback of the first base. isTaken answers for a batch.
func suggestFree(name string, minLen, want int, isTaken func([]string) (map[string]bool, error)) ([]string, error) {
	cands := PrefixCandidates(name, minLen)
	base := prefixWords(name)[0]
	if len(cands) > 0 {
		base = cands[0]
	} else if len(base) > prefixSuggestionMax {
		base = base[:prefixSuggestionMax]
	}
	var out []string
	try := func(batch []string) error {
		taken, err := isTaken(batch)
		if err != nil {
			return err
		}
		for _, c := range batch {
			if !taken[c] && len(out) < want {
				out = append(out, c)
			}
		}
		return nil
	}
	if err := try(cands); err != nil {
		return nil, err
	}
	for n := 2; len(out) < want && n <= prefixNumberedMax; n += 10 {
		var batch []string
		for k := n; k < n+10 && k <= prefixNumberedMax; k++ {
			if c := numbered(base, k); len(c) >= minLen {
				batch = append(batch, c)
			}
		}
		if err := try(batch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// prefixSuggestionCount — the field's value plus 3–4 more (plan 4.4 p.5).
const prefixSuggestionCount = 5

// SuggestPrefixes answers GET /projects/prefix-suggestions: up to 5 free
// prefixes for a project named name, best first.
func (s *Store) SuggestPrefixes(ctx context.Context, name string) ([]string, error) {
	return suggestFree(name, s.prefixMinLength, prefixSuggestionCount, func(c []string) (map[string]bool, error) {
		return s.takenPrefixes(ctx, c)
	})
}

// prefixAlternatives is how many free variants a «taken» answer carries.
const prefixAlternatives = 3

// PrefixAlternatives are the 2–3 free prefixes a «taken» answer offers for a
// project named name (the prefix itself when there is no name).
func (s *Store) PrefixAlternatives(ctx context.Context, name string) ([]string, error) {
	return suggestFree(name, s.prefixMinLength, prefixAlternatives, func(c []string) (map[string]bool, error) {
		return s.takenPrefixes(ctx, c)
	})
}

// takenPrefixes reports which of candidates are taken: the prefix of any
// project of the installation or a reserved prefix of a deleted one.
func (s *Store) takenPrefixes(ctx context.Context, candidates []string) (map[string]bool, error) {
	out := make(map[string]bool, len(candidates))
	if len(candidates) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT lower(id_prefix) FROM projects WHERE lower(id_prefix) = ANY($1::text[])
UNION
SELECT prefix FROM reserved_prefixes WHERE prefix = ANY($1::text[])`, candidates)
	if err != nil {
		return nil, fmt.Errorf("reading taken prefixes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("reading taken prefixes: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}

// PrefixTaken reports whether prefix is taken now (see takenPrefixes).
func (s *Store) PrefixTaken(ctx context.Context, prefix string) (bool, error) {
	taken, err := s.takenPrefixes(ctx, []string{strings.ToLower(prefix)})
	if err != nil {
		return false, err
	}
	return taken[strings.ToLower(prefix)], nil
}

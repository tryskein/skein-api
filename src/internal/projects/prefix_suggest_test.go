package projects

// prefix_suggest_test.go — sk-674.18 T025, matrix row 104 (SC-016).
// Reference — SAAS-PLAN 4.4, written before the code (the algorithm of
// OpenProject's ProjectIdentifierSuggestionGenerator): the name is
// transliterated by unidecode and lowercased; a name of several words gives
// its initials (up to 5), then lengthens them (`ps` → `pls` → `plsh`); a
// one-word name gives its first 3 characters, then one more; never shorter
// than SKEIN_PROJECT_PREFIX_MIN_LENGTH, never longer than 10; when every
// candidate is taken — a number (`app` taken → `app2`); a prefix never
// starts with a digit. Taken is a substituted set — the database is the
// generator's boundary.

import (
	"regexp"
	"slices"
	"testing"
)

func takenSet(taken ...string) func([]string) (map[string]bool, error) {
	return func(batch []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, c := range batch {
			out[c] = slices.Contains(taken, c)
		}
		return out, nil
	}
}

func TestSuggestPrefixes_ReferenceNames(t *testing.T) {
	for _, c := range []struct {
		name   string
		in     string
		minLen int
		taken  []string
		want   []string
	}{
		{"several words: initials, then lengthened", "Plush Shop", 0, nil, []string{"ps", "pls", "plsh"}},
		{"several words: the taken initials are skipped", "Plush Shop", 0, []string{"ps", "pls"}, []string{"plsh"}},
		{"initials of at most 5 words", "Alpha Beta Gamma Delta Epsilon Zeta", 0, nil, []string{"abgde"}},
		{"one word: the first 3 characters, then one more", "Skein", 0, nil, []string{"ske", "skei", "skein"}},
		{"Cyrillic is transliterated", "Проект", 0, nil, []string{"pro", "proe", "proek"}},
		{"Cyrillic, several words", "Мой Проект", 0, nil, []string{"mp"}},
		{"every candidate taken: a number", "App", 0, []string{"app"}, []string{"app2", "app3", "app4"}},
		{"the minimum length lifts the initials", "Plush Shop", 4, nil, []string{"plsh"}},
		{"the minimum length lifts a one-word name", "Skein", 5, nil, []string{"skein"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := suggestFree(c.in, c.minLen, len(c.want), takenSet(c.taken...))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("%q (min %d, taken %v): %v, want %v", c.in, c.minLen, c.taken, got, c.want)
			}
		})
	}
}

func TestSuggestPrefixes_Bounds(t *testing.T) {
	form := regexp.MustCompile(`^[a-z]`)
	for _, c := range []struct {
		name   string
		in     string
		minLen int
		taken  []string
	}{
		{"a long word, nothing taken", "Supercalifragilisticexpialidocious", 0, nil},
		{"a long word, its candidates taken", "Supercalifragilisticexpialidocious", 0,
			[]string{"sup", "supe", "supercalif", "super", "superc", "superca", "supercal", "supercali"}},
		{"a name starting with digits", "2048 Game", 0, nil},
		{"a name of digits only", "2048", 0, nil},
		{"a long name under a minimum length", "International Business Machines", 3, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := suggestFree(c.in, c.minLen, 5, takenSet(c.taken...))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				t.Fatalf("%q: no suggestion", c.in)
			}
			for _, p := range got {
				if len(p) > 10 || len(p) < c.minLen {
					t.Errorf("%q: %q is outside [%d, 10]", c.in, p, c.minLen)
				}
				if !form.MatchString(p) {
					t.Errorf("%q: %q does not start with a letter", c.in, p)
				}
				if slices.Contains(c.taken, p) {
					t.Errorf("%q: %q is taken", c.in, p)
				}
			}
		})
	}
}

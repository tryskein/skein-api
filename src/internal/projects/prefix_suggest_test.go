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
	"errors"
	"regexp"
	"slices"
	"strconv"
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

// TestSuggestPrefixes_OrderAndExhaustion — the exact order of the
// suggestions and the ends of both ranges, from the plan: a one-word name
// lengthens one character at a time up to a candidate of exactly 10; the
// numbered fallback runs base2 … base99 and stops there; a numbered
// candidate stays within 10 characters by cutting the base; a failure to read
// any group of candidates is returned, not swallowed.
func TestSuggestPrefixes_OrderAndExhaustion(t *testing.T) {
	numberedTaken := func(base string, upTo int) []string {
		taken := []string{base}
		for n := 2; n <= upTo; n++ {
			taken = append(taken, base+strconv.Itoa(n))
		}
		return taken
	}

	t.Run("a one-word name up to the 10-character candidate, then numbers", func(t *testing.T) {
		got, err := suggestFree("Supercalifragilistic", 0, 10, takenSet())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"sup", "supe", "super", "superc", "superca", "supercal", "supercali", "supercalif", "sup2", "sup3"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a numbered candidate cuts its base to stay within 10", func(t *testing.T) {
		got, err := suggestFree("Supercalifragilistic", 10, 2, takenSet("supercalif"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"supercali2", "supercali3"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("99 is the last number offered", func(t *testing.T) {
		got, err := suggestFree("App", 0, 3, takenSet(numberedTaken("app", 98)...))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"app99"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("past 99 nothing more is asked or offered", func(t *testing.T) {
		taken := numberedTaken("app", 99)
		var asked []string
		got, err := suggestFree("App", 0, 5, func(batch []string) (map[string]bool, error) {
			asked = append(asked, batch...)
			return takenSet(taken...)(batch)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want nothing: every candidate up to app99 is taken", got)
		}
		if slices.Contains(asked, "app100") || slices.Contains(asked, "app1") {
			t.Errorf("asked %v, want only app and app2 … app99", asked)
		}
		if !slices.Contains(asked, "app99") {
			t.Errorf("asked %v, want app99 asked", asked)
		}
	})

	errRead := errors.New("reading taken prefixes: connection lost")
	for _, failAt := range []string{"app", "app2", "app12", "app92"} {
		t.Run("a failed read of the group with "+failAt+" is returned", func(t *testing.T) {
			got, err := suggestFree("App", 0, 5, func(batch []string) (map[string]bool, error) {
				if slices.Contains(batch, failAt) {
					return nil, errRead
				}
				return takenSet(numberedTaken("app", 99)...)(batch)
			})
			if !errors.Is(err, errRead) || got != nil {
				t.Errorf("got %v, %v; want nil and the read error", got, err)
			}
		})
	}
}

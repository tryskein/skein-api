package accounts_test

// username_suggest_test.go — (US-8, contract
// UsernameSuggestionsResponse / UsernameTakenError: «2–3 usernames free at
// the moment of the answer»). On a populated instance every conventional
// candidate of a common stem (the stem, its domain variants, stem2…stem99)
// may be taken; the answer still carries 2–3 distinct free valid usernames.
// The directory is a fake that decides what is taken.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"skein-api/internal/accounts"
)

// takenExcept marks taken every candidate built on stem with no suffix or a
// numeric suffix below 100 or a non-numeric suffix — the whole
// conventional set — except the names in free.
type takenExcept struct {
	stem string
	free map[string]bool
}

func (takenExcept) EmailTaken(context.Context, string) (bool, error) { return false, nil }

func (d takenExcept) TakenUsernames(_ context.Context, cands []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range cands {
		if d.free[c] {
			continue
		}
		rest, ok := strings.CutPrefix(c, d.stem)
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(rest); err == nil && n >= 100 {
			continue // beyond the conventional set
		}
		out[c] = true
	}
	return out, nil
}

var usernameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,29}$`)

func suggestionsWith(t *testing.T, ids accounts.Identities, email string) []string {
	t.Helper()
	svc := accounts.NewService(accounts.ServiceConfig{SignupEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local"},
		fakeDB{}, fakeUsers{}, ids, noCaptcha{}, accounts.NewRateLimiter())
	resp, err := svc.UsernameSuggestions(context.Background(), email, "")
	if err != nil {
		t.Fatalf("UsernameSuggestions: %v", err)
	}
	return resp.Suggestions
}

func assertTwoToThreeFree(t *testing.T, ids takenExcept, got []string) {
	t.Helper()
	if len(got) < 2 || len(got) > 3 {
		t.Fatalf("suggestions = %v, want 2–3", got)
	}
	taken, _ := ids.TakenUsernames(context.Background(), got)
	seen := map[string]bool{}
	for _, s := range got {
		if !usernameRule.MatchString(s) || strings.HasPrefix(s, "deleted-") {
			t.Errorf("suggestion %q breaks the username rule", s)
		}
		if taken[s] {
			t.Errorf("suggestion %q is taken", s)
		}
		if seen[s] {
			t.Errorf("suggestion %q repeated", s)
		}
		seen[s] = true
	}
}

func TestUsernameSuggestions_ConventionalSetExhausted(t *testing.T) {
	ids := takenExcept{stem: "dev", free: map[string]bool{}}
	assertTwoToThreeFree(t, ids, suggestionsWith(t, ids, "dev@corp.test"))
}

func TestUsernameSuggestions_OnlyOneConventionalFree(t *testing.T) {
	ids := takenExcept{stem: "dev", free: map[string]bool{"dev57": true}}
	got := suggestionsWith(t, ids, "dev@corp.test")
	assertTwoToThreeFree(t, ids, got)
	found := false
	for _, s := range got {
		found = found || s == "dev57"
	}
	if !found {
		t.Errorf("suggestions = %v, want the one free conventional name dev57 among them", got)
	}
}

// takenSet marks taken exactly the names it holds.
type takenSet map[string]bool

func (takenSet) EmailTaken(context.Context, string) (bool, error) { return false, nil }

func (d takenSet) TakenUsernames(_ context.Context, cands []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range cands {
		out[c] = d[c]
	}
	return out, nil
}

// TestUsernameSuggestions_ConventionalOrder — the conventional candidates in
// the order the contract gives them (the stem, stem2, stem-domain, stem +
// the domain's initial, stem3 … stem9, then stem10 … stem99), the first free
// ones first: a free conventional name is never skipped for a later one or
// for the random fallback.
func TestUsernameSuggestions_ConventionalOrder(t *testing.T) {
	taken := func(names ...string) takenSet {
		s := takenSet{}
		for _, n := range names {
			s[n] = true
		}
		return s
	}
	upTo := func(from, to int) []string {
		var out []string
		for n := from; n <= to; n++ {
			out = append(out, "dev"+strconv.Itoa(n))
		}
		return out
	}
	conventional := append([]string{"dev", "dev-corp", "devc"}, upTo(2, 9)...)

	cases := []struct {
		name  string
		taken takenSet
		want  []string
	}{
		{"nothing taken", taken(), []string{"dev", "dev2", "dev-corp"}},
		{"the stem and stem2 taken: the domain variants next", taken("dev", "dev2"), []string{"dev-corp", "devc", "dev3"}},
		{"only the stem free", taken(append(upTo(2, 9), "dev-corp", "devc")...), []string{"dev", "dev10", "dev11"}},
		{"the first ten taken: the numbered fallback from 10", taken(conventional...), []string{"dev10", "dev11", "dev12"}},
		{"everything up to 98 taken: 99 is still offered", taken(append(conventional, upTo(10, 98)...)...), []string{"dev99"}},
		{"stem9 is the last of the first group", taken(append([]string{"dev", "dev-corp", "devc"}, upTo(2, 8)...)...), []string{"dev9", "dev10", "dev11"}},
		{"numbers continue across the groups without a gap", taken(append(conventional, upTo(10, 18)...)...), []string{"dev19", "dev20", "dev21"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := suggestionsWith(t, c.taken, "dev@corp.test")
			if len(got) < len(c.want) {
				t.Fatalf("suggestions = %v, want them to start with %v", got, c.want)
			}
			if prefix := got[:len(c.want)]; strings.Join(prefix, ",") != strings.Join(c.want, ",") {
				t.Errorf("suggestions = %v, want them to start with %v", got, c.want)
			}
			for _, s := range got {
				if c.taken[s] {
					t.Errorf("suggestion %q is taken", s)
				}
			}
		})
	}
}

// TestUsernameSuggestions_DomainVariants — the domain variants follow the
// username rule and the stem: a domain label too long to fit beside the stem
// gives no candidate (never an empty or invalid one), and a domain equal to
// the stem adds no variant of itself.
func TestUsernameSuggestions_DomainVariants(t *testing.T) {
	longLabel := strings.Repeat("q", 40)
	cases := []struct {
		name  string
		email string
		taken takenSet
		want  []string
	}{
		{"a long domain label", "dev@" + longLabel + ".test", takenSet{"dev": true, "dev2": true}, []string{"devq", "dev3", "dev4"}},
		{"the domain is the stem", "corp@corp.test", takenSet{"corp": true, "corp2": true}, []string{"corp3", "corp4", "corp5"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := suggestionsWith(t, c.taken, c.email)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("suggestions = %q, want %q", got, c.want)
			}
		})
	}
}

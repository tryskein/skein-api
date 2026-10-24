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

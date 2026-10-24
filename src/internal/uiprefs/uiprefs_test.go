package uiprefs

// Reference — the sk-551.5 brief: the set of kinds ⊂ {cards, kanban, tree}; repeats
// collapse; the order is stable cards, kanban, tree; an unknown kind is
// a refusal naming it; an empty set is an array, not null.

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeIssueModalViews(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		badView string
	}{
		{name: "nil is the empty set", in: nil, want: []string{}},
		{name: "reordered and deduplicated", in: []string{"tree", "kanban", "tree", "cards"}, want: []string{"cards", "kanban", "tree"}},
		{name: "single", in: []string{"kanban"}, want: []string{"kanban"}},
		{name: "unknown name", in: []string{"kanban", "board"}, badView: "board"},
		{name: "case matters", in: []string{"Tree"}, badView: "Tree"},
		{name: "empty string", in: []string{""}, badView: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeIssueModalViews(tc.in)
			if tc.want == nil {
				var unknown *UnknownViewError
				if !errors.As(err, &unknown) || unknown.View != tc.badView {
					t.Fatalf("NormalizeIssueModalViews(%q) error = %v, want UnknownViewError{%q}", tc.in, err, tc.badView)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeIssueModalViews(%q): %v", tc.in, err)
			}
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NormalizeIssueModalViews(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// Reference — the maintainers' decision sk-551.7: density "Normal / Compact",
// on the wire normal / compact; any other word is a refusal naming it.
func TestValidateDensity(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"normal", true},
		{"compact", true},
		{"tiny", false},
		{"Compact", false},
		{" compact", false},
		{"", false},
	}
	for _, tc := range cases {
		err := ValidateDensity(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("ValidateDensity(%q) = %v, want nil", tc.in, err)
			}
			continue
		}
		var unknown *UnknownDensityError
		if !errors.As(err, &unknown) || unknown.Density != tc.in {
			t.Errorf("ValidateDensity(%q) = %v, want UnknownDensityError{%q}", tc.in, err, tc.in)
		}
	}
}

// Reference — the sk-551.8 spec, the PUT /api/v1/me/ui-prefs contract: the left
// panel mode columns / sections; an unknown value, including an empty string and
// another case, is a refusal; the case is strict, no trim: a space before or after
// the value is also a refusal naming the value.
func TestValidateNavLayout(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"columns", true},
		{"sections", true},
		{"", false},
		{"Columns", false},
		{"Sections", false},
		{" sections", false},
		{"sections ", false},
		{"tiles", false},
	}
	for _, tc := range cases {
		err := ValidateNavLayout(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("ValidateNavLayout(%q) = %v, want nil", tc.in, err)
			}
			continue
		}
		var unknown *UnknownNavLayoutError
		if !errors.As(err, &unknown) || unknown.NavLayout != tc.in {
			t.Errorf("ValidateNavLayout(%q) = %v, want UnknownNavLayoutError{%q}", tc.in, err, tc.in)
		}
	}
}

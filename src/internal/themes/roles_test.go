package themes

import (
	"reflect"
	"testing"
)

// TestClassifyRoles covers the three answers the classifier can give and
// the one it must never give: a retired name (with and without a
// successor), a name from no dictionary, and a live role.
//
// The live-role case is the load-bearing one and it is asserted as an
// EXACT empty result rather than as "no error": both callers branch on
// `len(...) > 0`, so a classifier that flagged a working role would
// refuse a legitimate write (set_variables) and cry wolf on every
// preview.
func TestClassifyRoles(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want []RoleIssue
	}{
		{
			name: "every key is a live role — nothing to report",
			in:   map[string]string{"--primary": "#123456", "--ink": "#000000", "--space-4": "16px"},
			want: nil,
		},
		{
			name: "a retired name reports its successor",
			in:   map[string]string{"--bg-base": "#ffffff"},
			want: []RoleIssue{{Key: "--bg-base", Status: RoleRetired, Successor: "--bg"}},
		},
		{
			name: "a retired name with no counterpart reports none",
			in:   map[string]string{"--font-size-body": "14px"},
			want: []RoleIssue{{Key: "--font-size-body", Status: RoleRetired}},
		},
		{
			name: "a name from no dictionary is unknown, not retired",
			in:   map[string]string{"--primaryy": "#123456"},
			want: []RoleIssue{{Key: "--primaryy", Status: RoleUnknown}},
		},
		{
			name: "a mixed map reports only the bad keys, sorted",
			in: map[string]string{
				"--primary":  "#123456", // live
				"--bg-hover": "rgba(0,0,0,0.05)",
				"--zzz-nope": "1px",
				"--brand":    "#cb4b16",
			},
			want: []RoleIssue{
				{Key: "--bg-hover", Status: RoleRetired, Successor: "--elev"},
				{Key: "--brand", Status: RoleRetired, Successor: "--primary"},
				{Key: "--zzz-nope", Status: RoleUnknown},
			},
		},
		{
			name: "an empty map has nothing wrong with it",
			in:   map[string]string{},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRoles(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ClassifyRoles() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestClassifyRolesIsOrdered pins the sort, which is not cosmetic: the
// result is rendered into a refusal message an agent may diff between
// runs, and Go's map iteration order is deliberately randomised. Without
// the sort the same patch produces a different message every call.
//
// The input is authored so that map order CANNOT accidentally be the
// answer — the keys are inserted in an order opposite to the expected
// one, and the assertion runs the classifier repeatedly so a lucky
// iteration cannot pass for a sort.
func TestClassifyRolesIsOrdered(t *testing.T) {
	in := map[string]string{
		"--zzz-last":   "1px",
		"--match-fg":   "#000000",
		"--brand":      "#cb4b16",
		"--accent-red": "#ff0000",
	}
	want := []string{"--accent-red", "--brand", "--match-fg", "--zzz-last"}

	for i := 0; i < 25; i++ {
		got := ClassifyRoles(in)
		keys := make([]string, 0, len(got))
		for _, it := range got {
			keys = append(keys, it.Key)
		}
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("run %d: keys = %v, want %v", i, keys, want)
		}
	}
}

// TestFormatRoleIssues pins the three renderings, because the message is
// the half of the answer a human reads in a transcript. A retired name
// whose successor went missing from the string would leave the reader
// with "that role is gone" and no next step.
func TestFormatRoleIssues(t *testing.T) {
	got := FormatRoleIssues([]RoleIssue{
		{Key: "--bg-base", Status: RoleRetired, Successor: "--bg"},
		{Key: "--space-xs", Status: RoleRetired},
		{Key: "--zzz-nope", Status: RoleUnknown},
	})
	want := "--bg-base (retired, use --bg), --space-xs (retired, no replacement), --zzz-nope (unknown)"
	if got != want {
		t.Fatalf("FormatRoleIssues() =\n  %q\nwant\n  %q", got, want)
	}
	if FormatRoleIssues(nil) != "" {
		t.Fatalf("FormatRoleIssues(nil) = %q, want empty", FormatRoleIssues(nil))
	}
}

// TestIsCurrentRole guards the membership test the classifier is built
// on, including the fourteen names that belong to BOTH dictionaries —
// they are live roles and must never be reported as retired.
func TestIsCurrentRole(t *testing.T) {
	for _, key := range []string{"--primary", "--n-500", "--font-ui", "--width-sliver", "--font-weight-bold"} {
		if !IsCurrentRole(key) {
			t.Errorf("IsCurrentRole(%q) = false, want true", key)
		}
		if _, retired := RetiredRoles[key]; retired {
			t.Errorf("%q is a live role and must not appear in RetiredRoles", key)
		}
	}
	for _, key := range []string{"--bg-base", "--brand", "--zzz-nope", "", "primary"} {
		if IsCurrentRole(key) {
			t.Errorf("IsCurrentRole(%q) = true, want false", key)
		}
	}
}

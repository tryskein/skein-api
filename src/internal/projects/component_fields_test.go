package projects

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// componentPathsEqual replaced a reflect.DeepEqual call (audit sk-194,
// finding opt-A11-024). A hand-written comparator only stays correct while
// it compares every field of ComponentPath, and only preserves the Diff
// contract while it keeps DeepEqual's nil≠empty distinction — the two
// properties these tests lock down.

// TestComponentPathFieldCount fails when ComponentPath gains or loses a
// field, because componentPathsEqual compares its fields by hand and would
// otherwise start ignoring the new one silently — a diff that reports "no
// change" for a real change, i.e. a lost event.
func TestComponentPathFieldCount(t *testing.T) {
	const want = 2 // RepoID, Glob — both compared in componentPathsEqual
	if got := reflect.TypeFor[ComponentPath]().NumField(); got != want {
		t.Fatalf("ComponentPath has %d fields, componentPathsEqual compares %d — "+
			"update the comparator (and this count) in the same change", got, want)
	}
}

func TestComponentPathsEqual(t *testing.T) {
	repoA := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	repoB := uuid.MustParse("22222222-2222-4222-8222-222222222222")

	tests := []struct {
		name string
		a, b []ComponentPath
		want bool
	}{
		{name: "both nil", a: nil, b: nil, want: true},
		{
			// DeepEqual treats nil and empty as different; the Diff closure
			// relies on that to tell "paths cleared" from "paths untouched".
			name: "nil vs empty", a: nil, b: []ComponentPath{}, want: false,
		},
		{name: "empty vs nil", a: []ComponentPath{}, b: nil, want: false},
		{name: "both empty", a: []ComponentPath{}, b: []ComponentPath{}, want: true},
		{
			name: "same single element",
			a:    []ComponentPath{{RepoID: repoA, Glob: "src/**"}},
			b:    []ComponentPath{{RepoID: repoA, Glob: "src/**"}},
			want: true,
		},
		{
			name: "different RepoID",
			a:    []ComponentPath{{RepoID: repoA, Glob: "src/**"}},
			b:    []ComponentPath{{RepoID: repoB, Glob: "src/**"}},
			want: false,
		},
		{
			name: "different Glob",
			a:    []ComponentPath{{RepoID: repoA, Glob: "src/**"}},
			b:    []ComponentPath{{RepoID: repoA, Glob: "docs/**"}},
			want: false,
		},
		{
			// Order matters: the whole jsonb blob is replaced on UPDATE and
			// subscribers may render the diff order-aware.
			name: "same elements reordered",
			a: []ComponentPath{
				{RepoID: repoA, Glob: "src/**"},
				{RepoID: repoB, Glob: "docs/**"},
			},
			b: []ComponentPath{
				{RepoID: repoB, Glob: "docs/**"},
				{RepoID: repoA, Glob: "src/**"},
			},
			want: false,
		},
		{
			name: "different length",
			a:    []ComponentPath{{RepoID: repoA, Glob: "src/**"}},
			b: []ComponentPath{
				{RepoID: repoA, Glob: "src/**"},
				{RepoID: repoB, Glob: "docs/**"},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := componentPathsEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("componentPathsEqual = %v, want %v", got, tc.want)
			}
			// Parity with the behaviour this comparator replaced.
			if deep := reflect.DeepEqual(tc.a, tc.b); deep != tc.want {
				t.Fatalf("reflect.DeepEqual = %v, want %v — comparator and "+
					"DeepEqual disagree on this input", deep, tc.want)
			}
		})
	}
}

package mentions

import (
	"testing"

	"github.com/google/uuid"
)

// TestDiff covers the §F-9 PatchIssue diff: only newly-introduced
// usernames get notifications. Already-watching users stay watching
// (sticky no-op via AddIssueWatcher) but no fresh fan-out fires.
func TestDiff(t *testing.T) {
	cases := []struct {
		name string
		newC []string
		oldC []string
		want []string
	}{
		{
			name: "both empty",
			newC: nil,
			oldC: nil,
			want: nil,
		},
		{
			name: "no old, all new",
			newC: []string{"alice", "bob"},
			oldC: nil,
			want: []string{"alice", "bob"},
		},
		{
			name: "no new, drop everything",
			// Removing a mention from text doesn't generate a notification.
			newC: nil,
			oldC: []string{"alice"},
			want: nil,
		},
		{
			name: "exact same set",
			newC: []string{"alice", "bob"},
			oldC: []string{"alice", "bob"},
			want: nil,
		},
		{
			name: "added one",
			newC: []string{"alice", "bob", "charlie"},
			oldC: []string{"alice", "bob"},
			want: []string{"charlie"},
		},
		{
			name: "added one, removed one",
			// `bob` left, `dave` was added — only dave fires.
			newC: []string{"alice", "dave"},
			oldC: []string{"alice", "bob"},
			want: []string{"dave"},
		},
		{
			name: "preserves new-list order",
			newC: []string{"zelda", "alice", "ned"},
			oldC: nil,
			want: []string{"zelda", "alice", "ned"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Diff(tc.newC, tc.oldC)
			if !equalStringSlices(got, tc.want) {
				t.Errorf("Diff(%v, %v) = %v, want %v", tc.newC, tc.oldC, got, tc.want)
			}
		})
	}
}

// TestFilterOutSelf covers the §F-5 exclusion: an actor mentioning
// themselves doesn't get a notification or auto-watch.
func TestFilterOutSelf(t *testing.T) {
	alice := uuid.New()
	bob := uuid.New()

	resolved := []ResolvedUser{
		{Username: "alice", UserID: alice},
		{Username: "bob", UserID: bob},
	}

	t.Run("nil actor is a no-op", func(t *testing.T) {
		got := FilterOutSelf(resolved, uuid.Nil)
		if len(got) != 2 {
			t.Errorf("nil actor filter changed slice: got %d entries, want 2", len(got))
		}
	})

	t.Run("actor is alice — alice removed", func(t *testing.T) {
		got := FilterOutSelf(resolved, alice)
		if len(got) != 1 || got[0].Username != "bob" {
			t.Errorf("expected only bob, got %v", got)
		}
	})

	t.Run("actor not in list — slice unchanged", func(t *testing.T) {
		other := uuid.New()
		got := FilterOutSelf(resolved, other)
		if len(got) != 2 {
			t.Errorf("expected unchanged slice, got %d entries", len(got))
		}
	})

	t.Run("empty resolved — empty result", func(t *testing.T) {
		got := FilterOutSelf(nil, alice)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})
}

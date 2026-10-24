package push

import "testing"

// IsKnownCategory accepts every registered key and rejects others.
func TestIsKnownCategory(t *testing.T) {
	for _, c := range Categories {
		if !IsKnownCategory(c.Key) {
			t.Errorf("registry key %q should be known", c.Key)
		}
		if c.Label == "" {
			t.Errorf("category %q missing label", c.Key)
		}
	}
	for _, bad := range []string{"", "Assigned", "assign", "unknown", "reminder "} {
		if IsKnownCategory(bad) {
			t.Errorf("%q should NOT be a known category", bad)
		}
	}
}

// The documented categories are present.
func TestCategoriesContainCoreThree(t *testing.T) {
	want := map[string]bool{
		CategoryAssigned:        false,
		CategoryReminder:        false,
		CategoryAccepted:        false,
		CategoryDelegatedStatus: false,
	}
	for _, c := range Categories {
		if _, ok := want[c.Key]; ok {
			want[c.Key] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("category %q missing from registry", k)
		}
	}
}

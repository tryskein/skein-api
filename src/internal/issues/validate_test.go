package issues

import (
	"strings"
	"testing"
)

// TestValidateStatus_AcceptsStanding pins sk-118 (F2a): 'standing' is now
// a valid stored status. ValidateStatus must return it canonicalised and
// without error. The remaining cases keep the pre-existing contract green
// (the 4 lifecycle statuses accepted, 'blocked' rejected with the derived
// code, and a garbage value rejected with invalid_status).
func TestValidateStatus_AcceptsStanding(t *testing.T) {
	got, err := ValidateStatus("standing")
	if err != nil {
		t.Fatalf("ValidateStatus(standing) returned error: %v", err)
	}
	if got != StatusStanding {
		t.Fatalf("ValidateStatus(standing) = %q, want %q", got, StatusStanding)
	}

	// Trims surrounding whitespace, same as the other statuses.
	got, err = ValidateStatus("  standing  ")
	if err != nil {
		t.Fatalf("ValidateStatus(padded standing) returned error: %v", err)
	}
	if got != StatusStanding {
		t.Fatalf("ValidateStatus(padded standing) = %q, want %q", got, StatusStanding)
	}
}

// TestValidateStatus_ExistingCasesStillGreen guards against regressions in
// the rest of the closed set while F2a widens it.
func TestValidateStatus_ExistingCasesStillGreen(t *testing.T) {
	for _, s := range []string{StatusOpen, StatusInProgress, StatusDeferred, StatusClosed} {
		got, err := ValidateStatus(s)
		if err != nil {
			t.Errorf("ValidateStatus(%q) returned error: %v", s, err)
			continue
		}
		if got != s {
			t.Errorf("ValidateStatus(%q) = %q, want %q", s, got, s)
		}
	}

	// 'blocked' is still rejected with its dedicated derived-status code.
	if _, err := ValidateStatus(StatusBlocked); err == nil {
		t.Errorf("ValidateStatus(blocked) should error")
	} else if ve, ok := IsValidationError(err); !ok || ve.Code != "blocked_is_derived" {
		t.Errorf("ValidateStatus(blocked) code = %v, want blocked_is_derived", err)
	}

	// An unknown value is rejected with invalid_status.
	if _, err := ValidateStatus("bogus"); err == nil {
		t.Errorf("ValidateStatus(bogus) should error")
	} else if ve, ok := IsValidationError(err); !ok || ve.Code != "invalid_status" {
		t.Errorf("ValidateStatus(bogus) code = %v, want invalid_status", err)
	}
}

// TestValidateTitle_LengthInCharacters — every surface documents the title
// as 1..1000 characters; a title is counted in characters, not in the bytes
// of its UTF-8 form, so a non-ASCII title is held to the same limit.
func TestValidateTitle_LengthInCharacters(t *testing.T) {
	cases := []struct {
		name  string
		title string
		ok    bool
	}{
		{"1000 ASCII characters", strings.Repeat("a", 1000), true},
		{"1001 ASCII characters", strings.Repeat("a", 1001), false},
		{"1000 Cyrillic characters", strings.Repeat("ж", 1000), true},
		{"1001 Cyrillic characters", strings.Repeat("ж", 1001), false},
		{"surrounding spaces are not counted", "  " + strings.Repeat("ж", 1000) + "  ", true},
		{"blank", "   ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateTitle(c.title)
			if (err == nil) != c.ok {
				t.Fatalf("ValidateTitle(%d runes) = %v, want ok=%v", len([]rune(c.title)), err, c.ok)
			}
			if err != nil {
				if ve, ok := IsValidationError(err); !ok || ve.Code != "invalid_title" {
					t.Errorf("ValidateTitle error = %v, want code invalid_title", err)
				}
			}
		})
	}
}

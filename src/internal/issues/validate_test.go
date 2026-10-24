package issues

import "testing"

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

package auth

import "testing"

// TestSurfaceGateRejects exhausts the fail-closed decision table (sk-163
// S7a). It is DB-less and always runs, so the pure security invariant is
// covered even in CI without TEST_DATABASE_URL (the full middleware test
// skips there). Only (public surface, internal-owner) rejects; every other
// combination passes.
func TestSurfaceGateRejects(t *testing.T) {
	cases := []struct {
		name            string
		surfaceInternal bool
		ownerExternal   bool
		wantReject      bool
	}{
		{"internal surface, external owner → allow", true, true, false},
		{"internal surface, internal owner → allow", true, false, false},
		{"public surface, external owner → allow", false, true, false},
		{"public surface, internal owner → REJECT", false, false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := surfaceGateRejects(tc.surfaceInternal, tc.ownerExternal); got != tc.wantReject {
				t.Errorf("surfaceGateRejects(%v, %v) = %v, want %v",
					tc.surfaceInternal, tc.ownerExternal, got, tc.wantReject)
			}
		})
	}
}

// TestSurfaceIsInternal pins the marker-parsing helper: only the exact token
// "internal" (trim + case-insensitive) asserts the internal surface; absence
// and anything else are public (fail-closed). DB-less, always runs.
func TestSurfaceIsInternal(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},          // absent header → public
		{"internal", true},   // canonical
		{"INTERNAL", true},   // case-insensitive
		{" internal ", true}, // trimmed
		{"external", false},  // wrong token
		{"garbage", false},   // arbitrary
		{"internalx", false}, // no prefix/substring match
		{"public", false},    // wrong token
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			if got := SurfaceIsInternal(tc.in); got != tc.want {
				t.Errorf("SurfaceIsInternal(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

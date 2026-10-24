package dependencies

import "testing"

// TestValidateGate pins the gate↔type contract the agent guide and the
// skein_dep_add / skein_dep_add_many / skein_create schemas describe (sk-483):
// `gate` is OPTIONAL on waits-for (an absent or blank gate is accepted and
// nothing is stored), must be one of the two values when present, and is
// rejected with invalid_gate on every other type.
func TestValidateGate(t *testing.T) {
	strp := func(s string) *string { return &s }
	cases := []struct {
		name     string
		depType  string
		gate     *string
		want     string
		wantCode string // "" = accepted
	}{
		{name: "waits-for without gate accepted", depType: DepWaitsFor, gate: nil, want: ""},
		{name: "waits-for blank gate accepted as absent", depType: DepWaitsFor, gate: strp("  "), want: ""},
		{name: "waits-for all-children", depType: DepWaitsFor, gate: strp(GateAllChildren), want: GateAllChildren},
		{name: "waits-for any-children trimmed", depType: DepWaitsFor, gate: strp(" any-children "), want: GateAnyChildren},
		{name: "waits-for unknown gate", depType: DepWaitsFor, gate: strp("half-children"), wantCode: "invalid_gate"},
		{name: "blocks with gate", depType: DepBlocks, gate: strp(GateAllChildren), wantCode: "invalid_gate"},
		{name: "related with gate", depType: DepRelated, gate: strp(GateAnyChildren), wantCode: "invalid_gate"},
		{name: "blocks without gate", depType: DepBlocks, gate: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateGate(tc.depType, tc.gate)
			if tc.wantCode != "" {
				ve, ok := IsValidationError(err)
				if !ok || ve.Code != tc.wantCode {
					t.Fatalf("ValidateGate(%q, %v) err = %v, want validation code %q", tc.depType, tc.gate, err, tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateGate(%q, %v) err = %v, want nil", tc.depType, tc.gate, err)
			}
			if got != tc.want {
				t.Errorf("ValidateGate(%q, %v) = %q, want %q", tc.depType, tc.gate, got, tc.want)
			}
		})
	}
}

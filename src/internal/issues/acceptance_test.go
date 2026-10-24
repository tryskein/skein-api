package issues

// acceptance_test — sk-303.13, coverage-matrix row 34.
//
// Unit gate of the "journal versus delivery" decision on acceptance. No
// database: the decision IS a pure function, and that is why it is
// checkable in full, not by traces in tables.
//
// What is proven here and why these are two DIFFERENT claims:
//
//	event    — written on every transition of the mark from empty to
//	           filled, on any path (INV-5);
//	delivery — exists only for explicit accept; automatic acceptance
//	           gets no reason and stays journal-only (R-11, registry).
//
// Collapsing them into one flag is the very mistake this function was
// extracted to prevent: then "write to the audit" and "wake the creator"
// become one decision, and the audit hole can be closed only at the
// cost of a second notification for one action (INV-6).

import "testing"

func TestClassifyAcceptance(t *testing.T) {
	cases := []struct {
		name         string
		hadStamp     bool
		stampsNow    bool
		explicitTool bool
		want         AcceptanceOutcome
		why          string
	}{
		{
			name:         "explicit accept of an unaccepted issue",
			hadStamp:     false,
			stampsNow:    true,
			explicitTool: true,
			want:         AcceptanceOutcome{WriteEvent: true, Deliver: true},
			why:          "the canonical positive case: the transition is audited AND the creator is told",
		},
		{
			name:         "claim of an unaccepted issue",
			hadStamp:     false,
			stampsNow:    true,
			explicitTool: false,
			want:         AcceptanceOutcome{WriteEvent: true, Deliver: false},
			why:          "INV-5 wants the event; INV-6 forbids the second notification for the same action",
		},
		{
			name:         "creating an issue on yourself",
			hadStamp:     false,
			stampsNow:    true,
			explicitTool: false,
			want:         AcceptanceOutcome{WriteEvent: true, Deliver: false},
			why:          "same shape as the claim: the row is born stamped, and nobody needs telling",
		},
		{
			name:         "self-assign on patch",
			hadStamp:     false,
			stampsNow:    true,
			explicitTool: false,
			want:         AcceptanceOutcome{WriteEvent: true, Deliver: false},
			why:          "third automatic path, identical answer",
		},
		{
			name:         "repeat accept of an already-accepted issue",
			hadStamp:     true,
			stampsNow:    true,
			explicitTool: true,
			want:         AcceptanceOutcome{},
			why:          "no transition, so no event — idempotency is a property of the decision, not of the caller remembering to check",
		},
		{
			name:         "claim of an already-accepted issue",
			hadStamp:     true,
			stampsNow:    true,
			explicitTool: false,
			want:         AcceptanceOutcome{},
			why:          "the original acceptance timestamp stays the truth; re-stamping is not a transition",
		},
		{
			name:         "a write that touches no acceptance stamp",
			hadStamp:     false,
			stampsNow:    false,
			explicitTool: false,
			want:         AcceptanceOutcome{},
			why:          "renaming an issue is not an acceptance",
		},
		{
			name:         "reassignment clears the stamp",
			hadStamp:     true,
			stampsNow:    false,
			explicitTool: false,
			want:         AcceptanceOutcome{},
			why:          "the reverse transition writes nothing and deletes nothing — INV-5 is one-directional (US-11)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyAcceptance(tc.hadStamp, tc.stampsNow, tc.explicitTool)
			if got != tc.want {
				t.Errorf("ClassifyAcceptance(had=%v, stamps=%v, explicit=%v) = %+v, want %+v\nwhy: %s",
					tc.hadStamp, tc.stampsNow, tc.explicitTool, got, tc.want, tc.why)
			}
		})
	}
}

// TestClassifyAcceptance_DeliveryNeverWithoutEvent pins the relationship
// BETWEEN the two bits over the whole input space (8 combinations).
//
// A delivered acceptance that left no audit record would be the inverse of
// INV-5 and just as bad: the creator is told the task was accepted while the
// journal cannot say when or by whom. The table above would still pass if
// some future edit produced that combination in a case nobody listed, so the
// relationship is asserted exhaustively rather than case by case.
func TestClassifyAcceptance_DeliveryNeverWithoutEvent(t *testing.T) {
	for _, had := range []bool{false, true} {
		for _, stamps := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				got := ClassifyAcceptance(had, stamps, explicit)
				if got.Deliver && !got.WriteEvent {
					t.Errorf("ClassifyAcceptance(had=%v, stamps=%v, explicit=%v) delivers without auditing: %+v",
						had, stamps, explicit, got)
				}
				// And the delivery bit is decided by ONE input: automatic
				// acceptance never delivers, whatever else is true.
				if got.Deliver && !explicit {
					t.Errorf("ClassifyAcceptance(had=%v, stamps=%v, explicit=false) delivers; automatic acceptance is journal-only: %+v",
						had, stamps, got)
				}
			}
		}
	}
}

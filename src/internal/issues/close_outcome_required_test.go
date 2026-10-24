package issues

import "testing"

// sk-540.1 — the MCP surfaces demand an explicit outcome on a TRANSITION
// into closed. The rule lives in DecideCloseOutcome (the one resolver both
// writers call) behind CloseOutcomeRequest.RequireOnTransition; these cases
// pin its truth table without a database. Expected outcomes come from the
// task's acceptance: a transition without an outcome is refused on MCP
// (standing included), a re-close keeps the previous outcome, and the REST
// defaults (flag off) are unchanged.
func TestDecideCloseOutcome_RequireOnTransition(t *testing.T) {
	wontDo := CloseOutcomeWontDo

	tests := []struct {
		name        string
		in          CloseOutcomeRequest
		wantCode    string // non-empty: a *ValidationError with this code
		wantOutcome string
	}{
		{
			name:     "mcp transition from open without outcome is refused",
			in:       CloseOutcomeRequest{IssueID: "x-1", RequireOnTransition: true},
			wantCode: "close_outcome_required",
		},
		{
			name:     "mcp transition from standing without outcome is refused",
			in:       CloseOutcomeRequest{IssueID: "x-1", Standing: true, RequireOnTransition: true},
			wantCode: "close_outcome_required",
		},
		{
			name: "mcp re-close without outcome keeps a non-default previous outcome",
			in: CloseOutcomeRequest{
				IssueID: "x-1", AlreadyClosed: true, PreviousOutcome: CloseOutcomeWontDo,
				RequireOnTransition: true,
			},
			wantOutcome: CloseOutcomeWontDo,
		},
		{
			name:        "mcp transition with explicit outcome is accepted",
			in:          CloseOutcomeRequest{IssueID: "x-1", Requested: &wontDo, RequireOnTransition: true},
			wantOutcome: CloseOutcomeWontDo,
		},
		{
			name:        "rest transition without outcome still defaults to done",
			in:          CloseOutcomeRequest{IssueID: "x-1"},
			wantOutcome: CloseOutcomeDone,
		},
		{
			name:        "rest standing transition without outcome still defaults to done",
			in:          CloseOutcomeRequest{IssueID: "x-1", Standing: true},
			wantOutcome: CloseOutcomeDone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecideCloseOutcome(tc.in)
			if tc.wantCode != "" {
				ve, ok := IsValidationError(err)
				if !ok || ve.Code != tc.wantCode {
					t.Fatalf("DecideCloseOutcome(%+v) err = %v, want validation code %q", tc.in, err, tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecideCloseOutcome(%+v) err = %v, want nil", tc.in, err)
			}
			if got.Outcome != tc.wantOutcome {
				t.Errorf("DecideCloseOutcome(%+v).Outcome = %q, want %q", tc.in, got.Outcome, tc.wantOutcome)
			}
		})
	}
}

// The refusal message is what teaches the agent the correct call, so it must
// list every value of the closed set (acceptance: "validation_error with
// the list of values").
func TestCloseOutcomeRequiredError_ListsEveryValue(t *testing.T) {
	ve, ok := IsValidationError(CloseOutcomeRequiredError())
	if !ok {
		t.Fatal("CloseOutcomeRequiredError() is not a *ValidationError")
	}
	for _, v := range []string{"done", "wont_do", "duplicate", "superseded"} {
		if !containsWord(ve.Message, v) {
			t.Errorf("message %q does not list outcome %q", ve.Message, v)
		}
	}
}

func containsWord(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || !isIdentByte(s[i-1])
		after := i+len(w) == len(s) || !isIdentByte(s[i+len(w)])
		if before && after {
			return true
		}
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

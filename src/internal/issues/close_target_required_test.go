package issues

import "testing"

// sk-540.2 — duplicate/superseded need closeTarget. The truth table below is
// taken from the task's acceptance: refused without a target, EXCEPT
// (a) changing the outcome of an already closed issue (allowed only if the
// matching edge exists — the decider flags it, the writer checks it) and
// (b) a re-close that keeps the outcome, historical issues without an edge
// included. Form-only callers (RowStateUnknown) skip the rule entirely.
func TestDecideCloseOutcome_TargetRequired(t *testing.T) {
	dup := CloseOutcomeDuplicate
	sup := CloseOutcomeSuperseded

	tests := []struct {
		name         string
		in           CloseOutcomeRequest
		wantCode     string // non-empty: a *ValidationError with this code
		wantOutcome  string
		wantNeedLink bool
	}{
		{
			name:     "transition as duplicate without target is refused",
			in:       CloseOutcomeRequest{IssueID: "x-1", Requested: &dup},
			wantCode: "close_target_required",
		},
		{
			name:     "transition as superseded without target is refused (mcp surface)",
			in:       CloseOutcomeRequest{IssueID: "x-1", Requested: &sup, RequireOnTransition: true},
			wantCode: "close_target_required",
		},
		{
			name:     "archive of an open issue as duplicate without target is refused",
			in:       CloseOutcomeRequest{IssueID: "x-1", Requested: &dup, Archive: true},
			wantCode: "close_target_required",
		},
		{
			name:        "transition as duplicate with target is accepted",
			in:          CloseOutcomeRequest{IssueID: "x-1", Requested: &dup, Target: "x-2"},
			wantOutcome: CloseOutcomeDuplicate,
		},
		{
			name: "(b) re-close with the same explicit outcome needs no target",
			in: CloseOutcomeRequest{
				IssueID: "x-1", Requested: &sup, AlreadyClosed: true, PreviousOutcome: CloseOutcomeSuperseded,
			},
			wantOutcome: CloseOutcomeSuperseded,
		},
		{
			name: "(b) re-close omitting the outcome keeps duplicate without target",
			in: CloseOutcomeRequest{
				IssueID: "x-1", AlreadyClosed: true, PreviousOutcome: CloseOutcomeDuplicate, RequireOnTransition: true,
			},
			wantOutcome: CloseOutcomeDuplicate,
		},
		{
			name: "(a) changing a closed issue's outcome to duplicate flags the edge check",
			in: CloseOutcomeRequest{
				IssueID: "x-1", Requested: &dup, AlreadyClosed: true, PreviousOutcome: CloseOutcomeDone,
			},
			wantOutcome:  CloseOutcomeDuplicate,
			wantNeedLink: true,
		},
		{
			name: "(a) switching duplicate to superseded flags the edge check",
			in: CloseOutcomeRequest{
				IssueID: "x-1", Requested: &sup, AlreadyClosed: true, PreviousOutcome: CloseOutcomeDuplicate,
			},
			wantOutcome:  CloseOutcomeSuperseded,
			wantNeedLink: true,
		},
		{
			name: "changing a closed issue's outcome with a target needs no existing edge",
			in: CloseOutcomeRequest{
				IssueID: "x-1", Requested: &dup, Target: "x-2", AlreadyClosed: true, PreviousOutcome: CloseOutcomeDone,
			},
			wantOutcome: CloseOutcomeDuplicate,
		},
		{
			name:        "form-only call skips the row-dependent rule",
			in:          CloseOutcomeRequest{Requested: &dup, RowStateUnknown: true},
			wantOutcome: CloseOutcomeDuplicate,
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
			if got.NeedsExistingLink != tc.wantNeedLink {
				t.Errorf("DecideCloseOutcome(%+v).NeedsExistingLink = %v, want %v",
					tc.in, got.NeedsExistingLink, tc.wantNeedLink)
			}
		})
	}
}

package issues

import "testing"

// sk-540.4 — archiving decides per row. Reference: the task's acceptance
// matrix (owner decision 2026-09-29):
//   - unclosed non-standing row, no outcome → close_outcome_required (no
//     silent wont_do);
//   - unclosed row, outcome given → that outcome (duplicate/superseded still
//     need a target, sk-540.2);
//   - already closed row → keeps its outcome whatever was requested, and the
//     target is dropped;
//   - standing row → done whatever was requested, and the target is dropped;
//   - a form-only call (the batch preflight) never refuses on row state.
func TestDecideCloseOutcome_Archive(t *testing.T) {
	wontDo := CloseOutcomeWontDo
	dup := CloseOutcomeDuplicate

	tests := []struct {
		name        string
		in          CloseOutcomeRequest
		wantCode    string
		wantOutcome string
		wantDropped bool
		wantChanged bool
	}{
		{
			name:     "unclosed row without outcome is refused",
			in:       CloseOutcomeRequest{IssueID: "x-1", Archive: true},
			wantCode: "close_outcome_required",
		},
		{
			name:        "unclosed row takes the explicit outcome",
			in:          CloseOutcomeRequest{IssueID: "x-1", Archive: true, Requested: &wontDo},
			wantOutcome: CloseOutcomeWontDo,
		},
		{
			name:        "unclosed row takes duplicate with a target",
			in:          CloseOutcomeRequest{IssueID: "x-1", Archive: true, Requested: &dup, Target: "x-9"},
			wantOutcome: CloseOutcomeDuplicate,
		},
		{
			name:        "closed row without outcome keeps its outcome",
			in:          CloseOutcomeRequest{IssueID: "x-1", Archive: true, AlreadyClosed: true, PreviousOutcome: CloseOutcomeDone},
			wantOutcome: CloseOutcomeDone,
			wantDropped: true,
		},
		{
			name: "closed row ignores the requested outcome",
			in: CloseOutcomeRequest{IssueID: "x-1", Archive: true, Requested: &wontDo,
				AlreadyClosed: true, PreviousOutcome: CloseOutcomeDone},
			wantOutcome: CloseOutcomeDone,
			wantDropped: true,
		},
		{
			name: "closed row ignores duplicate without a target instead of refusing",
			in: CloseOutcomeRequest{IssueID: "x-1", Archive: true, Requested: &dup,
				AlreadyClosed: true, PreviousOutcome: CloseOutcomeWontDo},
			wantOutcome: CloseOutcomeWontDo,
			wantDropped: true,
		},
		{
			name: "closed row that is the target itself is not refused as self-target",
			in: CloseOutcomeRequest{IssueID: "x-1", Archive: true, Requested: &dup, Target: "x-1",
				AlreadyClosed: true, PreviousOutcome: CloseOutcomeDone},
			wantOutcome: CloseOutcomeDone,
			wantDropped: true,
		},
		{
			name:        "standing row without outcome gets done",
			in:          CloseOutcomeRequest{IssueID: "x-1", Archive: true, Standing: true},
			wantOutcome: CloseOutcomeDone,
			wantDropped: true,
		},
		{
			name: "standing row ignores outcome and target",
			in: CloseOutcomeRequest{IssueID: "x-1", Archive: true, Standing: true,
				Requested: &dup, Target: "x-9"},
			wantOutcome: CloseOutcomeDone,
			wantDropped: true,
		},
		{
			name:     "form of the request is still checked on a closed row",
			in:       CloseOutcomeRequest{IssueID: "x-1", Archive: true, Target: "x-9", AlreadyClosed: true, PreviousOutcome: CloseOutcomeDone},
			wantCode: "close_target_requires_outcome",
		},
		{
			name:        "form-only call does not refuse a missing outcome",
			in:          CloseOutcomeRequest{Archive: true, RowStateUnknown: true},
			wantOutcome: CloseOutcomeDone,
		},
		{
			name:        "a close that is not an archive keeps the done default",
			in:          CloseOutcomeRequest{IssueID: "x-1"},
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
			if got.TargetDropped != tc.wantDropped {
				t.Errorf("DecideCloseOutcome(%+v).TargetDropped = %v, want %v", tc.in, got.TargetDropped, tc.wantDropped)
			}
			if got.Changed != tc.wantChanged {
				t.Errorf("DecideCloseOutcome(%+v).Changed = %v, want %v", tc.in, got.Changed, tc.wantChanged)
			}
		})
	}
}

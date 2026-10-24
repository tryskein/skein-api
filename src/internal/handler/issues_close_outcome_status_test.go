package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/issues"
)

// sk-489 (review fix A/2) — two close outcome refusals, two DIFFERENT boundaries.
//
// The storage returns two typed refusals for the close outcome, and their HTTP class
// differs:
//
//   - `close_outcome_not_a_close` (issues.Store.UpdateStatus) — an outcome or target
//     passed together with a transition to a status other than "closed". Such a
//     combination of fields is impossible in any state of the issue: an error of the
//     request SHAPE → 400.
//   - `close_outcome_requires_closed` (issues.Store.PatchIssueWithResult) —
//     an outcome edit of an issue that is NOT closed yet. The request is correct, the
//     current state forbids it: a business refusal → 422
//     (the project guidelines, "HTTP codes for business errors").
//
// The status is chosen by exactly one predicate — `issueValidationIsBusinessError`, and
// it chooses it EXCLUSIVELY by code. While there was one code for both refusals, these two
// contract lines could not be green at the same time: add the code to
// the list and the transition starts answering 422, leave it out and the edit answers 400.
// The test holds both sides at once, so it goes red both on "forgot to add the
// business code" and on "added an extra one" — that is, exactly on an attempt to collapse
// the two refusals into one class again.
//
// Level — unit, modelled on `claim_standing_rest_test.go`: the subject here is
// precisely the mapping of a code to a status, and WHO returns which code is proven
// by the integration tables of the writers in `internal/issues`.
func TestCloseOutcome_RefusalCodesMapToDifferentStatuses(t *testing.T) {
	cases := []struct {
		name       string
		code       string
		message    string
		wantStatus int
	}{
		{
			name:       "transition with an outcome — shape error",
			code:       "close_outcome_not_a_close",
			message:    "closeOutcome and closeTarget are only accepted when the new status is closed",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "outcome edit on a non-closed issue — business refusal",
			code:       "close_outcome_requires_closed",
			message:    "closeOutcome can only be edited on a closed issue",
			wantStatus: http.StatusUnprocessableEntity,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handled := mapIssueValidation(rec, &issues.ValidationError{Code: c.code, Message: c.message})
			if !handled {
				t.Fatalf("mapIssueValidation did not handle %s", c.code)
			}
			if rec.Code != c.wantStatus {
				t.Fatalf("%s → HTTP %d, want %d", c.code, rec.Code, c.wantStatus)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("parse problem body: %v (raw=%s)", err, rec.Body.String())
			}
			// The code is a stable machine identifier: clients branch on it,
			// and it must reach the response body.
			if got, _ := body["error"].(string); got != c.code {
				t.Fatalf("refusal body carries error=%v, want %s", body["error"], c.code)
			}
		})
	}
}

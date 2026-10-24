package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/issues"
)

// sk-118 (F2d) — REST-boundary status for the standing-claim rejection.
//
// skein-api exposes claim ONLY through the MCP tool surface; there is no
// standalone REST POST /claim route (the web UI claims via the /mcp proxy,
// which calls the same handleClaim → Store.Claim path). The REST contract that
// matters is therefore the shared error-mapper `mapIssueValidation`: a REST
// claim route — or any REST path that bubbles a Store.Claim error — must surface
// the typed issues.ErrCannotClaimStanding (a *ValidationError, code
// `cannot_claim_standing`) as HTTP 422, NOT the default 400. 422 is the org-wide
// status for business errors (the project guidelines)
// and keeps standing-claim consistent with the other closed-/locked-state
// business errors (version_conflict, locked_by_other) already mapped to 422.
func TestClaim_StandingRejected_REST(t *testing.T) {
	rec := httptest.NewRecorder()

	// The exact error Store.Claim returns when a standing issue is claimed.
	handled := mapIssueValidation(rec, issues.ErrCannotClaimStanding)
	if !handled {
		t.Fatalf("mapIssueValidation returned false for ErrCannotClaimStanding; want it handled")
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("HTTP status = %d, want 422 for cannot_claim_standing", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v (raw=%s)", err, rec.Body.String())
	}
	// writeProblem emits the stable code under `error`; agents/clients branch on it.
	if got, _ := body["error"].(string); got != "cannot_claim_standing" {
		t.Fatalf("problem body error = %v, want cannot_claim_standing", body["error"])
	}
}

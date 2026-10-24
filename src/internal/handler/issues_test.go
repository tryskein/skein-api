package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"skein-api/internal/issues"
)

// TestMapIssueErr_LockedByOther_REST_Returns422 covers W6 — verify that
// REST PATCH on a locked issue surfaces 422 with code "locked_by_other"
// and the lockedBy session id, mirroring the MCP tool error code so REST
// and MCP agents see the same code on the same condition.
func TestMapIssueErr_LockedByOther_REST_Returns422(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &issues.ErrLockedByOther{LockedBy: "sess-A"}

	if !mapIssueErr(rec, err) {
		t.Fatalf("mapIssueErr returned false; expected it to handle ErrLockedByOther")
	}

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != 422 {
		t.Fatalf("status = %d, want 422", res.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["error"]; got != "locked_by_other" {
		t.Fatalf("error = %v, want \"locked_by_other\"", got)
	}
	if got := body["lockedBy"]; got != "sess-A" {
		t.Fatalf("lockedBy = %v, want \"sess-A\"", got)
	}
}

// TestMapIssueErr_SerializationRetry_REST_Returns422 pins the sk-303.17
// coderev fix at the REST boundary. AddComment / UpdateComment /
// DeleteComment exhausting their SERIALIZABLE retry budget now returns
// issues.ErrSerializationRetry from the store — before this fix that
// code did not exist as a distinct mapIssueErr branch, and (worse, for
// DeleteComment specifically) the DeleteComment handler didn't call
// mapIssueErr AT ALL, so any error past ErrCommentNotFound fell straight
// to a bare 500. This pins the mapIssueErr side of that fix; the
// DeleteComment-calls-mapIssueErr side has no independent unit test (it
// would need a DB-backed handler test) but is visible by inspection at
// internal/handler/issues.go's DeleteComment handler.
//
// Red on the pre-fix mapIssueErr (no issues.ErrSerializationRetry
// branch): mapIssueErr returns false and the caller falls through to a
// bare internal_error/500.
func TestMapIssueErr_SerializationRetry_REST_Returns422(t *testing.T) {
	rec := httptest.NewRecorder()
	err := issues.ErrSerializationRetry

	if !mapIssueErr(rec, err) {
		t.Fatalf("mapIssueErr returned false; expected it to handle ErrSerializationRetry")
	}

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != 422 {
		t.Fatalf("status = %d, want 422", res.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["error"]; got != "serialization_retry" {
		t.Fatalf("error = %v, want \"serialization_retry\"", got)
	}
	if got := body["error"]; got == "version_conflict" {
		t.Fatalf("a lost serialization race reported as version_conflict — the resource's version did not move")
	}
}

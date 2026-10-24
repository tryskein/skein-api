package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/issues"
)

// TestParseTreeMaxDepth — acceptance of depth parameter parsing (sk-421.2 T006).
// Pure unit: no DB needed, so the test cannot be skipped silently.
func TestParseTreeMaxDepth(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantN    int
		wantOK   bool
		wantCode string
	}{
		{"absent — not set", "", 0, true, ""},
		{"empty string — not set", "maxDepth=", 0, true, ""},
		{"one — direct children", "maxDepth=1", 1, true, ""},
		{"ceiling is allowed", "maxDepth=32", issues.IssueTreeMaxDepth, true, ""},
		{"non-integer", "maxDepth=abc", 0, false, "invalid_max_depth"},
		{"fractional", "maxDepth=1.5", 0, false, "invalid_max_depth"},
		{"zero", "maxDepth=0", 0, false, "invalid_max_depth"},
		{"negative", "maxDepth=-1", 0, false, "invalid_max_depth"},
		{"above ceiling", "maxDepth=33", 0, false, "invalid_max_depth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := "/api/v1/issues/tree"
			if c.query != "" {
				url += "?" + c.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()

			got, ok := parseTreeMaxDepth(rec, req)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if got != c.wantN {
				t.Errorf("depth = %d, want %d", got, c.wantN)
			}
			if c.wantOK {
				if rec.Code != http.StatusOK {
					t.Errorf("on success no response is written, but code %d was written", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("response code = %d, want 400 — this is range validation "+
					"of a request parameter, not a business rejection", rec.Code)
			}
			var problem map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
				t.Fatalf("rejection body cannot be parsed: %v", err)
			}
			// writeProblem writes a FLAT document: the code key is called
			// "error", and extra is laid out at the top level, not nested.
			if problem["error"] != c.wantCode {
				t.Errorf("error = %v, want %q (body: %v)", problem["error"], c.wantCode, problem)
			}
		})
	}
}

// TestParseTreeMaxDepth_CeilingComesFromTheStore pins that the ceiling in
// the rejection is the same one the traversal uses, not a re-typed constant.
func TestParseTreeMaxDepth_CeilingComesFromTheStore(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/tree?maxDepth=999", nil)
	rec := httptest.NewRecorder()
	if _, ok := parseTreeMaxDepth(rec, req); ok {
		t.Fatalf("999 must be rejected at ceiling %d", issues.IssueTreeMaxDepth)
	}
	var problem map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&problem)
	if got, want := problem["max"], float64(issues.IssueTreeMaxDepth); got != want {
		t.Errorf("max in rejection = %v, want %v (body: %v)", got, want, problem)
	}
}

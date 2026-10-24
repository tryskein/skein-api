package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sk-421.2.8 T005, test-plan row 7 — parsing of ?includeBlockers= (R-8).
//
// Exactly `true` and `false` are valid; an absent or empty value counts as
// no flag. Everything else is a 400 invalid_include_blockers. Modelled on
// TestParseTreeMaxDepth; the model's limit: it does not check case or the
// strconv.ParseBool boolean synonyms, which R-8 forbids accepting silently.
func TestParseTreeIncludeBlockers(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		want   bool
		wantOK bool
	}{
		{"absent — no flag", "", false, true},
		{"empty string — no flag", "includeBlockers=", false, true},
		{"true — include", "includeBlockers=true", true, true},
		{"false — do not include", "includeBlockers=false", false, true},

		// Synonyms and case that strconv.ParseBool would accept silently.
		{"1 — ParseBool synonym", "includeBlockers=1", false, false},
		{"t — ParseBool synonym", "includeBlockers=t", false, false},
		{"TRUE — different case", "includeBlockers=TRUE", false, false},
		{"True — different case", "includeBlockers=True", false, false},
		{"yes — not a boolean value", "includeBlockers=yes", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := "/api/v1/issues/tree"
			if c.query != "" {
				url += "?" + c.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()

			got, ok := parseTreeIncludeBlockers(rec, req)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if got != c.want {
				t.Errorf("flag = %v, want %v", got, c.want)
			}
			if c.wantOK {
				if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
					t.Errorf("on success no response is written, but code %d and body %q were written", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (parameter syntax, class invalid_max_depth)", rec.Code)
			}
			var problem map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
				t.Fatalf("rejection body is not JSON: %v", err)
			}
			if problem["error"] != "invalid_include_blockers" {
				t.Errorf("code in body = %v, want invalid_include_blockers (body: %v)", problem["error"], problem)
			}
		})
	}
}

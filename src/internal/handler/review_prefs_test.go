package handler

// review_prefs_test.go — sk-303.14 (T010), the modular slice of the settings
// contract: parsing the three states of a schedule field on the way in. The
// three states are a load-bearing property of the contract, not a convenience:
// without them "reset the parameter to factory" is inexpressible, and "field
// omitted" would silently become zero.

import (
	"encoding/json"
	"testing"
)

func TestParseNullableIntThreeStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantSet bool
		wantVal *int
	}{
		{"key absent — do not touch", `{}`, false, nil},
		{"explicit null — empty, factory", `{"burstCount":null}`, true, nil},
		{"explicit zero — no burst", `{"burstCount":0}`, true, intPtr(0)},
		{"number", `{"burstCount":6}`, true, intPtr(6)},
		{"negative is parsed; bounds are not this layer", `{"burstCount":-1}`, true, intPtr(-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req putReviewPrefsRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("body %s not parsed: %v", tc.body, err)
			}
			set, val, err := parseNullableInt(req.BurstCount)
			if err != nil {
				t.Fatalf("parse rejected: %v", err)
			}
			if set != tc.wantSet {
				t.Errorf("set = %v, want %v", set, tc.wantSet)
			}
			switch {
			case tc.wantVal == nil && val != nil:
				t.Errorf("got value %d, want empty", *val)
			case tc.wantVal != nil && val == nil:
				t.Errorf("got empty, want %d", *tc.wantVal)
			case tc.wantVal != nil && *val != *tc.wantVal:
				t.Errorf("got %d, want %d", *val, *tc.wantVal)
			}
		})
	}
}

func TestParseNullableIntRejectsNonNumbers(t *testing.T) {
	t.Parallel()

	// Not a number and not null — a BODY-SHAPE error (400), not a business
	// refusal: a value like this never reaches the schedule bounds.
	for _, body := range []string{
		`{"burstCount":"3"}`,
		`{"burstCount":3.5}`,
		`{"burstCount":true}`,
		`{"burstCount":[3]}`,
		`{"burstCount":{"value":3}}`,
	} {
		var req putReviewPrefsRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("body %s not parsed at the top level: %v", body, err)
		}
		if _, _, err := parseNullableInt(req.BurstCount); err == nil {
			t.Errorf("body %s accepted even though the value is not an integer", body)
		}
	}
}

func intPtr(v int) *int { return &v }

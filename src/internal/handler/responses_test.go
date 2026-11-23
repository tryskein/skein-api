package handler

// responses_test.go — the wire form of the two response writers every handler
// of this package funnels through: writeJSON (success bodies) and writeProblem
// (refusals). Reference — the doc comments of problemBody and writeProblem:
// a refusal is one flat JSON object {"error":"<code>","message":"<msg>",...extra},
// the message is omitted when empty, the structured context of extra is
// spliced in next to the code; both writers answer with a JSON Content-Type.
// A client that dispatches on Content-Type (the BFF proxy, fetch wrappers)
// treats a body without it as text, so the header is part of the contract.
//
// The writers are observed on a recorded HTTP response: status, the headers as
// committed with the status (rec.Result(), not the still mutable header map) and
// the decoded body. Message text is never asserted beyond presence: it is free
// text (TEST_GUIDELINES §5); the test passes its own value and checks that it
// arrives.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordedJSON decodes a recorded body into a JSON object.
func recordedJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", rec.Body.String(), err)
	}
	return body
}

func requireJSONContentType(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestWriteProblem_WireForm(t *testing.T) {
	tests := []struct {
		name  string
		msg   string
		extra map[string]any
		// wantKeys is the exact key set of the body.
		wantKeys []string
	}{
		{"code and message", "a human-readable reason", nil, []string{"error", "message"}},
		{"code alone: an empty message is omitted", "", nil, []string{"error"}},
		{"structured context next to code and message", "a human-readable reason",
			map[string]any{"suggestions": []any{"alpha", "beta"}, "spaceId": "abc123"},
			[]string{"error", "message", "suggestions", "spaceId"}},
		{"structured context with an empty message omits the message", "",
			map[string]any{"suggestions": []any{}},
			[]string{"error", "suggestions"}},
		{"the code wins over an extra key of the same name", "why",
			map[string]any{"error": "not-the-code", "field": "name"},
			[]string{"error", "message", "field"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeProblem(rec, http.StatusUnprocessableEntity, "some_code", tc.msg, tc.extra)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", rec.Code)
			}
			requireJSONContentType(t, rec)
			body := recordedJSON(t, rec)

			if body["error"] != "some_code" {
				t.Errorf("error = %#v, want the machine code some_code", body["error"])
			}
			if len(body) != len(tc.wantKeys) {
				t.Errorf("body keys = %v, want exactly %v", keysOf(body), tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if _, ok := body[k]; !ok {
					t.Errorf("key %q is missing from %v", k, body)
				}
			}
			if tc.msg != "" && body["message"] != tc.msg {
				t.Errorf("message = %#v, want the given %q", body["message"], tc.msg)
			}
			for k, v := range tc.extra {
				if k == "error" {
					continue
				}
				got, _ := json.Marshal(body[k])
				want, _ := json.Marshal(v)
				if string(got) != string(want) {
					t.Errorf("extra %q = %s, want %s", k, got, want)
				}
			}
		})
	}
}

func TestWriteJSON_WireForm(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]any{"id": "x-1", "count": 2})

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want the given 201", rec.Code)
	}
	requireJSONContentType(t, rec)
	body := recordedJSON(t, rec)
	if body["id"] != "x-1" || body["count"] != float64(2) {
		t.Errorf("body = %v, want {id: x-1, count: 2}", body)
	}
}

func TestMethod405_WireForm(t *testing.T) {
	rec := httptest.NewRecorder()
	Method405("GET, PATCH, POST")(rec, httptest.NewRequest(http.MethodDelete, "/ui/v1/projects/x", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if got := rec.Result().Header.Get("Allow"); got != "GET, PATCH, POST" {
		t.Errorf("Allow = %q, want the given verbs", got)
	}
	requireJSONContentType(t, rec)
	if body := recordedJSON(t, rec); body["error"] != "method_not_allowed" {
		t.Errorf("error = %#v, want method_not_allowed", body["error"])
	}
}

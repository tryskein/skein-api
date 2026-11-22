package handler

// custom_watcher_limits_test.go — POST /me/custom-watchers refuses a filter
// over a limit (sk-977.7.73) with 422 validation_error and the case in
// `code`, the same check as MCP filter_create, before the store is reached.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

func TestCreateCustomWatcher_FilterLimitsAre422(t *testing.T) {
	labels := func(label string, n int) string {
		ls := make([]string, n)
		for i := range ls {
			ls[i] = label
		}
		b, err := json.Marshal(map[string]any{"name": "limits", "filter": map[string]any{"labels": ls}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tc := range []struct {
		name     string
		body     string
		wantCase string
	}{
		{"51 labels", labels("bug", 51), "filter_too_many_values"},
		{"label of 101 characters", labels(strings.Repeat("a", 101), 1), "invalid_label"},
		{"over 16 KiB encoded", labels(strings.Repeat("\U0001F600", 100), 50), "filter_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No pool: the refusal must come before the store.
			h := NewNotificationsHandler(nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/me/custom-watchers", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			ctx := auth.WithUserID(req.Context(), uuid.New())
			ctx = auth.WithUserRole(ctx, "user")
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			h.CreateCustomWatcher(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["error"] != "validation_error" || body["code"] != tc.wantCase {
				t.Fatalf("error = %v, code = %v; want validation_error, %s", body["error"], body["code"], tc.wantCase)
			}
		})
	}
}

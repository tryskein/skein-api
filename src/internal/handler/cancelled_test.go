package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnswerCancelled — a 5xx answer of a request its client cancelled is
// answered 408 request_cancelled; a timeout of our own, a live request and a
// non-error answer pass unchanged. Red when a client's cancellation is
// reported as a server error, or when a real one is hidden.
func TestAnswerCancelled(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-timedOut.Done()

	cases := []struct {
		name       string
		ctx        context.Context
		status     int
		wantStatus int
		wantCode   string
	}{
		{"cancelled, 500", canceled, http.StatusInternalServerError, http.StatusRequestTimeout, "request_cancelled"},
		{"cancelled, 503", canceled, http.StatusServiceUnavailable, http.StatusRequestTimeout, "request_cancelled"},
		{"cancelled, 200", canceled, http.StatusOK, http.StatusOK, ""},
		{"cancelled, 422", canceled, http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, "refused"},
		{"timed out, 500", timedOut, http.StatusInternalServerError, http.StatusInternalServerError, "internal_error"},
		{"live, 500", context.Background(), http.StatusInternalServerError, http.StatusInternalServerError, "internal_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := AnswerCancelledInternal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				code := "internal_error"
				if c.status < 500 {
					code = "refused"
				}
				if c.status == http.StatusOK {
					writeJSON(w, http.StatusOK, map[string]string{})
					return
				}
				writeProblem(w, c.status, code, "", nil)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(c.ctx))
			if rec.Code != c.wantStatus {
				t.Fatalf("status %d, want %d (%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not one JSON object: %v", rec.Body.String(), err)
			}
			if c.wantCode != "" && body["error"] != c.wantCode {
				t.Errorf("error %v, want %s", body["error"], c.wantCode)
			}
		})
	}
}

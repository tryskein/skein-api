package main

// cancelled_requests_test.go — a request its client cancelled is not counted
// or answered as a server error on either REST surface of the production
// wiring: the internal API answers 408 request_cancelled (as /search does)
// and http_requests_total records 408, not 500; the public API answers its
// request_cancelled refusal (retryable). The reference is the review of the
// defect: a cancelled GET /ui/v1/issues was answered and counted as 500.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// requestsCounted sums http_requests_total of the internal entry for path and
// status, as the router's /metrics exports it.
func requestsCounted(t *testing.T, s *parityStack, path, status string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	total := 0.0
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "http_requests_total{") || !strings.Contains(line, `entry="internal"`) ||
			!strings.Contains(line, `path="`+path+`"`) || !strings.Contains(line, `status="`+status+`"`) {
			continue
		}
		v, err := strconv.ParseFloat(line[strings.LastIndex(line, " ")+1:], 64)
		if err != nil {
			t.Fatalf("unparseable %q: %v", line, err)
		}
		total += v
	}
	return total
}

func TestCancelledRequests_NotServerErrors(t *testing.T) {
	t.Run("internal API", func(t *testing.T) {
		s := newParityStack(t)
		u := s.f.User(t, testsupport.ActorProjectOwner)
		const path = "/ui/v1/issues"
		before500, before408 := requestsCounted(t, s, path, "500"), requestsCounted(t, s, path, "408")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		req.Header.Set(auth.HeaderUserID, u.ID.String())
		req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
		req.Header.Set(auth.HeaderUsername, u.Username)
		req.Header.Set(auth.HeaderUserRole, u.Role)
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestTimeout || !strings.Contains(rec.Body.String(), `"request_cancelled"`) {
			t.Fatalf("answered %d %s, want 408 request_cancelled", rec.Code, rec.Body.String())
		}
		if d := requestsCounted(t, s, path, "500") - before500; d != 0 {
			t.Errorf("status 500 counted %v more times, want 0", d)
		}
		if d := requestsCounted(t, s, path, "408") - before408; d != 1 {
			t.Errorf("status 408 counted %v more times, want 1", d)
		}
	})

	t.Run("public API", func(t *testing.T) {
		w := newClassWorld(t)
		key := w.key(w.owner, levelKey(apikeys.LevelRead))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?project="+w.pPrefix, nil).WithContext(ctx)
		req.Header.Set(auth.HeaderAPIKey, key)
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("answer %d %q: %v", rec.Code, rec.Body.String(), err)
		}
		if rec.Code >= 500 || body["code"] != "request_cancelled" || body["retryable"] != true {
			t.Errorf("answered %d %s, want request_cancelled, retryable, not a 5xx", rec.Code, rec.Body.String())
		}
	})
}

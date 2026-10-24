package handler

// projects_prefix_ratelimit_test.go — sk-674.18 T025, matrix row 108
// (US-15). Reference — US-15 "the check rate is limited" and the rule of
// section 9 of the test rationale: the limit of GET /projects/prefix-check
// counts the caller — a positive limit, then a refusal rate_limited, a fresh
// window after it; another caller has his own count; the checked prefix is
// not part of the key, so changing it does not lift the refusal. Through the
// route's handler, with managed clocks (the limiter's now). The checked
// prefixes break the prefix rule, so the answer comes without the database.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/auth"
)

func TestPrefixCheck_RateLimitedPerCaller(t *testing.T) {
	h := NewProjectsHandler(nil, nil)
	clock := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	h.prefixLimiter.now = func() time.Time { return clock }
	r := chi.NewRouter()
	r.With(ParseUserHeaders, RequireUser).Get("/projects/prefix-check", h.PrefixCheck)

	check := func(user uuid.UUID, prefix string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/projects/prefix-check?prefix="+prefix, nil)
		req.Header.Set(auth.HeaderUserID, user.String())
		req.Header.Set(auth.HeaderUsername, "u"+user.String()[:8])
		req.Header.Set(auth.HeaderUserRole, "user")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Error
	}

	me, other := uuid.New(), uuid.New()
	passed := 0
	for i := 0; i < 1000; i++ {
		code, _ := check(me, "1abc")
		if code != http.StatusOK {
			break
		}
		passed++
	}
	if passed == 0 || passed == 1000 {
		t.Fatalf("%d checks passed in one window, want a positive limit", passed)
	}
	if code, e := check(me, "1abc"); code != http.StatusTooManyRequests || e != "rate_limited" {
		t.Errorf("check %d: %d %q, want 429 rate_limited", passed+1, code, e)
	}
	if code, e := check(me, "2xyz"); code != http.StatusTooManyRequests || e != "rate_limited" {
		t.Errorf("another prefix lifted the refusal: %d %q", code, e)
	}
	if code, _ := check(other, "1abc"); code != http.StatusOK {
		t.Errorf("another caller in the same window: %d, want his own count", code)
	}
	clock = clock.Add(prefixCheckWindow + time.Second)
	if code, _ := check(me, "1abc"); code != http.StatusOK {
		t.Errorf("after the window: %d, want a fresh count", code)
	}
}

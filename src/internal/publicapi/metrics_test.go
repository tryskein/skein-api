package publicapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/publicapi"
	"skein-api/internal/testsupport"
)

// gathered is the value of the series of metric name whose labels are
// exactly want, read from the default registry (0 when absent).
func gathered(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != len(want) {
				continue
			}
			for _, l := range m.GetLabel() {
				if want[l.GetName()] != l.GetValue() {
					continue series
				}
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			return float64(m.GetHistogram().GetSampleCount())
		}
	}
	return 0
}

// TestPublicMetrics_SeriesMove — row 67 (SC-021, "Limits: Observability").
// A request through the public router moves http_requests_total with
// entry="public" and the route pattern; one through the internal middleware
// moves entry="internal"; a credential failure moves the series of its
// reason, an internal error the series "internal_error", each limit its own
// series. Changes of values are observed, not declarations.
func TestPublicMetrics_SeriesMove(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")

	t.Run("requests by entry and route pattern", func(t *testing.T) {
		rig := newChainRig(t, pool, nil, 100, 100)
		key, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		public := map[string]string{"method": "GET", "path": "/api/v1/test-things", "status": "200", "entry": "public"}
		before := gathered(t, "http_requests_total", public)
		durBefore := gathered(t, "http_request_duration_seconds", map[string]string{"method": "GET", "path": "/api/v1/test-things", "entry": "public"})
		if rec := rig.do(t, req{ip: "203.0.113.80", key: key}); rec.Code != http.StatusOK {
			t.Fatalf("request: %d", rec.Code)
		}
		if got := gathered(t, "http_requests_total", public) - before; got != 1 {
			t.Errorf("http_requests_total%v delta %v, want 1", public, got)
		}
		if got := gathered(t, "http_request_duration_seconds", map[string]string{"method": "GET", "path": "/api/v1/test-things", "entry": "public"}) - durBefore; got != 1 {
			t.Errorf("http_request_duration_seconds{entry=public} samples delta %v, want 1", got)
		}

		internal := map[string]string{"method": "GET", "path": "/ui/v1/__public_metrics_probe", "status": "200", "entry": "internal"}
		before = gathered(t, "http_requests_total", internal)
		r := chi.NewRouter()
		r.Use(handler.HTTPMetrics)
		r.Get("/ui/v1/__public_metrics_probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ui/v1/__public_metrics_probe", nil))
		if got := gathered(t, "http_requests_total", internal) - before; got != 1 {
			t.Errorf("http_requests_total%v delta %v, want 1", internal, got)
		}
	})

	t.Run("authentication failures by reason", func(t *testing.T) {
		rig := newChainRig(t, pool, nil, 100, 100)
		revoked, revokedID := mintKey(t, pool, owner, level(apikeys.LevelRead))
		malformed := publicapi.AuthFailuresTotal.WithLabelValues("malformed")
		before := testutil.ToFloat64(malformed)
		if rec := rig.do(t, req{ip: "203.0.113.81", key: "not-a-key"}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("malformed key: %d", rec.Code)
		}
		if got := testutil.ToFloat64(malformed) - before; got != 1 {
			t.Errorf("auth failures{reason=malformed} delta %v, want 1", got)
		}

		if _, err := pool.Exec(t.Context(), `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, revokedID); err != nil {
			t.Fatal(err)
		}
		rev := publicapi.AuthFailuresTotal.WithLabelValues("revoked")
		before = testutil.ToFloat64(rev)
		if rec := rig.do(t, req{ip: "203.0.113.81", key: revoked}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked key: %d", rec.Code)
		}
		if got := testutil.ToFloat64(rev) - before; got != 1 {
			t.Errorf("auth failures{reason=revoked} delta %v, want 1", got)
		}
	})

	t.Run("internal error", func(t *testing.T) {
		broken := openExtraPool(t)
		broken.Close()
		rig := newChainRig(t, pool, auth.NewAPIKeyAuthenticator(broken, auth.MinBcryptCost), 100, 100)
		key, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		internal := publicapi.AuthFailuresTotal.WithLabelValues("internal_error")
		before := testutil.ToFloat64(internal)
		if rec := rig.do(t, req{ip: "203.0.113.82", key: key}); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("closed pool: %d, want 503", rec.Code)
		}
		if got := testutil.ToFloat64(internal) - before; got != 1 {
			t.Errorf("auth failures{reason=internal_error} delta %v, want 1", got)
		}
	})

	t.Run("limits", func(t *testing.T) {
		rig := newChainRig(t, pool, nil, 1, 1)
		key, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		cold, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))

		reqs := publicapi.RateLimitedTotal.WithLabelValues("requests")
		before := testutil.ToFloat64(reqs)
		rig.do(t, req{ip: "203.0.113.83", key: key})
		if rec := rig.do(t, req{ip: "203.0.113.83", key: key}); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("second request of a key limited to one: %d", rec.Code)
		}
		if got := testutil.ToFloat64(reqs) - before; got != 1 {
			t.Errorf("rate limited{limit=requests} delta %v, want 1", got)
		}

		fails := publicapi.RateLimitedTotal.WithLabelValues("auth_failures")
		failLimit := publicapi.AuthFailuresTotal.WithLabelValues("failure_limit")
		before, beforeReason := testutil.ToFloat64(fails), testutil.ToFloat64(failLimit)
		rig.do(t, req{ip: "203.0.113.84", key: "not-a-key"})
		if rec := rig.do(t, req{ip: "203.0.113.84", key: cold}); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("cold key from an address over its failure limit: %d", rec.Code)
		}
		if got := testutil.ToFloat64(fails) - before; got != 1 {
			t.Errorf("rate limited{limit=auth_failures} delta %v, want 1", got)
		}
		if got := testutil.ToFloat64(failLimit) - beforeReason; got != 1 {
			t.Errorf("auth failures{reason=failure_limit} delta %v, want 1", got)
		}
	})

	t.Run("key right refusals", func(t *testing.T) {
		rig := newChainRig(t, pool, nil, 100, 100)
		key, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		write := publicapi.ScopeRefusalsTotal.WithLabelValues("write")
		before := testutil.ToFloat64(write)
		if rec := rig.do(t, req{method: http.MethodPost, path: pathWrite, ip: "203.0.113.85", key: key}); rec.Code != http.StatusForbidden {
			t.Fatalf("read key on a write operation: %d", rec.Code)
		}
		if got := testutil.ToFloat64(write) - before; got != 1 {
			t.Errorf("scope refusals{required_right=write} delta %v, want 1", got)
		}
	})
}

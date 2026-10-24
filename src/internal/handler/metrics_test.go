package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestCollectors builds a fresh, UNREGISTERED pair of collectors with the
// same label sets as the package-level ones. Tests observe these instead of
// the default registry so they neither depend on nor perturb process-wide
// state.
func newTestCollectors() (*prometheus.CounterVec, *prometheus.HistogramVec) {
	reqs := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total"},
		[]string{"method", "path", "status"},
	)
	dur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "http_request_duration_seconds", Buckets: prometheus.DefBuckets},
		[]string{"method", "path"},
	)
	return reqs, dur
}

// newTestRouter mounts a router shaped like the real one: probe routes at the
// root, a parametrised REST route and a mounted subtree standing in for /mcp.
func newTestRouter(reqs *prometheus.CounterVec, dur *prometheus.HistogramVec) chi.Router {
	r := chi.NewRouter()
	r.Use(httpMetricsWith(reqs, dur))

	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }

	r.Get("/healthz", ok)
	r.Get("/ready", ok)
	r.Get("/metrics", ok)
	r.Get("/api/v1/issues/{issueID}", ok)
	r.Post("/api/v1/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	})
	r.Route("/mcp", func(r chi.Router) {
		r.Mount("/", http.HandlerFunc(ok))
	})
	return r
}

func do(t *testing.T, r chi.Router, method, target string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Code
}

// TestHTTPMetrics_PathLabelIsRoutePattern is the cardinality guard this
// middleware exists to satisfy: ids in the URL must NOT mint series. Every
// distinct issue id has to collapse onto the single template label
// "/api/v1/issues/{issueID}" — one series, not one per id.
func TestHTTPMetrics_PathLabelIsRoutePattern(t *testing.T) {
	reqs, dur := newTestCollectors()
	r := newTestRouter(reqs, dur)

	ids := []string{"sk-1", "sk-2", "sk-255.9", "demo-42.1.7", "01900000-0000-7000-8000-0000000000a1"}
	for _, id := range ids {
		if got := do(t, r, http.MethodGet, "/api/v1/issues/"+id); got != http.StatusOK {
			t.Fatalf("GET /api/v1/issues/%s = %d, want %d", id, got, http.StatusOK)
		}
	}

	if got, want := testutil.CollectAndCount(reqs), 1; got != want {
		t.Errorf("http_requests_total series after %d distinct ids = %d, want %d (path label must be the route pattern, not the URL)",
			len(ids), got, want)
	}
	if got, want := testutil.CollectAndCount(dur), 1; got != want {
		t.Errorf("http_request_duration_seconds series after %d distinct ids = %d, want %d",
			len(ids), got, want)
	}

	counter := reqs.WithLabelValues(http.MethodGet, "/api/v1/issues/{issueID}", "200")
	if got, want := testutil.ToFloat64(counter), float64(len(ids)); got != want {
		t.Errorf("http_requests_total{method=GET,path=/api/v1/issues/{issueID},status=200} = %v, want %v", got, want)
	}
}

// TestHTTPMetrics_ExcludesProbes pins the /healthz, /ready and /metrics
// exclusion: Kubernetes probes run on a fixed cadence and would otherwise
// dominate both RPS and the latency quantiles.
func TestHTTPMetrics_ExcludesProbes(t *testing.T) {
	reqs, dur := newTestCollectors()
	r := newTestRouter(reqs, dur)

	for _, path := range []string{"/healthz", "/ready", "/metrics"} {
		for range 3 {
			if got := do(t, r, http.MethodGet, path); got != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d", path, got, http.StatusOK)
			}
		}
	}

	if got := testutil.CollectAndCount(reqs); got != 0 {
		t.Errorf("http_requests_total series after probe traffic only = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(dur); got != 0 {
		t.Errorf("http_request_duration_seconds series after probe traffic only = %d, want 0", got)
	}
}

func TestHTTPMetrics_Labels(t *testing.T) {
	tests := []struct {
		name           string
		method, target string
		wantStatus     int
		wantMethod     string
		wantPath       string
	}{
		{
			name: "business error keeps its status", method: http.MethodPost, target: "/api/v1/issues",
			wantStatus: http.StatusUnprocessableEntity, wantMethod: http.MethodPost, wantPath: "/api/v1/issues",
		},
		{
			// The handler writes a body without WriteHeader; net/http put
			// 200 on the wire, so 200 is what must be recorded (chi's
			// WrapResponseWriter reports 0 in that case).
			name: "implicit 200", method: http.MethodGet, target: "/api/v1/issues/sk-1",
			wantStatus: http.StatusOK, wantMethod: http.MethodGet, wantPath: "/api/v1/issues/{issueID}",
		},
		{
			name: "unrouted path is not a series per URL", method: http.MethodGet, target: "/does/not/exist",
			wantStatus: http.StatusNotFound, wantMethod: http.MethodGet, wantPath: pathUnmatched,
		},
		{
			name: "method not allowed", method: http.MethodDelete, target: "/api/v1/issues",
			wantStatus: http.StatusMethodNotAllowed, wantMethod: http.MethodDelete, wantPath: pathUnmatched,
		},
		{
			// All 67 MCP tools arrive on one route; the per-tool split lives
			// in skein_mcp_tool_calls_total, not here.
			name: "mcp subtree collapses to one pattern", method: http.MethodPost, target: "/mcp",
			wantStatus: http.StatusOK, wantMethod: http.MethodPost, wantPath: "/mcp/*",
		},
		{
			name: "unknown method folds to other", method: "FOOBAR", target: "/api/v1/issues/sk-1",
			wantStatus: http.StatusMethodNotAllowed, wantMethod: methodOther, wantPath: pathUnmatched,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs, dur := newTestCollectors()
			r := newTestRouter(reqs, dur)

			if got := do(t, r, tt.method, tt.target); got != tt.wantStatus {
				t.Fatalf("%s %s = %d, want %d", tt.method, tt.target, got, tt.wantStatus)
			}

			counter := reqs.WithLabelValues(tt.wantMethod, tt.wantPath, strconv.Itoa(tt.wantStatus))
			if got := testutil.ToFloat64(counter); got != 1 {
				t.Errorf("http_requests_total{method=%q,path=%q,status=%d} = %v, want 1",
					tt.wantMethod, tt.wantPath, tt.wantStatus, got)
			}
			// Exactly one series per collector: a stray extra means the
			// labels above are not the ones actually emitted.
			if got := testutil.CollectAndCount(reqs); got != 1 {
				t.Errorf("http_requests_total series = %d, want 1", got)
			}
			if got := testutil.CollectAndCount(dur); got != 1 {
				t.Errorf("http_request_duration_seconds series = %d, want 1", got)
			}
		})
	}
}

// TestHTTPMetrics_ObservesPanicAsServerError pins the middleware ordering
// contract: installed OUTSIDE chi's Recoverer, a panicking handler is
// recorded as the 500 the client received.
func TestHTTPMetrics_ObservesPanicAsServerError(t *testing.T) {
	reqs, dur := newTestCollectors()

	r := chi.NewRouter()
	r.Use(httpMetricsWith(reqs, dur))
	r.Use(recoverer500)
	r.Get("/api/v1/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })

	if got := do(t, r, http.MethodGet, "/api/v1/boom"); got != http.StatusInternalServerError {
		t.Fatalf("GET /api/v1/boom = %d, want %d", got, http.StatusInternalServerError)
	}

	counter := reqs.WithLabelValues(http.MethodGet, "/api/v1/boom", "500")
	if got := testutil.ToFloat64(counter); got != 1 {
		t.Errorf("http_requests_total{...,status=500} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(dur); got != 1 {
		t.Errorf("http_request_duration_seconds series = %d, want 1", got)
	}
}

// TestHTTPMetrics_PanicAfterWriteKeepsWireStatus pins the honest half of
// the panic story, against the REAL chi Recoverer rather than the stub
// above: once the handler has written, the response header is already on
// the wire and Recoverer's WriteHeader(500) is a no-op, so what the metric
// records is 200 — the status the client got. This is why the annotation
// on http_requests_total calls status="500" a lower bound on panics and
// not a panic count; a test is here so the annotation cannot quietly go
// back to promising an exact count.
func TestHTTPMetrics_PanicAfterWriteKeepsWireStatus(t *testing.T) {
	reqs, dur := newTestCollectors()

	r := chi.NewRouter()
	r.Use(httpMetricsWith(reqs, dur))
	r.Use(middleware.Recoverer)
	r.Get("/api/v1/stream", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"partial":true}`))
		panic("boom after write")
	})

	if got := do(t, r, http.MethodGet, "/api/v1/stream"); got != http.StatusOK {
		t.Fatalf("GET /api/v1/stream = %d, want %d (Recoverer cannot re-write a sent header)", got, http.StatusOK)
	}

	if got := testutil.ToFloat64(reqs.WithLabelValues(http.MethodGet, "/api/v1/stream", "200")); got != 1 {
		t.Errorf("http_requests_total{...,status=200} = %v, want 1 — a panic after the header is out is recorded as the status on the wire", got)
	}
	if got := testutil.ToFloat64(reqs.WithLabelValues(http.MethodGet, "/api/v1/stream", "500")); got != 0 {
		t.Errorf("http_requests_total{...,status=500} = %v, want 0 — recording 500 here would mean the metric saw a status the client never got", got)
	}
	if got := testutil.CollectAndCount(dur); got != 1 {
		t.Errorf("http_request_duration_seconds series = %d, want 1", got)
	}
}

// TestHTTPMetrics_PanicOnUpgradeKeepsWireStatus pins the second gap, and
// the one that matters for skein: chi's Recoverer skips WriteHeader
// entirely when the request carries Connection: Upgrade
// (chi v5.2.5 middleware/recoverer.go), so a panic on a streaming request
// is recorded as 200. GET /mcp/* is exactly such a request.
func TestHTTPMetrics_PanicOnUpgradeKeepsWireStatus(t *testing.T) {
	reqs, dur := newTestCollectors()

	r := chi.NewRouter()
	r.Use(httpMetricsWith(reqs, dur))
	r.Use(middleware.Recoverer)
	r.Get("/mcp/*", func(http.ResponseWriter, *http.Request) { panic("boom on upgrade") })

	req := httptest.NewRequest(http.MethodGet, "/mcp/stream", nil)
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := testutil.ToFloat64(reqs.WithLabelValues(http.MethodGet, "/mcp/*", "200")); got != 1 {
		t.Errorf("http_requests_total{path=/mcp/*,status=200} = %v, want 1 — Recoverer writes no status for an upgrade panic", got)
	}
	if got := testutil.ToFloat64(reqs.WithLabelValues(http.MethodGet, "/mcp/*", "500")); got != 0 {
		t.Errorf("http_requests_total{path=/mcp/*,status=500} = %v, want 0", got)
	}
	if got := testutil.CollectAndCount(dur); got != 1 {
		t.Errorf("http_request_duration_seconds series = %d, want 1", got)
	}
}

// TestHTTPMetrics_AbortHandlerPanicIsStillObserved is the reason the
// observation lives in a defer. http.ErrAbortHandler is the one panic
// chi's Recoverer re-raises on purpose, so it unwinds THROUGH this
// middleware; written straight after next.ServeHTTP the request would
// disappear from both metrics.
func TestHTTPMetrics_AbortHandlerPanicIsStillObserved(t *testing.T) {
	reqs, dur := newTestCollectors()

	r := chi.NewRouter()
	r.Use(httpMetricsWith(reqs, dur))
	r.Use(middleware.Recoverer)
	r.Get("/api/v1/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Error("http.ErrAbortHandler did not unwind past the middleware — chi's Recoverer is documented to re-raise it")
			}
		}()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/abort", nil))
	}()

	if got := testutil.ToFloat64(reqs.WithLabelValues(http.MethodGet, "/api/v1/abort", "200")); got != 1 {
		t.Errorf("http_requests_total after an aborted request = %v, want 1 — the observation must survive an unwinding panic", got)
	}
	if got := testutil.CollectAndCount(dur); got != 1 {
		t.Errorf("http_request_duration_seconds series = %d, want 1 — latency must be observed on the unwinding path too", got)
	}
}

// recoverer500 stands in for chi's middleware.Recoverer — same contract
// (recover, write 500), without the stack dump in the test log.
func recoverer500(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// TestHTTPMetrics_UsesRegisteredCollectors checks the exported entry point is
// wired to the package-level collectors that promauto registered — the
// injected-collector tests above would all pass with HTTPMetrics unwired.
func TestHTTPMetrics_UsesRegisteredCollectors(t *testing.T) {
	// The package-level collectors are registered in the default registry
	// and shared by the whole process. This test therefore reads a DELTA on
	// one child series and never calls Reset(): resetting a registered
	// collector zeroes it for every other test (and, in a long-lived
	// process, for the scrape), so the first parallel test that ever routes
	// a request through HTTPMetrics would start failing at random.
	//
	// The route pattern is test-private AND unique per run, so the series
	// cannot collide with another test's traffic and `go test -count=N`
	// stays honest instead of measuring the previous iteration's leftovers.
	pattern := fmt.Sprintf("/__handler_test/registered/%d/{id}", time.Now().UnixNano())
	counter := httpRequestsTotal.WithLabelValues(http.MethodGet, pattern, "200")
	before := testutil.ToFloat64(counter)
	durSeriesBefore := testutil.CollectAndCount(httpRequestDuration)

	r := chi.NewRouter()
	r.Use(HTTPMetrics)
	r.Get(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	target := strings.Replace(pattern, "{id}", "sk-1", 1)
	if got := do(t, r, http.MethodGet, target); got != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", target, got, http.StatusOK)
	}

	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("http_requests_total{path=%q,status=200} delta via HTTPMetrics = %v, want 1 — the exported middleware is not wired to the registered collectors", pattern, got)
	}
	// The histogram child for a pattern nothing else serves is created
	// lazily, so exactly one new series must have appeared on the
	// registered vector. Delta again, for the same reason.
	if got := testutil.CollectAndCount(httpRequestDuration) - durSeriesBefore; got != 1 {
		t.Errorf("new http_request_duration_seconds series via HTTPMetrics = %d, want 1", got)
	}
}

package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the HTTP surface (sk-255.9). Until now skein-api
// exported Go-runtime, promhttp_* and business counters only — there was no
// RPS, no latency and no status-code breakdown for the Go backend at all, so
// the `Skein / General` dashboard could plot the HTTP row for skein-web and
// nothing for the API. Names and label sets follow the
// common Prometheus metric conventions, so a dashboard written against
// any other service in the org transplants without edits.
//
// Both collectors are package-level singletons registered with promauto in
// the default registerer — the /metrics endpoint mounted in cmd/api/main.go
// picks them up automatically.
//
// The one decision that matters here is the `path` label. It is the chi
// ROUTE PATTERN (/api/v1/issues/{issueID}), never the request URL: skein
// puts issue ids, project ids and component UUIDs in the path, so labelling
// by r.URL.Path would mint a fresh time series per issue ever fetched and
// take the whole scrape target down with it. See routePattern below.
var (
	// httpRequestsTotal counts one increment per served HTTP request.
	//
	// @metric description Total HTTP requests served by skein-api, one increment per completed request, labelled by method, chi route pattern and response status. This is the RPS and error-rate source for the Go backend: rate() over the whole vector is throughput, the same rate split by status is the 4xx/5xx ratio, and the path label localises a spike to one endpoint family. Counts requests, not work items — a bulk endpoint that updated twenty issues increments by one. Kubernetes probes and the scrape endpoint itself (/healthz, /ready, /metrics) are excluded, so the numbers are caller traffic and not probe traffic.
	// @metric calculation Incremented in handler.HTTPMetrics, the router-wide middleware installed in buildRouter (cmd/api/main.go) ahead of chi's Recoverer, from a defer that runs after the wrapped handler returns OR unwinds. Probe and scrape paths are matched on r.URL.Path against the closed probePaths set and return before any observation. status is read from chi's WrapResponseWriter after the handler ran; a handler that wrote a body without an explicit WriteHeader reports 0 and is recorded as 200, matching what net/http actually put on the wire. A panic is recorded as whatever status reached the wire, NOT as a dedicated value: chi's Recoverer sits below this middleware, so a handler that panicked before writing anything is recorded as the 500 Recoverer wrote, but a handler that panicked after its response header was already out keeps that earlier status (Recoverer's WriteHeader is a no-op by then) and a panic on a Connection: Upgrade request is recorded as 200 because chi's Recoverer writes no status for upgrades at all. status="500" is therefore a lower bound on panics and not a panic count — alert on it as an error-rate signal, and read the stack dumps Recoverer prints for the exact set.
	// @label method HTTP request method, filtered through the closed set of RFC 9110 methods (GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT, TRACE). Any other method token an arbitrary client can put on the wire collapses to "other" so the label cannot be inflated from outside.
	// @label path chi route PATTERN, not the request URL — "/api/v1/issues/{issueID}", never "/api/v1/issues/sk-255.9". Bounded by the routing tree. A request that matched no route (404) or no method for its route (405) is recorded as "unmatched". The whole MCP surface is one value, "/mcp/*": all 67 tools arrive as POST bodies on a single route, and the per-tool split already exists in skein_mcp_tool_calls_total — see internal/mcp/tools/metrics.go.
	// @label status HTTP response status code as a decimal string, for example "200", "422", "500".
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests by method, route pattern and status",
		},
		[]string{"method", "path", "status"},
	)

	// httpRequestDuration observes server-side latency per request.
	//
	// @metric description Server-side HTTP request duration in SECONDS, observed once per completed request and labelled by method and chi route pattern. Measures the middleware-inward span — from just after the probe filter to the moment the wrapped handler returns — so it includes handler work, database round-trips and response encoding, and excludes TLS, ingress and client network time. Use it for the latency row of the API dashboard (histogram_quantile over the _bucket series) and for slow-endpoint hunting via the path label. prometheus.DefBuckets is deliberate: the same bucket layout every other service in the org uses, so a single dashboard expression works across them. Note that GET /mcp/* is the MCP listening stream and is long-lived by design — its observations land in the top bucket and say nothing about handler health. It is its own label combination and does not contaminate the REST quantiles.
	// @metric calculation Observed in handler.HTTPMetrics, the router-wide middleware installed in buildRouter (cmd/api/main.go), as time.Since(start) taken in the same defer that increments http_requests_total; the same call site, the same exclusions and the same panic behaviour, so a request that panicked contributes a duration sample too. Not labelled by status on purpose: status would multiply the bucket count of an already bucketed metric for a split that latency questions do not ask.
	// @label method HTTP request method, filtered through the same closed set as http_requests_total; anything else collapses to "other".
	// @label path chi route PATTERN, identical semantics to the http_requests_total label of the same name — "unmatched" for a request that matched no route.
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds by method and route pattern",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	// notificationCountsRequestsTotal counts header-count requests by how
	// the counts gate served them (sk-622.21 T024, US-16).
	//
	// @metric description Requests to GET /api/v1/me/notification-counts that reached the counts gate, one increment per request, split by whether the request opened a batch whose single SQL computation it waited for ("computed") or joined a batch of the same user that was still collecting ("joined"). The ratio joined / (joined + computed) is how many header re-reads the gate saved on this replica — the tabs of one person re-reading together after one personal signal share one computation. Per replica: batches never span replicas. computed equals the number of count statements the endpoint ran.
	// @metric calculation Incremented in MeHandler.NotificationCounts after countsGate.Do returns, whether the computation succeeded or failed and whether the waiting request was cancelled; the outcome label is Do's computed flag. Unauthenticated requests are rejected before the gate and are not counted.
	// @label outcome How the request was served. Values: "computed" (it opened the batch; the batch ran one computation), "joined" (it joined a batch of the same user still collecting and received that batch's result).
	notificationCountsRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_notification_counts_requests_total",
			Help: "Header-count requests by counts-gate outcome (computed, joined)",
		},
		[]string{"outcome"},
	)
)

// pathUnmatched is the `path` label for a request chi could not route (404)
// or could route but not for that method (405). Those requests have an empty
// RoutePattern, and an empty label value is indistinguishable from a bug in
// the middleware; a named bucket says which it is.
const pathUnmatched = "unmatched"

// methodOther is the `method` label for any request method outside the
// RFC 9110 set. net/http accepts an arbitrary token as a method, so without
// this fold a client could mint one label value per request.
const methodOther = "other"

// probePaths are excluded from both metrics: Kubernetes liveness/readiness
// probes and the Prometheus scrape itself. They run on a fixed cadence
// independent of user traffic, so counting them would make RPS a function of
// the probe interval and pull the latency quantiles toward a trivial
// handler. Matched on r.URL.Path (before routing) because that is the only
// thing available when we decide to skip.
var probePaths = map[string]struct{}{
	"/healthz": {},
	"/ready":   {},
	"/metrics": {},
}

// knownMethods is the closed allowlist the `method` label is filtered
// through — the RFC 9110 method set. Everything else becomes methodOther.
var knownMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodPatch:   {},
	http.MethodDelete:  {},
	http.MethodConnect: {},
	http.MethodOptions: {},
	http.MethodTrace:   {},
}

// HTTPMetrics is the router-wide middleware that feeds http_requests_total
// and http_request_duration_seconds.
//
// Install it BEFORE chi's Recoverer (i.e. outside it), so a panicking
// handler is recovered below this middleware and observed with the status
// the client actually received. Installed after Recoverer it would sit
// inside the recovery boundary: the panic would unwind through the
// observation before Recoverer wrote anything, and the failure most worth
// alerting on would be recorded as a 200. TestBuildRouter_* in cmd/api
// pins the order on the real router.
//
// What this middleware does NOT do is count panics. From outside the
// recovery boundary a recovered panic is invisible as such — only its
// effect on the response writer is visible — so a dedicated
// status="panic" would mean moving this middleware inside Recoverer and
// recovering here, and it would also put a non-numeric value in a label
// every org dashboard reads as an HTTP code (`status=~"5.."` would then
// silently exclude exactly the class it was written for). Both prices are
// higher than the accuracy bought, so `status` stays a numeric code and
// the annotation above says plainly where 500 undercounts panics:
// a panic after the header is out keeps the status already on the wire,
// and chi's Recoverer writes no status at all when the request carries
// Connection: Upgrade (chi v5.2.5 middleware/recoverer.go). Both cases are
// pinned by tests in metrics_test.go.
//
// The observation is deferred rather than written straight after
// next.ServeHTTP so a panic that unwinds PAST this middleware — the
// http.ErrAbortHandler that Recoverer deliberately re-raises — still
// leaves the request counted instead of vanishing from both metrics.
func HTTPMetrics(next http.Handler) http.Handler {
	return httpMetricsWith(httpRequestsTotal, httpRequestDuration)(next)
}

// httpMetricsWith is HTTPMetrics with the collectors injected. It exists so
// tests can assert on freshly-built, unregistered collectors instead of
// mutating the process-wide default registry (and racing every other test in
// the package that happens to serve a request).
func httpMetricsWith(
	reqs *prometheus.CounterVec,
	dur *prometheus.HistogramVec,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, skip := probePaths[r.URL.Path]; skip {
				next.ServeHTTP(w, r)
				return
			}

			// WrapResponseWriter is what makes the status observable at
			// all; it preserves Flusher/Hijacker/ReaderFrom and exposes
			// Unwrap, so the MCP streaming transport keeps working through
			// it.
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			// Deferred, not written after next.ServeHTTP returns: a panic
			// that escapes the recovery boundary below (Recoverer re-raises
			// http.ErrAbortHandler by design) would otherwise unwind past
			// both writes and the request would appear in neither metric.
			// The defer costs nothing measurable next to a served request
			// and removes the whole "does this path unwind?" question.
			defer func() {
				// Read the pattern AFTER the handler ran: chi fills the
				// route context during dispatch, so before next.ServeHTTP
				// it is still empty. Same *chi.Context pointer either way —
				// the mux put it in the request context before the
				// middleware chain started and mutates it in place.
				method, path := labelMethod(r.Method), routePattern(r)

				// Status() is 0 when the handler wrote a body without
				// calling WriteHeader, and also when it panicked before
				// writing anything on an upgrade request (chi's Recoverer
				// skips WriteHeader for Connection: Upgrade). net/http
				// emitted 200 in the first case, so 200 is what the client
				// saw; in the second nothing usable reached the wire and
				// 200 is the honest floor. See the HTTPMetrics doc comment.
				status := ww.Status()
				if status == 0 {
					status = http.StatusOK
				}

				reqs.WithLabelValues(method, path, strconv.Itoa(status)).Inc()
				dur.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
			}()

			next.ServeHTTP(ww, r)
		})
	}
}

// routePattern returns the chi route pattern for a served request, e.g.
// "/api/v1/issues/{issueID}" — the whole point of the `path` label. Falls
// back to pathUnmatched when chi routed nothing (404/405) or when the
// middleware somehow ran outside a chi mux.
func routePattern(r *http.Request) string {
	if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
		return pattern
	}
	return pathUnmatched
}

// labelMethod folds an arbitrary request method token into the closed
// RFC 9110 set, so the `method` label stays bounded no matter what a client
// puts on the wire.
func labelMethod(method string) string {
	if _, known := knownMethods[method]; known {
		return method
	}
	return methodOther
}

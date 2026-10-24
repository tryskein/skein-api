package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/config"
	"skein-api/internal/search"
)

// newRouterUnderTest builds the PRODUCTION router. Nearly every dependency is
// nil: buildRouter only wires routes and middleware, and a route bound to a
// method value of a nil handler pointer binds fine — no request in this file
// reaches one. The exceptions are the three arguments buildRouter actually
// uses while assembling: cfg (the internal secret, bucket names, TTLs),
// searchSvc (Embedder() decides whether the MCP tools get a reranker) and the
// pool the auth middlewares refuse to construct without.
//
// The point of going through buildRouter instead of hand-rolling a router in
// the test is that the thing under test IS the wiring: which middleware are
// installed and in what order.
func newRouterUnderTest(t *testing.T) chi.Router {
	t.Helper()
	return newRouterWithOAuthServers(t, []string{"https://iden.test/realms/skein"})
}

// newRouterWithOAuthServers is newRouterUnderTest with the sk-304 discovery
// switch under the caller's control. Pass nil to build the router of a
// deployment that has never configured SKEIN_OAUTH_AUTHORIZATION_SERVERS —
// which is every deployment whose ConfigMap predates the variable, the
// production one included (review F1).
func newRouterWithOAuthServers(t *testing.T, oauthServers []string) chi.Router {
	t.Helper()

	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}

	// A pool value is required (auth.RequireAPIKey and the monitor-token
	// door refuse to construct on a nil pool — fail-fast wiring guards).
	// pgxpool connects lazily, so this never touches a database: no request
	// in this file reaches a handler that would acquire a connection.
	pool, err := pgxpool.New(context.Background(),
		"postgres://router-test:router-test@127.0.0.1:1/router-test?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	r, _ := buildRouter(
		pool, // pool
		// InternalSecret must be non-empty: RequireInternalSecret panics on
		// an empty secret at construction time (fail-fast wiring guard).
		// AgentPublicURL + OAuthAuthorizationServers must be valid too:
		// buildRouter builds the RFC 9728 metadata document from them at
		// wiring time and panics on invalid input (sk-304), the same
		// fail-fast contract as the internal secret above.
		&config.Config{
			InternalSecret:            "router-test-internal-secret",
			AgentPublicURL:            "https://skein-mcp.test",
			OAuthAuthorizationServers: oauthServers,
			InstanceName:              routerTestInstance,
		}, // cfg
		nil,       // authH
		nil,       // projectsH
		nil,       // componentsH
		nil,       // issuesH
		nil,       // depsH
		nil,       // readyH
		nil,       // searchH
		nil,       // notifH
		nil,       // bulkH
		nil,       // apiKeysH
		nil,       // usersH
		nil,       // issuesMeH
		nil,       // issuesAllH
		nil,       // activityH
		nil,       // dashboardH
		nil,       // usersStore
		nil,       // issuesStore
		nil,       // depsStore
		nil,       // projectsStore
		nil,       // notifStore
		nil,       // remindersStore
		searchSvc, // searchSvc
		nil,       // s3Client
		nil,       // attachmentsH
		nil,       // meH
		nil,       // themesH
		nil,       // meThemeH
		nil,       // themesStore
		nil,       // themesPublisher
		nil,       // inspectCache
		nil,       // remindersH
		nil,       // pushH
	)
	return r
}

// routerTestInstance is the SKEIN_INSTANCE_NAME of the router under test.
// /metrics labels every series with it (instance_label.go), so the exact
// series these tests read carry it too.
const routerTestInstance = "router-test-instance"

// scrapeCounter serves GET /metrics through the router itself and returns the
// value of one exact series, or 0 when the series is not exported yet.
//
// Reading the exposition rather than the collector is deliberate: the
// collectors are unexported in internal/handler, and what this test wants to
// prove is precisely that the number reaches /metrics of the real router —
// registered, exported, scrapeable.
func scrapeCounter(t *testing.T, r chi.Router, series string) float64 {
	t.Helper()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", rec.Code, http.StatusOK)
	}

	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		value, found := strings.CutPrefix(line, series+" ")
		if !found {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("unparseable value for %s: %q (%v)", series, value, err)
		}
		return v
	}
	return 0
}

// TestBuildRouter_PanicIsObservedAsServerError is the guard for the one
// property of buildRouter that no unit test can see from inside
// internal/handler: that HTTPMetrics is installed at all, and that it is
// installed OUTSIDE chi's Recoverer.
//
// Swapping the two r.Use lines (or dropping the HTTPMetrics one) used to be
// invisible — the handler-package tests build their own routers and would
// stay green. Here the request goes through the production chain and the
// number is read back off the production /metrics endpoint:
//
//   - HTTPMetrics removed → the series never appears, delta 0;
//   - HTTPMetrics moved below Recoverer → the panic unwinds through the
//     middleware before Recoverer writes anything, so the request is recorded
//     as 200 and the 500 delta is 0.
//
// A panicking route is the probe because a panic is the only request whose
// recorded status depends on the ordering.
func TestBuildRouter_PanicIsObservedAsServerError(t *testing.T) {
	const probePath = "/__router_order_probe"
	const series = `http_requests_total{method="GET",path="` + probePath + `",skein_instance="` + routerTestInstance + `",status="500"}`

	r := newRouterUnderTest(t)
	// Root-level route: no auth middleware (those live on the /api/v1, /mcp
	// and /agent/v1 groups), so the request reaches the handler and the only
	// middleware it crosses is the router-wide chain under test.
	r.Get(probePath, func(http.ResponseWriter, *http.Request) {
		panic("router-order probe: this stack dump is expected")
	})

	before := scrapeCounter(t, r, series)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, probePath, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET %s = %d, want %d — chi's Recoverer is not in the production chain",
			probePath, rec.Code, http.StatusInternalServerError)
	}

	if got := scrapeCounter(t, r, series) - before; got != 1 {
		t.Errorf("%s delta = %v, want 1 — handler.HTTPMetrics must be registered in buildRouter BEFORE middleware.Recoverer, so a panicking handler is counted as the 500 the client received",
			series, got)
	}
}

// TestBuildRouter_ProbesAreNotCounted pins the other half of the wiring at
// the same level: the exclusion list is only useful if the middleware that
// owns it is actually in front of the probe routes. /healthz is served by the
// production router here, so a future middleware ordering that put the
// counter ahead of the exclusion would show up as a series.
func TestBuildRouter_ProbesAreNotCounted(t *testing.T) {
	const series = `http_requests_total{method="GET",path="/healthz",skein_instance="` + routerTestInstance + `",status="200"}`

	r := newRouterUnderTest(t)
	before := scrapeCounter(t, r, series)

	for range 3 {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
		}
	}

	if got := scrapeCounter(t, r, series) - before; got != 0 {
		t.Errorf("%s delta = %v, want 0 — Kubernetes probe traffic must stay out of RPS and the latency quantiles", series, got)
	}
}

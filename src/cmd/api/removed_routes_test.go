package main

// sk-303.13 / T017: the routes removed with the Outbox surface stay
// unrouted on the production router, and their neighbours stay routed.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// registeredRoutes walks the production router and returns the set of
// "METHOD path" it actually serves.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	r := newRouterUnderTest(t)
	out := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi reports the trailing-slash form for sub-router roots
		// ("/api/v1/themes/"); annotations are written without it.
		route = strings.TrimSuffix(route, "/")
		if route == "" {
			route = "/"
		}
		out[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
	return out
}

// TestRemovedOutboxRoutesAreUnrouted (sk-303.13 / T017, contract C2): the
// per-issue notification tail is gone from the PRODUCTION router.
//
// "The handler is deleted" and "the path is unrouted" are different claims,
// and only the second one is what a client meets. Asserted here rather than
// in the handler package because this is the router the process actually
// serves, assembled by the same buildRouter call.
func TestRemovedOutboxRoutesAreUnrouted(t *testing.T) {
	routes := registeredRoutes(t)

	gone := []string{
		// The per-issue notification tail existed only for the Outbox
		// card's read history and went with it.
		"GET /api/v1/issues/{issueID}/notifications",
	}
	for _, key := range gone {
		if routes[key] {
			t.Errorf("route %q is still registered — the Outbox surface it served is removed", key)
		}
	}

	// Neighbours on the same sub-router must be untouched: the removal is one
	// route, not the group.
	for _, key := range []string{
		"POST /api/v1/issues/{issueID}/watch",
		"DELETE /api/v1/issues/{issueID}/watch",
		"GET /api/v1/issues/{issueID}/watchers",
	} {
		if !routes[key] {
			t.Errorf("route %q disappeared with the Outbox removal — it is unrelated to it", key)
		}
	}
}

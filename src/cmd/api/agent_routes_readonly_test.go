package main

// agent_routes_readonly_test.go — sk-579 matrix row 52 (INV-10): the
// /agent/v1 door carries no write route. This is the one code-structure check
// the owner permitted (projects/skein-api/CLAUDE.md, "## Testing",
// "checks that read code"): its subject is the router the process actually
// assembles — buildRouter through newRouterUnderTest, walked with chi.Walk —
// not the source text. It stops being needed if the spec ever decides that
// /agent/v1 gets a write route.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestAgentRoutes_ReadOnly(t *testing.T) {
	r := newRouterUnderTest(t)

	var agentRoutes []string
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/agent/v1/") && route != "/agent/v1" {
			return nil
		}
		agentRoutes = append(agentRoutes, method+" "+route)
		if method != http.MethodGet && method != http.MethodHead {
			t.Errorf("/agent/v1 has a write route: %s %s (INV-10: the monitor door is read-only)", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	// An empty walk would pass the loop above vacuously: the group must exist.
	if len(agentRoutes) == 0 {
		t.Fatalf("no route under /agent/v1 in the built router — the monitor door is missing")
	}
}

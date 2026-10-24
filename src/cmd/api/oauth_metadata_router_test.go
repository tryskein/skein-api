package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
)

// These tests go through the PRODUCTION router (newRouterUnderTest →
// buildRouter), because sk-304 is a wiring defect: the document and the
// challenge are only worth anything if they are reachable on the exact paths
// a client probes, without credentials, and if the 401 that starts the whole
// discovery dance actually carries the pointer.
//
// They need no database. The metadata routes are static, and the 401 under
// test is produced by the api-key gate's cheap shape filter, which rejects a
// missing/malformed key before any lookup. That is deliberate: the guard for
// the public discovery surface must run in every suite, not only where a
// Postgres is up.

const (
	// Must match newRouterUnderTest's config (router_metrics_test.go).
	routerPublicURL   = "https://skein-mcp.test"
	routerInternalSec = "router-test-internal-secret"
)

// TestWellKnown_BothPathsAreServedWithoutAuth — acceptance criterion 1: the
// path-aware URI answers, not only the root one. Before this change the
// path-aware URI 404'd (the cluster gateway matched the root path Exact-ly and
// the suffixed request fell through to a service that knew nothing about it).
func TestWellKnown_BothPathsAreServedWithoutAuth(t *testing.T) {
	r := newRouterUnderTest(t)

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// No X-Skein-Api-Key, no X-Skein-Internal-Secret: a client reads
			// this document precisely because it cannot yet authenticate.
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 (body %q)", path, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("GET %s Content-Type = %q, want application/json", path, ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("GET %s Cache-Control = %q, want no-store", path, cc)
			}

			var doc struct {
				Resource             string   `json:"resource"`
				AuthorizationServers []string `json:"authorization_servers"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("GET %s body is not JSON: %v (%s)", path, err, rec.Body.String())
			}
			if want := routerPublicURL + "/mcp"; doc.Resource != want {
				t.Fatalf("GET %s resource = %q, want %q", path, doc.Resource, want)
			}
			if len(doc.AuthorizationServers) == 0 {
				t.Fatalf("GET %s authorization_servers is empty — the MCP authorization "+
					"spec requires at least one", path)
			}
		})
	}
}

// TestMCP401_CarriesResourceMetadataChallenge — acceptance criterion 3. The
// MCP authorization spec makes WWW-Authenticate on a 401 the client's primary
// discovery route; it must name the PATH-AWARE document, exactly, quoted
// (RFC 9728 §5.1).
func TestMCP401_CarriesResourceMetadataChallenge(t *testing.T) {
	r := newRouterUnderTest(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	// Channel secret present (the BFF always adds it); api key absent — this
	// is the unauthenticated probe an MCP client opens with.
	req.Header.Set(auth.HeaderInternalSecret, routerInternalSec)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /mcp without api key = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	const want = `Bearer resource_metadata="https://skein-mcp.test/.well-known/oauth-protected-resource/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// TestMCP401_ChallengeURLIsServed closes the loop the client walks: follow the
// URL out of the challenge and the document must be there, naming the very
// resource the client asked about. A challenge pointing at a 404 would leave
// the client exactly where sk-304 left it.
func TestMCP401_ChallengeURLIsServed(t *testing.T) {
	r := newRouterUnderTest(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set(auth.HeaderInternalSecret, routerInternalSec)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	challenge := rec.Header().Get("WWW-Authenticate")
	metaURL := strings.TrimSuffix(strings.TrimPrefix(challenge, `Bearer resource_metadata="`), `"`)
	if metaURL == challenge || metaURL == "" {
		t.Fatalf("unparseable challenge %q", challenge)
	}
	path := strings.TrimPrefix(metaURL, routerPublicURL)
	if path == metaURL {
		t.Fatalf("challenge %q does not point at the configured public URL %q", metaURL, routerPublicURL)
	}

	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, path, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET %s (from challenge) = %d, want 200", path, rec2.Code)
	}

	var doc struct {
		Resource string `json:"resource"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &doc); err != nil {
		t.Fatalf("challenge document is not JSON: %v", err)
	}
	// The exact-match rule the client applies (RFC 9728 §6): the document must
	// name the URL the client was trying to reach, character for character.
	if want := routerPublicURL + "/mcp"; doc.Resource != want {
		t.Fatalf("challenge document resource = %q, want %q", doc.Resource, want)
	}
}

// TestWellKnown_NotShadowedByAuthGates guards the ordering the fix depends on:
// the metadata routes sit at the router root, ABOVE the /mcp group, so neither
// the channel-secret gate nor the api-key gate can turn a discovery probe into
// a 401. A future refactor that mounts them inside an authenticated group
// would reintroduce the bug in a form that looks like a config problem.
func TestWellKnown_NotShadowedByAuthGates(t *testing.T) {
	r := newRouterUnderTest(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil)
	// A deliberately WRONG internal secret: still must not matter here.
	req.Header.Set(auth.HeaderInternalSecret, "wrong-secret")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("metadata probe with a bad internal secret = %d, want 200", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("metadata document answered with an auth challenge — it must be public")
	}
}

// Compile-time reminder that the handler constructor lives where the router
// expects it; keeps this file honest if the handler is ever moved.
var _ = handler.ProtectedResourceMetadata

// TestOAuthDiscoveryDisabled_ServiceStillServesMCP — sk-304 review F1, at the
// wiring level. With SKEIN_OAUTH_AUTHORIZATION_SERVERS unset the feature is
// OFF, and "off" has to mean exactly three things and nothing more:
//
//  1. the router still builds (the first cut panicked here, and config.Load
//     refused to return at all — a pod that would not boot);
//  2. the discovery routes are absent rather than serving a document with an
//     empty `authorization_servers`, which the MCP authorization spec makes a
//     MUST — a non-conformant document is worse than no document;
//  3. /mcp keeps working: the api-key gate still runs, still 401s an
//     unauthenticated probe, and just carries no challenge. That is the live
//     path today — a static ska_ key on X-Skein-Api-Key — and it must not
//     depend on OAuth being configured.
func TestOAuthDiscoveryDisabled_ServiceStillServesMCP(t *testing.T) {
	r := newRouterWithOAuthServers(t, nil)

	t.Run("metadata routes are not registered", func(t *testing.T) {
		for _, path := range []string{
			"/.well-known/oauth-protected-resource",
			"/.well-known/oauth-protected-resource/mcp",
		} {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s with discovery off = %d, want 404 — an empty "+
					"authorization_servers array is a non-conformant document (body %q)",
					path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("mcp door still gated, without a challenge", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set(auth.HeaderInternalSecret, routerInternalSec)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST /mcp without api key = %d, want 401 — the static-key door "+
				"must not depend on OAuth being configured", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Fatalf("WWW-Authenticate = %q with discovery off, want none: the header "+
				"would point at a document this deployment does not serve", got)
		}
	})
}

// TestMCP401_ChannelSecretGateAlsoChallenges — sk-304 review F5. /mcp has TWO
// gates and the channel-secret one runs FIRST, so it is the 401 a client meets
// whenever the internal secret is missing or wrong. The MCP authorization spec
// requires the challenge on a 401 from an MCP server unconditionally, not on
// the subset of 401s we find interesting: a client refused at the outer gate
// with a bare 401 has nothing to discover and stalls exactly as sk-304
// describes. Both doors, one behaviour.
func TestMCP401_ChannelSecretGateAlsoChallenges(t *testing.T) {
	r := newRouterUnderTest(t)

	for _, tc := range []struct {
		name   string
		secret string // "" = header absent
	}{
		{"internal secret absent", ""},
		{"internal secret wrong", "not-the-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
			if tc.secret != "" {
				req.Header.Set(auth.HeaderInternalSecret, tc.secret)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("POST /mcp = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			const want = `Bearer resource_metadata="https://skein-mcp.test/.well-known/oauth-protected-resource/mcp"`
			if got := rec.Header().Get("WWW-Authenticate"); got != want {
				t.Fatalf("WWW-Authenticate on the channel-secret 401 = %q, want %q", got, want)
			}
		})
	}
}

// TestNonMCP401_CarriesNoChallenge is the other half of F5: the challenge is
// per-MOUNT. The same RequireInternalSecret guards /api/v1 and /agent/v1, whose
// 401s are cookie/monitor-token failures with no OAuth protected resource
// behind them. Advertising the /mcp metadata document there would point a
// client at an identifier that does not describe the door it was refused at —
// and the byte-for-byte comparison would then fail, which is sk-304 again in
// mirror image.
func TestNonMCP401_CarriesNoChallenge(t *testing.T) {
	r := newRouterUnderTest(t)

	for _, path := range []string{"/api/v1/projects", "/agent/v1/notifications/poll"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusUnauthorized {
				t.Skipf("GET %s = %d, not a 401 — nothing to assert here", path, rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Fatalf("GET %s 401 carries WWW-Authenticate %q; the /mcp metadata document "+
					"does not describe this door", path, got)
			}
		})
	}
}
